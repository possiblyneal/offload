package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// checkStdin refuses --stdin when stdin is not a pipe or a file (a terminal, say):
// forwarding it would block on the keyboard rather than carry data.
func checkStdin(stdin *os.File, flag bool) error {
	if !flag {
		return nil
	}
	if stdioClosed(0) {
		return errors.New("--stdin: stdin is closed, so there is nothing to forward")
	}
	info, err := stdin.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeNamedPipe == 0 && !info.Mode().IsRegular() {
		return errors.New("--stdin: stdin is not a pipe or a file")
	}
	return nil
}

// sameFile reports whether a and b are the same open file: the same device and inode
// (both ends of one pipe share an inode; two pipes do not).
func sameFile(a, b *os.File) bool {
	var sa, sb syscall.Stat_t
	if syscall.Fstat(int(a.Fd()), &sa) != nil || syscall.Fstat(int(b.Fd()), &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// jobStdin is the job ssh's stdin: env, then follow (the caller's stdin, with --stdin)
// from the moment start runs, or nil for EOF after env. It is a pipe rather than a
// Reader because exec.Cmd.Wait waits for the copy from a Reader to end, which an idle
// caller's stdin never does, whatever WaitDelay says; a pipe leaves the copy here, where a
// blocked read holds up nothing.
func jobStdin(env []byte, follow io.Reader) (r *os.File, start func(), err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	started := make(chan struct{})
	start = func() { close(started) }
	go func() {
		defer w.Close()
		if _, err := w.Write(env); err != nil || follow == nil {
			return
		}
		<-started
		io.Copy(w, follow)
	}()
	return r, start, nil
}

// stdioPlan is how the job's stdout and stderr reach the caller's, read once from fd 1 and
// fd 2 and handed to the launcher as OFFLOAD_STDIO, which offload-wait follows. A stream
// that is a pipe, a terminal or a device is relayed over ssh; one that is a regular file
// the remote job writes itself, so a job that seeks it, reopens /dev/stdout or reads back
// what it wrote gets the file a local run would leave.
type stdioPlan struct {
	merged bool         // stderr is stdout's open file: one stream, which keeps a 2>&1 order
	files  []outputFile // the streams, at most two, that are regular files
}

// outputFile is a stream the remote job opens on dev's file itself.
type outputFile struct {
	stream     string // "out" or "err"
	fd         int    // the caller's descriptor: 1 or 2
	path       string // the file's real path on dev
	appendMode bool   // opened with O_APPEND (`>>`)
	readWrite  bool   // opened for reading too (`<>`)
	offset     int64  // where the job's first write goes, read as the job is sent (at)
}

// sysKcmp is kcmp(2) on linux/amd64, the only target; the syscall package has no name for it.
const sysKcmp = 312

// sameOpenFile reports whether fds a and b are one open file description (kcmp(2)
// KCMP_FILE), so a write through one moves the other's offset; ok is false when kcmp
// cannot tell.
func sameOpenFile(a, b int) (same, ok bool) {
	pid := uintptr(os.Getpid())
	r, _, errno := syscall.Syscall6(sysKcmp, pid, pid, 0, uintptr(a), uintptr(b), 0)
	return r == 0, errno == 0
}

// readStdio is the caller's stdio plan, or why a remote job could not write its output as
// a local run would: a regular file that is removed, read-only to the caller, not the
// one its path names now, or opened twice not both appending (`>f 2>f`, two offsets
// whose overwrites a remote cannot order as a local run's). Two appending openings
// (`>>f 2>>f`) each append, as the remote job's two do.
func readStdio() (stdioPlan, string) {
	var plan stdioPlan
	plan.merged = sameFile(os.Stdout, os.Stderr)
	if plan.merged && regularFile(1) {
		if same, ok := sameOpenFile(1, 2); !ok || !same && !(appending(1) && appending(2)) {
			return stdioPlan{}, "stdout and stderr are two openings of one file, whose writes a remote job cannot interleave as a local run's would"
		} else if !same {
			plan.merged = false
		}
	}
	for _, s := range []struct {
		stream string
		fd     int
		name   string
	}{{"out", 1, "stdout"}, {"err", 2, "stderr"}} {
		var st syscall.Stat_t
		if syscall.Fstat(s.fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			continue
		}
		if s.fd == 2 && plan.merged {
			continue // stdout's open file, which offload-wait gives stderr too
		}
		p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(s.fd))
		if err == nil && heldElsewhere(s.fd, p) {
			continue // relayed: written here, it moves the one offset every holder writes at
		}
		var at syscall.Stat_t
		if err != nil || !filepath.IsAbs(p) || syscall.Stat(p, &at) != nil || at.Dev != st.Dev || at.Ino != st.Ino {
			return stdioPlan{}, s.name + " is a file no path on dev names any more (removed or replaced), which a remote job cannot open"
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(s.fd), syscall.F_GETFL, 0)
		if errno != 0 || flags&syscall.O_ACCMODE == syscall.O_RDONLY {
			return stdioPlan{}, s.name + " is the file " + p + " opened read-only, which a job's writes fail on"
		}
		readWrite := flags&syscall.O_ACCMODE == syscall.O_RDWR
		mode, how := uint32(2), "writing" // W_OK: the remote job opens it anew, as the caller
		if readWrite {
			mode, how = 6, "reading and writing" // R_OK|W_OK
		}
		if syscall.Access(p, mode) != nil {
			return stdioPlan{}, s.name + " is the file " + p + ", which the caller may not open for " + how + ", as a remote job must"
		}
		plan.files = append(plan.files, outputFile{stream: s.stream, fd: s.fd, path: p, appendMode: flags&syscall.O_APPEND != 0, readWrite: readWrite})
	}
	return plan, ""
}

// heldElsewhere reports whether another process holds fd's open file description, whose
// path is p (`make -j CC='offload -- gcc' >log`, a `{ …; } >log` shell): a remote job would
// write from an offset of its own, over what the other holders write at the shared one, so
// such a stream is relayed instead. One it cannot rule out counts as held.
func heldElsewhere(fd int, p string) bool {
	self := os.Getpid()
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == self {
			continue
		}
		dir := "/proc/" + proc.Name() + "/fd/"
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue // gone, or another user's, which this cannot inspect
		}
		for _, f := range fds {
			other, err := strconv.Atoi(f.Name())
			if err != nil {
				continue
			}
			if link, err := os.Readlink(dir + f.Name()); err != nil || link != p {
				continue
			}
			r, _, errno := syscall.Syscall6(sysKcmp, uintptr(pid), uintptr(self), 0, uintptr(other), uintptr(fd), 0)
			if errno != 0 || r == 0 {
				return true
			}
		}
	}
	return false
}

