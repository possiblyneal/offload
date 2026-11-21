// Command offload-wait is the parent of an offloaded job: it runs the job, then makes sure
// the job's writes reached dev.
//
//	usage: offload-wait CWD WRITABLE PRIVATE STDIO -- COMMAND [ARGS...]
//
// NFS reports a failed write (ENOSPC, EDQUOT, EIO, a file made immutable on dev) when the
// page is written back, which for most jobs is the close at exit, whose error nobody reads.
// So this opens a descriptor on each filesystem of the WRITABLE paths (newline-separated;
// those that do not exist are skipped) before the command starts, and calls syncfs on each
// once it has ended: since Linux 5.8 syncfs reports a writeback error the filesystem met
// after that descriptor was opened. A failure is printed, naming the paths, and turns an
// exit code of 0 into 1; a job's own non-zero code is kept.
//
// PRIVATE (newline-separated) are the directories that end with the job on the remote
// (/dev/shm, /dev/mqueue). Anything left in one when the command ends would have stayed on
// dev after a local run, so it is a failure too, printed with the names, as above.
//
// The launcher runs this as the init of the job's pid namespace, so every process the job
// leaves running (`cmd &`, a daemon it started) is killed when this exits, where a local run
// would have kept it. Each is named on stderr as a warning; the exit code is unchanged, since
// a build's server or agent left behind is routine. As that init it also collects each of
// them that ends first, so a job polling with `kill -0` for one to go sees it go, as it
// would locally.
//
// The launcher opens this binary before it pivots into dev's filesystem, where the binary
// is not, and runs it through that descriptor as the job's user, as its last step. It
// enters CWD (dev's home is 0700, so only the user can) and runs COMMAND in the process
// group the launcher signals; it catches SIGINT, SIGTERM, SIGHUP, SIGQUIT, SIGUSR1 and
// SIGUSR2 so that it outlives them and reports the job's own exit, 128+n for death by
// signal n. A failure before COMMAND starts exits 125. Once COMMAND runs it writes a byte
// to descriptor startedFD when the launcher opened it: an exit with none written is a step
// before this failing (nsenter, setpriv, this binary), never the job's own code.
//
// As the namespace's init it cannot die by a signal itself, so a death by signal n (the
// job's, or one that arrived before the job started) is also written as n to descriptor
// signalFD when the launcher opened it: the launcher reports `status signal n`, and the
// wrapper dies by n as a caller expects of a command interrupted by Ctrl-C. A signal sent
// before its handlers exist is discarded (an init ignores what it does not handle), so the
// launcher also appends each signal it forwards to end the run to the file on descriptor
// abortFD, which this reads before it starts the job.
//
// STDIO is the wrapper's stdio plan (OFFLOAD_STDIO): a stream it names as `<stream> file
// <[r]a|[r]w> <offset> <path>` is the caller's regular file, which this opens on dev for the
// job (appending with `a`, read-write with a leading `r`), at the caller's offset, as the
// job's own, flushing it to dev every flushEvery while the job runs; once the job has ended
// it writes `<stream> <offset>` to descriptor offsetFD for each, where the job left it, or
// the plan's offsets when it ends before starting the job. `err merged` gives stderr
// stdout's file or relay. A file that will not open fails the run before the job.
//
// The job's other streams are pipes this process relays to its own (one pipe for both when
// they are one file). The caller's reader of one gone, the wrapper says so and the launcher
// appends `out` or `err` to the file on descriptor eowFD and sends this process alone
// readerGoneSignal: the relay of that stream closes, so the job's next write to it fails with EPIPE
// (SIGPIPE) as it would have locally, and the job runs on. Nothing else in the job is
// signalled: `sh -c 'yes; cleanup'` still runs cleanup. Once the job has ended, each relay
// passes on what is left in its pipe before this reports.
//
// A job that leaves the launcher's process group (`timeout`, `setsid`) gets each signal from
// this process instead, since the launcher's signal to the group no longer reaches it.
//
// It is built static (CGO_ENABLED=0): it starts from a page cache of its own on the remote
// in a few milliseconds, where an interpreter read from dev over NFS took a third of a
// second per job.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"devex/offload/internal/strerror"
)

