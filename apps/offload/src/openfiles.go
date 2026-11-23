package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// selfMountinfo is the mount table lockDeviceMismatch reads; a var so a test can show a
// writable path on a device its locks do not name.
var selfMountinfo = "/proc/self/mountinfo"

// fdGlob names every open file of dev's processes. A var so a test build can look at
// one process only (-X main.fdGlob=): dev's own processes hold WAL databases under the
// writable ~/code at any time.
var fdGlob = allFds

const allFds = "/proc/[0-9]*/fd/*"

// openFile is a file a dev process has open: its path, and its identity as /proc/locks
// names it (zero when it could not be read).
type openFile struct {
	path    string
	id      fileID
	foreign bool   // the holder is in another mount namespace, so path is as it sees it, not dev
	nlink   uint64 // the file's names (hard links), zero when it could not be read
	pid     string // the holder

	flock           bool // the holder's fdinfo lists a flock on it
	excludesReaders bool // and a write lock over SQLite's reader bytes (excludesSQLiteReaders)
}

// fileID is a file's device and inode, as /proc/locks prints them.
type fileID struct {
	major, minor uint32
	ino          uint64
}

// openFileTargets lists what dev's processes have open, and the /proc/<pid> folders of
// those whose open files it may not read (another account's).
func openFileTargets() ([]openFile, []string) {
	dirs, _ := filepath.Glob(filepath.Dir(fdGlob))
	return openFilesOf(dirs)
}

// openFilesOf lists the open files under each /proc/<pid>/fd folder, with the locks the
// holder's fdinfo lists on each regular file and directory, and the /proc/<pid> folders it
// may not read; a process that has ended adds neither. fdinfo lists a lock on every
// descriptor that shares its open file (a flock's or an OFD lock's), or on the owner's
// (a POSIX lock's), each file built in one pass, so unlike /proc/locks it skips none.
func openFilesOf(fdDirs []string) (out []openFile, unreadable []string) {
	self, _ := os.Readlink("/proc/self/ns/mnt")
	for _, dir := range fdDirs {
		proc := filepath.Dir(dir)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrPermission) {
			unreadable = append(unreadable, proc)
		}
		// A namespace that cannot be read counts as dev's own.
		ns, _ := os.Readlink(proc + "/ns/mnt")
		for _, e := range entries {
			fd := filepath.Join(dir, e.Name())
			t, err := os.Readlink(fd)
			if err != nil {
				continue
			}
			f := openFile{path: t, pid: filepath.Base(proc)}
			var st syscall.Stat_t
			if syscall.Stat(fd, &st) == nil {
				f.id, f.nlink = idOf(st), st.Nlink
				if st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
					f.nlink = 1 // a directory has one name; its count is its subdirectories
				}
				if m := st.Mode & syscall.S_IFMT; m == syscall.S_IFREG || m == syscall.S_IFDIR {
					info, _ := os.ReadFile(proc + "/fdinfo/" + e.Name())
					for _, l := range parseLockLines(string(info)) {
						f.flock = f.flock || l.kind == "FLOCK"
						f.excludesReaders = f.excludesReaders || l.excludesSQLiteReaders()
					}
				}
			}
			f.foreign = ns != "" && self != "" && ns != self
			out = append(out, f)
		}
	}
	return out, unreadable
}

// lockLine is a held lock as /proc/locks prints it (`1: FLOCK  ADVISORY  WRITE 123
// fd:01:456 0 EOF`), and fdinfo after `lock:`.
type lockLine struct {
	kind       string // FLOCK, POSIX, OFDLCK, LEASE
	write      bool
	id         fileID
	start, end uint64 // the byte range; end is MaxUint64 for EOF
}