// regularFile reports whether fd is open on a regular file.
func regularFile(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFREG
}

// appending reports whether fd was opened with O_APPEND.
func appending(fd int) bool {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	return errno == 0 && flags&syscall.O_APPEND != 0
}

// outputRefusal is why a remote job could not reach one of files as dev's: under /dev,
// /proc or /sys, which are the remote's own, on a mount the export does not carry, under a private directory, under a remote-local path, or bound
// read-only in the job; or "".
func outputRefusal(files []outputFile, mounts, remoteLocal, readOnly []string) string {
	for _, f := range files {
		for _, q := range []string{f.path, realPath(f.path)} {
			switch {
			case within(mounts, q) != "":
				return fmt.Sprintf("the job's output file %s is on dev's %s mount, which is not exported", f.path, within(mounts, q))
			case within(privateDirs, q) != "":
				return fmt.Sprintf("the job's output file %s is under %s, which the job sees as a private directory", f.path, within(privateDirs, q))
			case within([]string{"/dev", "/proc", "/sys"}, q) != "":
				return fmt.Sprintf("the job's output file %s is under %s, which a remote job sees as the remote's own", f.path, within([]string{"/dev", "/proc", "/sys"}, q))
			case within(remoteLocal, q) != "":
				return fmt.Sprintf("the job's output file %s is under the remote-local %s, which a remote job sees as the remote's own copy", f.path, within(remoteLocal, q))
			case within(readOnly, q) != "":
				return fmt.Sprintf("the job's output file %s is under %s, which the job sees read-only", f.path, within(readOnly, q))
			}
		}
	}
	return ""
}