const (
	setupFailed = 125
	signalFD    = 9
	abortFD     = 10
	eowFD       = 11
	offsetFD    = 12
	startedFD   = 13
)

// The signals the launcher forwards to end the run, by the names it records on abortFD.
var ending = map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT}

// The signals that inform a job (dd's progress) and end nothing.
var informational = map[string]syscall.Signal{"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2}

// flushSignal is raised by this process at itself to learn that every signal delivered
// before it has been handled; nothing else sends it.
const flushSignal = syscall.Signal(64)

// readerGoneSignal is the launcher's word that it recorded a stream on eowFD. It is not
// SIGPIPE: the job half ignores SIGPIPE, and a SIGPIPE sent to a Go process that inherited
// that ignore and lands on a thread running no goroutine is dropped (runtime sigfwdgo).
const readerGoneSignal = syscall.Signal(63)

// planLine is a stdio plan line naming an output file: stream, mode, offset and path, which
// is the line's rest and may hold spaces.
var planLine = regexp.MustCompile(`^(out|err) file (r?[aw]) ([0-9]+) (/.*)$`)

// The steps the tests replace; nothing else does.
var (
	flushEvery    = time.Second // between flushes of the job's output files to dev
	syncfs        = realSyncfs
	fsync         = syscall.Fsync
	chdir         = syscall.Chdir
	watchPaths    = watch
	readForwarded = forwarded
	startJob      = func(c *exec.Cmd) error { return c.Start() }
	sendSignal    = func(p *os.Process, s syscall.Signal) { _ = p.Signal(s) }
)

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	started := openedStarted() // before this opens anything, which could take the number
	if len(argv) < 6 || argv[4] != "--" {
		say("usage: offload-wait CWD WRITABLE PRIVATE STDIO -- COMMAND [ARGS...]")
		return setupFailed
	}
	cwd, writable, private, plan, command := argv[0], argv[1], argv[2], argv[3], argv[5:]
	keepFromJob()
	// A handler, not an ignore: exec resets a handled signal for the job, never an ignored
	// one. The launcher signals this process's group from before the job exists; a signal
	// that arrives before the job starts ends the run as the signal would have (SIGUSR1 and
	// SIGUSR2 inform a job, so with none yet they are dropped), and each that arrives while
	// the job is forked (the child's copy is lost at exec) is passed on after it.
	s := handleSignals()
	var opened []outputFile
	var streams map[string]*relay
	var job *exec.Cmd
	var seen int
	err := func() error {
		watched, err := watchPaths(lines(writable))
		if err != nil {
			return err
		}
		defer func() { s.watched = watched }()
		if err := chdir(cwd); err != nil {
			return &os.PathError{Op: "chdir", Path: cwd, Err: err}
		}
		// Each signal is judged once: those handled by now here, any later one passed on to
		// the job after the fork, so none lands between the two unseen. The launcher's
		// record lists its signals in the order it sent them, so it decides which came first.
		s.flush()
		s.mu.Lock()
		seen = len(s.caught)
		before := slices.Clone(s.caught)
		s.mu.Unlock()
		for _, n := range append(readForwarded(), before...) {
			if !isInformational(n) {
				return endedBy(n)
			}
		}
		var merged bool
		if opened, merged, err = outputFiles(plan); err != nil {
			return err
		}
		if streams, err = relays(opened, merged); err != nil {
			return err
		}
		s.setStreams(streams)
		// The job's end of each stream: its output file, or its relay's pipe; stdout's for
		// a merged stderr.
		ends := map[string]*os.File{}
		for k, r := range streams {
			ends[k] = r.w
		}
		for _, f := range opened {
			ends[f.stream] = f.f
		}
		job = exec.Command(command[0], command[1:]...)
		job.Stdin, job.Stdout, job.Stderr = os.Stdin, ends["out"], ends["err"]
		if merged {
			job.Stderr = ends["out"]
		}
		err = startJob(job)
		for _, r := range streams {
			r.w.Close() // a merged relay's twice, which is harmless
		}
		return err
	}()
	var ended endedBy
	if errors.As(err, &ended) {
		reportOffsets(planOffsets(plan))
		return diedBy(syscall.Signal(ended))
	}
	if err != nil {
		name, reason := describe(err)
		if name != "" {
			name += ": "
		}
		say(fmt.Sprintf("offload-launch: %s%s; the job did not run", name, reason))
		reportOffsets(planOffsets(plan))
		return setupFailed
	}
	// Flushed first, a signal handled before the job is recorded is in duringFork, and one
	// after it is passed on by the handler.
	s.flush()
	s.mu.Lock()
	s.job = job.Process
	duringFork := slices.Clone(s.caught[seen:])
	s.mu.Unlock()
	mark(started, "1", "started")
	reaped := make(chan struct{})
	go reapOrphans(job.Process.Pid, reaped)
	stopFlushing, flushed := flushLive(opened)
	for _, n := range duringFork {
		sendSignal(job.Process, n)
	}
	s.closeEnded() // a stream whose reader went before the handler or the relays existed
	_ = job.Wait()
	close(reaped)
	status := job.ProcessState.Sys().(syscall.WaitStatus)
	code := status.ExitStatus()
	if status.Signaled() {
		code = diedBy(status.Signal())
	}
	// A signal the launcher forwarded to end the run reached the job's other processes too
	// (`sh -c 'a & b & wait'` dies before its children's handlers finish); as on a local
	// run, they finish before the run ends, since this init's exit would kill them
	// mid-handler. The launcher's grace kill bounds the wait, so only its signals count: one
	// the job sent its own group (`kill 0`) starts no grace, and the wait would be unbounded.
	if slices.ContainsFunc(readForwarded(), func(n syscall.Signal) bool { return !isInformational(n) }) {
		for len(stragglers()) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	// The rest die with this init as it exits; killed now, none still writing holds a
	// relay's drain open, and what they wrote is passed on.
	left := stragglers()
	if len(left) > 0 {
		_ = syscall.Kill(-1, syscall.SIGKILL)
		for len(stragglers()) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	mark(started, "2", "ended")
	for _, r := range unique(streams) {
		r.finish()
	}
	close(stopFlushing)
	<-flushed
	var offsets []streamOffset
	for _, f := range opened {
		at, _ := f.f.Seek(0, io.SeekCurrent)
		offsets = setOffset(offsets, f.stream, at)
	}
	reportOffsets(offsets)

	failed := false
	for _, w := range s.watched {
		if e := syncfs(w.fd); e != 0 {
			failed = true
			say(fmt.Sprintf("offload: writing to dev failed under %s: %s; files the job wrote there may be incomplete",
				strings.Join(w.paths, ", "), strerror.Of(e)))
		}
	}
	for _, entry := range leftovers(lines(private)) {
		failed = true
		say(fmt.Sprintf("offload: the job left files in a directory private to the remote, deleted as the job ends: %s; run it with --where local to keep them", entry))
	}
	if len(left) > 0 {
		say("offload: the job left processes running, stopped as it ends (a local run would have kept them): " + strings.Join(left, ", "))
	}
	if code == 0 && failed {
		return 1
	}
	return code
}

// endedBy is a signal that ended the run before the job started.
type endedBy syscall.Signal

func (e endedBy) Error() string { return syscall.Signal(e).String() }

// planLineError is a stdio plan line this cannot read.
type planLineError string

func (e planLineError) Error() string { return string(e) }

// describe is the name a setup failure is about, if any, and its reason, as strerror words.
func describe(err error) (name, reason string) {
	var pathErr *os.PathError
	var execErr *exec.Error
	var lineErr planLineError
	var errno syscall.Errno
	switch {
	case errors.As(err, &lineErr):
		return string(lineErr), "not a stdio plan line"
	case errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound):
		return execErr.Name, strerror.Of(syscall.ENOENT)
	case errors.As(err, &pathErr) && errors.As(pathErr.Err, &errno):
		return pathErr.Path, strerror.Of(errno)
	case errors.As(err, &errno):
		return "", strerror.Of(errno)
	}
	return "", err.Error()
}

// say prints message and a newline on stderr. A caller whose reader of stderr is gone is
// not told, and the job's code still stands.
func say(message string) { _, _ = writeAll(2, []byte(message+"\n")) }

func lines(s string) []string {
	return slices.DeleteFunc(strings.Split(s, "\n"), func(l string) bool { return l == "" })
}

func isInformational(n syscall.Signal) bool {
	return slices.Contains(slices.Collect(maps.Values(informational)), n)
}

// openedStarted is startedFD when the launcher opened it, else -1.
func openedStarted() int {
	var st syscall.Stat_t
	if syscall.Fstat(startedFD, &st) != nil {
		return -1
	}
	return startedFD
}

// keepFromJob marks every descriptor this process inherited past stderr close-on-exec: the
// job gets its standard files alone, never the launcher's control channel or this binary.
func keepFromJob() {
	d, err := os.Open("/proc/self/fd")
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if fd, err := strconv.Atoi(n); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}

// signals is what the handler shares with the run.
type signals struct {
	mu      sync.Mutex
	caught  []syscall.Signal // before the job is recorded
	job     *os.Process
	streams map[string]*relay
	flushed chan struct{}
	watched []watchedFS
}

var handled = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2}