// parseLockLines reads the held locks of s; a waiter's line carries `->` and holds nothing.
func parseLockLines(s string) []lockLine {
	var out []lockLine
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "lock:"))
		if len(f) < 6 || f[1] == "->" {
			continue
		}
		l := lockLine{kind: f[1], write: f[3] == "WRITE", end: math.MaxUint64}
		if _, err := fmt.Sscanf(f[5], "%x:%x:%d", &l.id.major, &l.id.minor, &l.id.ino); err != nil {
			continue
		}
		if len(f) >= 8 {
			l.start, _ = strconv.ParseUint(f[6], 10, 64)
			if f[7] != "EOF" {
				l.end, _ = strconv.ParseUint(f[7], 10, 64)
			}
		}
		out = append(out, l)
	}
	return out
}

// sqliteReaderFirst and sqliteReaderLast bound the bytes of a SQLite main file every reader
// share-locks before it reads (SHARED_FIRST, 2^30+2, and SHARED_SIZE, 510).
const sqliteReaderFirst, sqliteReaderLast = 1<<30 + 2, 1<<30 + 2 + 509

// excludesSQLiteReaders reports whether l is a POSIX or OFD write lock over any of those
// bytes: a connection in exclusive locking mode holds one for as long as it is
// open, and no other connection can read the database meanwhile.
func (l lockLine) excludesSQLiteReaders() bool {
	return l.write && (l.kind == "POSIX" || l.kind == "OFDLCK") && l.start <= sqliteReaderLast && l.end >= sqliteReaderFirst
}

// lockedFiles are the files /proc/locks names locked, by a lock of any kind, on one of
// devs: SQLite locks the database it has open, so a WAL database's holder holds one too. A
// line's pid is the locker's, which may have ended and left the lock to a process that
// shares its descriptor, so the pid names no holder.
func lockedFiles(locks string, devs map[fileID]bool) map[fileID]bool {
	ids := map[fileID]bool{}
	for _, l := range parseLockLines(locks) {
		dev := l.id
		dev.ino = 0
		if devs[dev] {
			ids[l.id] = true
		}
	}
	return ids
}

// hiddenHolders lists, read as root with their locks, the open files of the processes
// openFileTargets could not read, which may hold a database or a lock the job uses. It reads
// them whatever /proc/locks says, since /proc/locks may have skipped the lock one of them
// holds, and says why when root could not. It says why, too, when a lock on a writable
// path's device has no holder: no process has the file open, as for a lock an offloaded job
// holds, which dev's nfsd threads hold for it. locks is /proc/locks as read before the scan,
// and reread reads it again: a lock released during the scan (a hook, a git command) had a
// holder the scan missed, so only a lock in both reads counts.
func hiddenHolders(targets []openFile, unreadable []string, locks string, reread func() string, devs map[fileID]bool) ([]openFile, string) {
	var files []openFile
	if len(unreadable) > 0 {
		pids := make([]string, len(unreadable))
		for i, proc := range unreadable {
			pids[i] = filepath.Base(proc)
		}
		var err error
		if files, err = listAsRoot(pids); err != nil {
			return nil, fmt.Sprintf("offload could not read the open files of %d processes of other accounts (%v)", len(unreadable), err)
		}
	}
	locked := lockedFiles(locks, devs)
	for _, t := range slices.Concat(targets, files) {
		delete(locked, t.id)
	}
	if len(locked) == 0 {
		return files, ""
	}
	still := lockedFiles(reread(), devs)
	for id := range locked {
		if still[id] {
			return files, fmt.Sprintf("no dev process has %02x:%02x:%d open, as when another offloaded job holds the lock", id.major, id.minor, id.ino)
		}
	}
	return files, ""
}

// readLocks is /proc/locks, empty when it cannot be read. A var so a test can drop a lock
// from it, as a page skip does.
var readLocks = func() string {
	b, _ := os.ReadFile("/proc/locks")
	return string(b)
}