// at is p with each file's offset read now, as the job is sent: what the wrapper wrote
// before (--explain) is not the job's to overwrite.
func (p stdioPlan) at() stdioPlan {
	files := slices.Clone(p.files)
	for i := range files {
		files[i].offset, _ = syscall.Seek(files[i].fd, 0, io.SeekCurrent)
	}
	p.files = files
	return p
}

// env is the plan as OFFLOAD_STDIO: one line per stream that is not a plain relay,
// `err merged` or `<stream> file <mode> <offset> <path>`, the mode `w` or `a` (appending),
// led by `r` when the caller's open reads too, the path last so it may hold spaces.
func (p stdioPlan) env() string {
	var lines []string
	if p.merged {
		lines = append(lines, "err merged")
	}
	for _, f := range p.files {
		mode := "w"
		if f.appendMode {
			mode = "a"
		}
		if f.readWrite {
			mode = "r" + mode
		}
		lines = append(lines, fmt.Sprintf("%s file %s %d %s", f.stream, mode, f.offset, f.path))
	}
	return strings.Join(lines, "\n")
}

// paths are the files the job writes itself, which it must see writable.
func (p stdioPlan) paths() []string {
	var out []string
	for _, f := range p.files {
		out = append(out, f.path)
	}
	return out
}

// settle leaves each file's offset where a local run would have: where the job left it
// (offsets, indexed by fd-1, from offload-wait, where seen), or the file's end when that is
// unknown. A job that was never sent (ran false: no `go`) leaves the caller's offset alone.
func (p stdioPlan) settle(offsets [2]int64, seen [2]bool, ran bool) {
	if !ran {
		return
	}
	for _, f := range p.files {
		i := f.fd - 1
		if seen[i] {
			syscall.Seek(f.fd, offsets[i], io.SeekStart)
		} else {
			syscall.Seek(f.fd, 0, io.SeekEnd)
		}
	}
}

// appendTo is a new open file description of fd's file, appending: what the wrapper writes
// through it lands at the file's end, past all the job wrote, and leaves fd's offset where
// the job left it.
func appendTo(fd int) (*os.File, error) {
	return os.OpenFile("/proc/self/fd/"+strconv.Itoa(fd), os.O_WRONLY|os.O_APPEND, 0)
}

// appended is each output descriptor the wrapper appends its messages to, with the file's
// size when it started.
var appended []struct {
	fd   int
	size int64
}

// trackAppended records that the wrapper appends fd's messages through a.
func trackAppended(fd int, a *os.File) {
	if st, err := a.Stat(); err == nil {
		appended = append(appended, struct {
			fd   int
			size int64
		}{fd, st.Size()})
	}
}

// followAppended moves each caller's descriptor the wrapper appended a message to past it,
// so what the caller writes next follows the message, as after a local run's; one the
// wrapper wrote nothing to stays where the job left it.
func followAppended() {
	for _, a := range appended {
		var st syscall.Stat_t
		if syscall.Fstat(a.fd, &st) == nil && st.Size > a.size {
			syscall.Seek(a.fd, 0, io.SeekEnd)
		}
	}
}

// heldOutput holds what reaches an output file over ssh while the remote job writes the
// file itself (a launcher's or ssh's message), which written at once would land where the
// job's later writes go over it. flush writes it when the job is over, and anything that
// arrives later goes straight to where flush wrote.
type heldOutput struct {
	f    *os.File
	mu   sync.Mutex
	buf  []byte
	done bool
}

func (h *heldOutput) Write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return h.f.Write(b)
	}
	h.buf = append(h.buf, b...)
	return len(b), nil
}

// flush writes what is held to to, from then on where every later write goes: the file
// appended to (appendTo) when the job may have run, else the caller's own descriptor, where
// a local run's message would go. A nil heldOutput holds nothing.
func (h *heldOutput) flush(to *os.File) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.f, h.done = to, true
	if len(h.buf) == 0 {
		return nil
	}
	_, err := h.f.Write(h.buf)
	h.buf = nil
	return err
}