func handleSignals() *signals {
	s := &signals{flushed: make(chan struct{})}
	c := make(chan os.Signal, 32)
	signal.Notify(c, append(handled, flushSignal, readerGoneSignal)...)
	go func() {
		for v := range c {
			n := v.(syscall.Signal)
			switch n {
			case flushSignal:
				s.flushed <- struct{}{}
			case readerGoneSignal:
				s.closeEnded() // a stream's reader is gone: the job is not signalled
			case syscall.SIGPIPE:
				// handled only so the job gets it at its default; the launcher never sends it
			default:
				s.mu.Lock()
				if s.job == nil {
					s.caught = append(s.caught, n)
				} else {
					passOn(s.job, n)
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// flush returns once every signal delivered before it has been handled: one sent to this
// process is delivered before kill returns, lowest number first, and the handler takes
// them in order.
func (s *signals) flush() {
	_ = syscall.Kill(os.Getpid(), flushSignal)
	<-s.flushed
}

func (s *signals) setStreams(streams map[string]*relay) {
	s.mu.Lock()
	s.streams = streams
	s.mu.Unlock()
}

// closeEnded closes the relay of each stream the launcher recorded as having lost its reader.
func (s *signals) closeEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range endedStreams() {
		if r := s.streams[name]; r != nil {
			r.close()
		}
	}
}

// passOn sends sig to a job that left this process's group (`timeout`, `setsid`), which the
// launcher's signal to the group no longer reaches; one still in it already has it.
func passOn(job *os.Process, sig syscall.Signal) {
	if pgid, err := syscall.Getpgid(job.Pid); err == nil && pgid != syscall.Getpgrp() {
		sendSignal(job, sig)
	}
}

// diedBy is 128+sig, the code for death by sig, after telling the launcher of the signal.
func diedBy(sig syscall.Signal) int {
	_, _ = writeAll(signalFD, []byte(strconv.Itoa(int(sig)))) // fails run outside the launcher
	return 128 + int(sig)
}

// forwarded is the signals the launcher forwarded to end the run, read from abortFD: one
// that reached this process before its handlers did was discarded (an init ignores a signal
// it has no handler for), so the launcher records each before it sends it.
func forwarded() []syscall.Signal {
	var out []syscall.Signal
	for _, n := range readRecord(abortFD) {
		if sig, ok := ending[n]; ok {
			out = append(out, sig)
		} else if sig, ok := informational[n]; ok {
			out = append(out, sig)
		}
	}
	return out
}

// endedStreams is the streams the launcher recorded on eowFD as having lost their reader.
func endedStreams() []string { return readRecord(eowFD) }

// readRecord is the words of the launcher's record on fd; none when it is not open, as run
// outside the launcher.
func readRecord(fd int) []string {
	buf := make([]byte, 4096)
	n, err := retry(func() (int, error) { return syscall.Pread(fd, buf, 0) })
	if err != nil {
		return nil
	}
	return strings.Fields(string(buf[:n]))
}

// watchedFS is one open descriptor on a filesystem with the writable paths it covers.
type watchedFS struct {
	fd    int
	paths []string
}

// watch opens one descriptor per filesystem among paths that exist. Any path of a
// filesystem serves for its syncfs; only a filesystem none of whose paths opens fails, with
// the last path's error.
func watch(paths []string) ([]watchedFS, error) {
	var devs []uint64
	byDev := map[uint64][]string{}
	for _, p := range paths {
		var st syscall.Stat_t
		if err := ignoringEINTR(func() error { return syscall.Stat(p, &st) }); err != nil {
			if err == syscall.ENOENT || err == syscall.ENOTDIR {
				continue // absent, as offload-bind --skip-missing judges it
			}
			return nil, &os.PathError{Op: "stat", Path: p, Err: err}
		}
		if _, ok := byDev[st.Dev]; !ok {
			devs = append(devs, st.Dev)
		}
		byDev[st.Dev] = append(byDev[st.Dev], p)
	}
	var watched []watchedFS
	for _, dev := range devs {
		covered := byDev[dev]
		for i, p := range covered {
			fd, err := openAny(p)
			if err == nil {
				watched = append(watched, watchedFS{fd, covered})
				break
			}
			// A path no open reaches (unwritable, a 0333 directory, a socket's ENXIO) leaves
			// the filesystem's other paths to try.
			switch err {
			case syscall.EACCES, syscall.EPERM, syscall.EISDIR, syscall.ENXIO:
				if i < len(covered)-1 {
					continue
				}
			}
			return nil, &os.PathError{Op: "open", Path: p, Err: err}
		}
	}
	return watched, nil
}

// openAny opens path for reading, or else for writing: a write-only file still counts.
func openAny(path string) (int, error) {
	const flags = syscall.O_NONBLOCK | syscall.O_NOCTTY | syscall.O_CLOEXEC
	fd, err := retry(func() (int, error) { return syscall.Open(path, syscall.O_RDONLY|flags, 0) })
	if err == syscall.EACCES || err == syscall.EPERM {
		return retry(func() (int, error) { return syscall.Open(path, syscall.O_WRONLY|flags, 0) })
	}
	return fd, err
}

// sysSyncfs is syncfs(2) on linux-amd64, which syscall does not name.
const sysSyncfs = 306

func realSyncfs(fd int) syscall.Errno {
	_, _, e := syscall.Syscall(sysSyncfs, uintptr(fd), 0, 0)
	return e
}

// runtimeLock is podman's lock table, which every rootless podman run leaves in /dev/shm and
// the next run recreates: it holds no data, so a job leaving it loses nothing.
var runtimeLock = regexp.MustCompile(`^libpod(_rootless)?_lock(_[0-9]+)?$`)

// leftovers is what is left in each of dirs, as `dir: name, ...`, less podman's lock table;
// a directory it cannot list counts.
func leftovers(dirs []string) []string {
	var left []string
	for _, d := range dirs {
		names, err := listDir(d)
		if err == syscall.ENOENT {
			continue
		}
		if err != nil {
			left = append(left, fmt.Sprintf("%s: cannot be listed (%s)", d, strerror.Of(err.(syscall.Errno))))
			continue
		}
		names = slices.DeleteFunc(names, runtimeLock.MatchString)
		slices.Sort(names)
		if len(names) > 0 {
			left = append(left, d+": "+strings.Join(names, ", "))
		}
	}
	return left
}

// listDir is the names in dir, or the errno that kept it from being listed.
func listDir(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err == nil {
		var names []string
		names, err = f.Readdirnames(-1)
		f.Close()
		if err == nil {
			return names, nil
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return nil, errno
	}
	return nil, syscall.EIO
}

// stragglers is `pid name` of each live process in this pid namespace but this one, when
// this is its init (pid 1); none otherwise, since then the namespace is not the job's alone.
func stragglers() []string {
	if os.Getpid() != 1 {
		return nil
	}
	d, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	var pids []int
	for _, n := range names {
		if pid, err := strconv.Atoi(n); err == nil && pid != 1 {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	var left []string
	for _, pid := range pids {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			continue // gone meanwhile
		}
		open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if open < 0 || end+2 >= len(stat) {
			continue
		}
		if stat[end+2] != 'Z' {
			left = append(left, fmt.Sprintf("%d %s", pid, stat[open+1:end]))
		}
	}
	return left
}

// reapOrphans collects each process that ends after the job left it to this init (`cmd &`
// in a subshell, a daemon), as a local init would: uncollected, it stays a zombie that
// `kill -0` and `pgrep` still find, and a job polling for it to go waits forever. The job's
// own status is its Wait's, so this waits for reaped rather than take it.
func reapOrphans(job int, reaped <-chan struct{}) {
	for {
		var info [128]byte // siginfo_t
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, 0 /* P_ALL */, 0,
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return // no children: the job is collected and left none behind
		}
		pid := int(*(*int32)(unsafe.Pointer(&info[16]))) // si_pid
		if pid == job {
			<-reaped
			continue
		}
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
	}
}

// outputFile is a stream the plan names as the caller's regular file, opened for the job.
type outputFile struct {
	stream string
	f      *os.File
}

// outputFiles opens the output files the plan names, each at the caller's offset, in the
// plan's order; and says whether stderr is stdout's (`err merged`).
func outputFiles(plan string) ([]outputFile, bool, error) {
	var files []outputFile
	merged := false
	for _, line := range strings.Split(plan, "\n") {
		m := planLine.FindStringSubmatch(line)
		switch {
		case line == "err merged":
			merged = true
		case m != nil:
			stream, mode, path := m[1], m[2], m[4]
			offset, err := strconv.ParseInt(m[3], 10, 64)
			if err != nil {
				return nil, false, planLineError(line)
			}
			access := os.O_WRONLY
			if strings.HasPrefix(mode, "r") {
				access = os.O_RDWR
			}
			if strings.HasSuffix(mode, "a") {
				access |= os.O_APPEND
			}
			f, err := os.OpenFile(path, access|syscall.O_NOCTTY, 0)
			if err != nil {
				return nil, false, err
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return nil, false, err
			}
			files = slices.DeleteFunc(files, func(o outputFile) bool { return o.stream == stream })
			files = append(files, outputFile{stream, f})
		case line != "":
			return nil, false, planLineError(line)
		}
	}
	return files, merged, nil
}

// streamOffset is where an output file was or was left, for the launcher's record.
type streamOffset struct {
	stream string
	at     int64
}

// setOffset sets stream's offset, keeping the order streams were first named in.
func setOffset(offsets []streamOffset, stream string, at int64) []streamOffset {
	for i := range offsets {
		if offsets[i].stream == stream {
			offsets[i].at = at
			return offsets
		}
	}
	return append(offsets, streamOffset{stream, at})
}

// planOffsets is each output file's offset as the plan gives it: where a job that never ran
// left it.
func planOffsets(plan string) []streamOffset {
	var offsets []streamOffset
	for _, line := range strings.Split(plan, "\n") {
		if m := planLine.FindStringSubmatch(line); m != nil {
			if at, err := strconv.ParseInt(m[3], 10, 64); err == nil {
				offsets = setOffset(offsets, m[1], at)
			}
		}
	}
	return offsets
}

// reportOffsets writes where the job left each output file to offsetFD, for the launcher. A
// failed write is reported and the run's code kept: without the record the wrapper leaves
// each file's descriptor at its end.
func reportOffsets(offsets []streamOffset) {
	var record strings.Builder
	for _, o := range offsets {
		fmt.Fprintf(&record, "%s %d\n", o.stream, o.at)
	}
	_, err := writeAll(offsetFD, []byte(record.String()))
	if errno, ok := err.(syscall.Errno); ok && errno != syscall.EBADF { // EBADF: run outside the launcher
		say("offload-launch: recording where the job left its output failed: " + strerror.Of(errno))
	}
}

// mark writes step to fd: `1` once the job runs, so the launcher reports its exit as the
// job's; `2` once it and all it left have ended, so the grace kill spares what remains (its
// output's drain, the flush to dev), which a local run would have waited out.
func mark(fd int, step, what string) {
	if fd < 0 {
		return
	}
	if _, err := writeAll(fd, []byte(step)); err != nil {
		say(fmt.Sprintf("offload-launch: recording that the job %s failed: %s", what, strerror.Of(err.(syscall.Errno))))
	}
}

// flushLive flushes each output file to dev every flushEvery until stop is closed, so a
// reader there (`tail -f`) sees the job's output while it runs, as on a local run; the NFS
// client would otherwise hold it until the job ends. A failed flush is left to the final
// syncfs check, which reports it. done is closed once it has stopped.
func flushLive(files []outputFile) (stop, done chan struct{}) {
	stop, done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				for _, f := range files {
					_ = fsync(int(f.f.Fd()))
				}
			}
		}
	}()
	return stop, done
}

// relay copies the pipe the job writes one stream to onto dst, this process's own, until the
// job's end (finish: what is left, then stop) or the caller's reader of it is gone (close:
// stop at once). Closing the pipe's read end is what makes the job's next write fail.
type relay struct {
	dst          int
	r            int
	w            *os.File // the job's end
	stopR, stopW int
	stopped      atomic.Bool
	done         chan struct{}
	buf          []byte
}

func newRelay(dst int) (*relay, error) {
	var p, q [2]int
	if err := syscall.Pipe2(p[:], syscall.O_CLOEXEC); err != nil {
		return nil, err
	}
	if err := syscall.Pipe2(q[:], syscall.O_CLOEXEC); err != nil {
		syscall.Close(p[0])
		syscall.Close(p[1])
		return nil, err
	}
	r := &relay{dst: dst, r: p[0], w: os.NewFile(uintptr(p[1]), "relay"), stopR: q[0], stopW: q[1], done: make(chan struct{})}
	go r.run()
	return r, nil
}

// relays makes a relay per stream that is not an output file, one shared when stdout and
// stderr are one file.
func relays(files []outputFile, merged bool) (map[string]*relay, error) {
	isFile := func(stream string) bool {
		return slices.ContainsFunc(files, func(f outputFile) bool { return f.stream == stream })
	}
	out := map[string]*relay{}
	if !isFile("out") {
		r, err := newRelay(1)
		if err != nil {
			return nil, err
		}
		out["out"] = r
	}
	if merged {
		if r := out["out"]; r != nil {
			out["err"] = r
		}
		return out, nil
	}
	if !isFile("err") {
		r, err := newRelay(2)
		if err != nil {
			return nil, err
		}
		out["err"] = r
	}
	return out, nil
}

// unique is each relay once: a merged one serves two streams.
func unique(streams map[string]*relay) []*relay {
	var rs []*relay
	for _, r := range streams {
		if !slices.Contains(rs, r) {
			rs = append(rs, r)
		}
	}
	return rs
}

// close stops the relay at once: the caller's reader of this stream is gone.
func (r *relay) close() {
	if r.stopped.CompareAndSwap(false, true) {
		_, _ = writeAll(r.stopW, []byte("c"))
	}
}

// finish passes on what the job left in the pipe, then stops; it returns once stopped.
func (r *relay) finish() {
	if r.stopped.CompareAndSwap(false, true) {
		_, _ = writeAll(r.stopW, []byte("d"))
	}
	<-r.done
}

func (r *relay) run() {
	defer close(r.done)
	defer syscall.Close(r.r)
	r.buf = make([]byte, 1<<16)
	for {
		fds := []pollFd{{fd: int32(r.r), events: pollIn}, {fd: int32(r.stopR), events: pollIn}}
		poll(fds, -1)
		if fds[1].revents != 0 {
			var b [1]byte
			_, _ = retry(func() (int, error) { return syscall.Read(r.stopR, b[:]) })
			if b[0] == 'd' {
				r.drain()
			}
			return
		}
		more, err := r.copy()
		if err == syscall.EAGAIN {
			waitWritable(r.dst) // a non-blocking dst is full
			continue
		}
		if !more {
			return
		}
	}
}

// drain passes on what is left in the pipe, however slowly dst's reader takes it, and stops
// once the pipe is empty. Non-blocking, a splice fails EAGAIN both on an empty pipe and on a
// full dst, so a full dst is waited out rather than taken for the end.
func (r *relay) drain() {
	_ = syscall.SetNonblock(r.r, true)
	for {
		more, err := r.copy()
		if err == syscall.EAGAIN {
			if poll([]pollFd{{fd: int32(r.r), events: pollIn}}, 0) == 0 {
				return // the pipe is empty
			}
			waitWritable(r.dst) // dst is full
			continue
		}
		if !more {
			return
		}
	}
}

// copy moves one chunk from the pipe to dst: false at the pipe's end or when dst fails (its
// reader gone too: the job's next write then fails as dst's did). A non-blocking pipe (a
// drain) or dst (the caller's, set so upstream) that cannot move a chunk returns EAGAIN; a
// chunk already read is written whole, waiting on dst.
func (r *relay) copy() (bool, error) {
	n, err := retry(func() (int, error) {
		n, err := syscall.Splice(r.r, nil, r.dst, nil, len(r.buf), 0)
		return int(n), err
	})
	switch err {
	case nil:
		return n > 0, nil
	case syscall.EAGAIN:
		return false, err
	case syscall.EINVAL: // dst is no pipe: splice cannot write it
	default:
		return false, nil
	}
	n, err = retry(func() (int, error) { return syscall.Read(r.r, r.buf) })
	if err == syscall.EAGAIN {
		return false, err
	}
	if err != nil || n == 0 {
		return false, nil
	}
	if _, err := writeAll(r.dst, r.buf[:n]); err != nil {
		return false, nil
	}
	return true, nil
}

// writeAll writes b whole to fd, waiting out a full non-blocking one.
func writeAll(fd int, b []byte) (int, error) {
	done := 0
	for {
		n, err := retry(func() (int, error) { return syscall.Write(fd, b[done:]) })
		if err == syscall.EAGAIN {
			waitWritable(fd)
			continue
		}
		if err != nil {
			return done, err
		}
		done += n
		if done == len(b) {
			return done, nil
		}
	}
}

const (
	pollIn  = 0x1
	pollOut = 0x4
)

type pollFd struct {
	fd      int32
	events  int16
	revents int16
}

// poll waits for an event on fds, at most timeout milliseconds (forever when negative), and
// returns how many have one.
func poll(fds []pollFd, timeout int) int {
	var ts *syscall.Timespec
	if timeout >= 0 {
		t := syscall.NsecToTimespec(int64(timeout) * int64(time.Millisecond))
		ts = &t
	}
	n, _ := retry(func() (int, error) {
		n, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&fds[0])), uintptr(len(fds)),
			uintptr(unsafe.Pointer(ts)), 0, 0, 0)
		if e != 0 {
			return 0, e
		}
		return int(n), nil
	})
	return n
}

func waitWritable(fd int) { poll([]pollFd{{fd: int32(fd), events: pollOut}}, -1) }

// retry calls f until it fails other than by EINTR, which a handled signal causes.
func retry(f func() (int, error)) (int, error) {
	for {
		n, err := f()
		if err != syscall.EINTR {
			return n, err
		}
	}
}

func ignoringEINTR(f func() error) error {
	_, err := retry(func() (int, error) { return 0, f() })
	return err
}