// listAsRoot lists the open files of pids, as this program's open-files mode run under
// sudo prints them. A var so a test can stand in for sudo.
var listAsRoot = func(pids []string) ([]openFile, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd := exec.Command("sudo", append([]string{"-n", self, openFilesCmd}, pids...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sudo -n %s %s: %v %s", self, openFilesCmd, err, strings.TrimSpace(stderr.String()))
	}
	var wire []wireFile
	if err := json.Unmarshal(out, &wire); err != nil {
		return nil, fmt.Errorf("sudo -n %s %s: %v", self, openFilesCmd, err)
	}
	files := make([]openFile, len(wire))
	for i, w := range wire {
		files[i] = w.openFile()
	}
	return files, nil
}

// openFilesCmd is the mode listAsRoot runs this program in, as root: it prints the open
// files of the pids it is given, as JSON.
const openFilesCmd = "open-files"

// wireFile is an openFile as open-files prints it.
type wireFile struct {
	Path         string
	Major, Minor uint32
	Ino          uint64
	Foreign      bool
	Nlink        uint64
	Pid          string
	Flock        bool
	Excludes     bool // excludesReaders
}

func (w wireFile) openFile() openFile {
	return openFile{w.Path, fileID{w.Major, w.Minor, w.Ino}, w.Foreign, w.Nlink, w.Pid, w.Flock, w.Excludes}
}

func (f openFile) wire() wireFile {
	return wireFile{f.path, f.id.major, f.id.minor, f.id.ino, f.foreign, f.nlink, f.pid, f.flock, f.excludesReaders}
}

// openFilesMain is the open-files mode: the open files of each pid, as JSON on out.
func openFilesMain(pids []string, out io.Writer) int {
	var dirs []string
	for _, pid := range pids {
		if _, err := strconv.Atoi(pid); err != nil {
			fmt.Fprintf(os.Stderr, "offload %s: %q is not a pid\n", openFilesCmd, pid)
			return 2
		}
		dirs = append(dirs, "/proc/"+pid+"/fd")
	}
	files, unreadable := openFilesOf(dirs)
	if len(unreadable) > 0 {
		fmt.Fprintf(os.Stderr, "offload %s: cannot read %s\n", openFilesCmd, strings.Join(unreadable, " "))
		return 1
	}
	wire := make([]wireFile, len(files))
	for i, f := range files {
		wire[i] = f.wire()
	}
	if err := json.NewEncoder(out).Encode(wire); err != nil {
		fmt.Fprintf(os.Stderr, "offload %s: %v\n", openFilesCmd, err)
		return 1
	}
	return 0
}

// idOf is a stat result's device and inode, as /proc/locks prints them.
func idOf(st syscall.Stat_t) fileID {
	return fileID{
		major: uint32((st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff),
		minor: uint32(st.Dev&0xff | (st.Dev>>12)&^0xff),
		ino:   st.Ino,
	}
}

// lockDeviceMismatch names a writable path whose files' device differs from the device its
// mount's locks are listed under, or "". /proc/locks names a file by its filesystem's
// device, which is what mountinfo shows; stat shows a btrfs subvolume's own device instead,
// so no open file would ever match a lock there and every lock check would pass unseen.
func lockDeviceMismatch(mountinfo io.Reader, paths []string) string {
	mounts, _ := readMountinfo(mountinfo)
	for _, p := range paths {
		rp := realPath(p)
		var on *mount
		for i := range mounts {
			if within([]string{mounts[i].point}, rp) != "" && (on == nil || len(mounts[i].point) >= len(on.point)) {
				on = &mounts[i]
			}
		}
		var st syscall.Stat_t
		if on == nil || syscall.Stat(p, &st) != nil {
			continue
		}
		id := idOf(st)
		if dev := fmt.Sprintf("%d:%d", id.major, id.minor); dev != on.device {
			return fmt.Sprintf("%s reports device %s but its filesystem's locks name %s, so offload cannot match a lock to the file it is on", p, dev, on.device)
		}
	}
	return ""
}

// deviceIDs are the devices the paths are on, as fileIDs with no inode; a missing path adds none.
func deviceIDs(paths []string) map[fileID]bool {
	devs := map[fileID]bool{}
	for _, p := range paths {
		var st syscall.Stat_t
		if syscall.Stat(p, &st) == nil {
			id := idOf(st)
			id.ino = 0
			devs[id] = true
		}
	}
	return devs
}
