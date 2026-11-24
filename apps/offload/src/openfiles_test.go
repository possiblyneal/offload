package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// /proc/locks names a file by its filesystem's device, which mountinfo shows; where stat
// shows another (a btrfs subvolume) no lock matches a file, so the job stays here.
func TestLockDeviceMismatchNamesAPathWhoseDeviceIsNotItsMounts(t *testing.T) {
	dir := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	id := idOf(st)
	line := func(dev string) string {
		return "1 0 " + dev + " / " + dir + " rw - btrfs /dev/x rw\n"
	}
	same := fmt.Sprintf("%d:%d", id.major, id.minor)
	if got := lockDeviceMismatch(strings.NewReader("9 0 8:1 / / rw - ext4 /dev/y rw\n"+line(same)), []string{dir}); got != "" {
		t.Errorf("matching devices refused: %s", got)
	}
	got := lockDeviceMismatch(strings.NewReader(line("0:4242")), []string{dir + "/missing", dir})
	if !strings.Contains(got, dir) || !strings.Contains(got, "0:4242") {
		t.Errorf("a mismatched device gave %q", got)
	}
	// A writable path spelled through a symlink is on the mount its real path is on.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if got := lockDeviceMismatch(strings.NewReader(line("0:4242")), []string{link}); !strings.Contains(got, "0:4242") {
		t.Errorf("a symlinked path on a mismatched device gave %q", got)
	}
}

// A writable path whose locks /proc/locks names under another device keeps the job here:
// no holder of a lock there could be matched to the file it is on.
func TestALockDeviceMismatchRefusesUnseenHolder(t *testing.T) {
	useTestMounts(t)
	dir := t.TempDir()
	mi := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mi, []byte("1 0 0:4242 / "+dir+" rw - btrfs /dev/x rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(m string) { selfMountinfo = m }(selfMountinfo)
	selfMountinfo = mi
	defer func(r func() string) { readLocks = r }(readLocks)
	readLocks = func() string { return "" }
	defer func(f func([]string) ([]openFile, error)) { listAsRoot = f }(listAsRoot)
	listAsRoot = func([]string) ([]openFile, error) { return nil, nil } // no sudo on a CI runner
	f, _ := gatherJobFacts(t.TempDir(), dir, "", []string{"make"}, []string{dir}, nil, nil, exportView{})
	if !strings.Contains(f.Unseen, "0:4242") {
		t.Fatalf("unseen %q, want the device mismatch named", f.Unseen)
	}
	if d := place(placeRequest{Remotes: []string{"server"}, Slots: map[string]slotState{"server": {Total: 1, Free: 1}}, Facts: f}); d.Rule != "unseen-holder" {
		t.Errorf("%+v, want the unseen-holder refusal", d)
	}
}

// openFileTargets marks a holder in another mount namespace, and not one in dev's own.
func TestOpenFileTargetsMarksAHolderInAnotherMountNamespace(t *testing.T) {
	needNamespaces(t, "-rm", "true")
	f := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args    []string
		foreign bool
	}{{nil, false}, {[]string{"-rm"}, true}} {
		cmd := exec.Command("sh", "-c", `exec 3<"$1"; echo up; exec sleep 30`, "sh", f)
		if c.args != nil {
			cmd = exec.Command("unshare", append(c.args, cmd.Args...)...)
		}
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		io.ReadFull(out, make([]byte, 3))
		// unshare execs the shell in place, so the pid is the holder's either way.
		defer func(g string) { fdGlob = g }(fdGlob)
		fdGlob = "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd/*"
		var found *openFile
		targets, _ := openFileTargets()
		for _, o := range targets {
			if o.path == f {
				found = &o
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
		if found == nil || found.foreign != c.foreign {
			t.Errorf("unshare %v: got %+v, want foreign %v", c.args, found, c.foreign)
		}
	}
}

// idOf splits a device number as the kernel's makedev packs it, including a minor above
// 255 and a major above 4095.
func TestIdOfSplitsLargeMajorAndMinor(t *testing.T) {
	mk := func(major, minor uint64) uint64 {
		return minor&0xff | (major&0xfff)<<8 | (minor&^0xff)<<12 | (major&^0xfff)<<32
	}
	for _, c := range [][2]uint32{{8, 1}, {253, 300}, {5000, 70000}} {
		got := idOf(syscall.Stat_t{Dev: mk(uint64(c[0]), uint64(c[1])), Ino: 7})
		if got != (fileID{c[0], c[1], 7}) {
			t.Errorf("makedev(%d,%d): got %+v", c[0], c[1], got)
		}
	}
}

// lockedFiles keeps the locks of every kind on the given devices, and skips a waiter's line.
func TestLockedFilesKeepsLocksOnTheGivenDevicesAndSkipsWaiters(t *testing.T) {
	locks := `1: FLOCK  ADVISORY  WRITE 99 103:02:11 0 EOF
2: POSIX  ADVISORY  READ 98 103:02:12 1073741826 1073742335
2: -> POSIX  ADVISORY  WRITE 97 103:02:13 0 EOF
3: OFDLCK ADVISORY  READ -1 00:2a:14 0 EOF
`
	got := lockedFiles(locks, map[fileID]bool{{major: 0x103, minor: 2}: true})
	want := map[fileID]bool{{0x103, 2, 11}: true, {0x103, 2, 12}: true}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Another account's processes are always read as root: /proc/locks may have skipped the
// lock one of them holds, so a lock with a seen holder, or none listed, proves nothing.
// A failure to read them is named, not passed over.
func TestHiddenHoldersReadsOtherAccountsFilesAsRoot(t *testing.T) {
	devs := map[fileID]bool{{major: 0x103, minor: 2}: true}
	locks := "1: FLOCK  ADVISORY  WRITE 99 103:02:11 0 EOF\n"
	seen := []openFile{{path: "/home/testuser/code/repo/lock", id: fileID{0x103, 2, 11}}}
	hiddenFile := openFile{path: "/home/testuser/code/repo/lock", id: fileID{0x103, 2, 11}}
	defer func(f func([]string) ([]openFile, error)) { listAsRoot = f }(listAsRoot)
	for _, c := range []struct {
		name       string
		targets    []openFile
		unreadable []string
		err        error
		calls      int
		files      int
		unseen     bool
	}{
		{"holder seen", seen, []string{"/proc/7"}, nil, 1, 1, false},
		{"holder seen, root cannot read", seen, []string{"/proc/7"}, errors.New("sudo: a password is required"), 1, 0, true},
		{"holder seen and no unreadable process", seen, nil, nil, 0, 0, false},
		{"no holder and no unreadable process", nil, nil, nil, 0, 0, true},
		{"holder hidden", nil, []string{"/proc/7", "/proc/8"}, nil, 1, 1, false},
		{"root cannot read", nil, []string{"/proc/7"}, errors.New("sudo: a password is required"), 1, 0, true},
	} {
		calls := 0
		listAsRoot = func(pids []string) ([]openFile, error) {
			calls++
			if want := []string{"7", "8"}[:len(c.unreadable)]; !slices.Equal(pids, want) {
				t.Errorf("%s: pids %v, want %v", c.name, pids, want)
			}
			if c.err != nil {
				return nil, c.err
			}
			return []openFile{hiddenFile}, nil
		}
		files, unseen := hiddenHolders(c.targets, c.unreadable, locks, unchanged(locks), devs)
		if calls != c.calls || len(files) != c.files || (unseen != "") != c.unseen {
			t.Errorf("%s: %d calls, files %v, unseen %q", c.name, calls, files, unseen)
		}
		if c.err != nil && !strings.Contains(unseen, "a password is required") {
			t.Errorf("%s: unseen %q does not say why", c.name, unseen)
		}
	}
	// A lock no process has open, as dev's nfsd threads hold one for another offloaded job,
	// is unseen whether or not a process was unreadable.
	listAsRoot = func([]string) ([]openFile, error) {
		return []openFile{{path: "/var/lib/other", id: fileID{0x103, 2, 99}}}, nil
	}
	for _, unreadable := range [][]string{nil, {"/proc/7"}} {
		_, unseen := hiddenHolders(nil, unreadable, locks, unchanged(locks), devs)
		if !strings.Contains(unseen, "no dev process has 103:02:11 open") {
			t.Errorf("unreadable %v: unseen %q, want the lock named", unreadable, unseen)
		}
	}
	if _, unseen := hiddenHolders(nil, nil, "1: POSIX  ADVISORY  READ 884574 103:02:11 128 128\n2: FLOCK ADVISORY WRITE 5 08:01:4 0 EOF\n", unchanged("1: POSIX  ADVISORY  READ 884574 103:02:11 128 128\n"), devs); unseen == "" {
		t.Errorf("an nfsd thread's POSIX lock on a writable device is not unseen")
	}
	if _, unseen := hiddenHolders(nil, nil, "2: FLOCK ADVISORY WRITE 5 08:01:4 0 EOF\n", unchanged(""), devs); unseen != "" {
		t.Errorf("a lock on another device is unseen: %q", unseen)
	}
}

// unchanged rereads /proc/locks as locks: nothing was released during the scan.
func unchanged(locks string) func() string { return func() string { return locks } }

// A lock released while the scan ran (a hook, a git command) had a holder the scan missed:
// it keeps no job here.
func TestHiddenHoldersIgnoresALockReleasedDuringTheScan(t *testing.T) {
	devs := map[fileID]bool{{major: 0x103, minor: 2}: true}
	locks := "1: FLOCK  ADVISORY  WRITE 99 103:02:11 0 EOF\n"
	later := "1: FLOCK  ADVISORY  WRITE 98 103:02:12 0 EOF\n" // a new lock; 11's is gone
	defer func(f func([]string) ([]openFile, error)) { listAsRoot = f }(listAsRoot)
	listAsRoot = func([]string) ([]openFile, error) { return nil, nil }
	for _, unreadable := range [][]string{nil, {"/proc/7"}} {
		if _, unseen := hiddenHolders(nil, unreadable, locks, unchanged(later), devs); unseen != "" {
			t.Errorf("unreadable %v: unseen %q for a lock released during the scan", unreadable, unseen)
		}
		if _, unseen := hiddenHolders(nil, unreadable, locks, unchanged(locks), devs); unseen == "" {
			t.Errorf("unreadable %v: a lock held throughout is not unseen", unreadable)
		}
	}
}

// open-files reads only pids, so root runs it on nothing else.
func TestOpenFilesRefusesAnArgumentThatIsNotAPid(t *testing.T) {
	var out strings.Builder
	if code := openFilesMain([]string{"1", "../etc"}, &out); code != 2 || out.Len() != 0 {
		t.Errorf("exit %d, out %q; want 2 and nothing printed", code, out.String())
	}
}

// A directory's link count is its subdirectories, not other names, so a flocked directory
// is bound read-only rather than refused as having hard links.
func TestOpenFilesOfGivesADirectoryOneName(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	files, _ := openFilesOf([]string{"/proc/self/fd"})
	for _, f := range files {
		if f.path == dir {
			if f.nlink != 1 {
				t.Errorf("nlink = %d, want 1", f.nlink)
			}
			return
		}
	}
	t.Fatalf("%s not among the open files", dir)
}

// Each open file carries the locks its holder's fdinfo lists on it: a flock, and a write
// lock over SQLite's reader bytes (an exclusive-mode connection's), POSIX or OFD; a read
// lock there is an ordinary reader's. open-files hands them on to the caller.
func TestOpenFilesOfReadsEachFilesLocksFromItsHolder(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *os.File {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	lockRange := func(f *os.File, cmd int, typ int16, start int64) {
		t.Helper()
		lk := syscall.Flock_t{Type: typ, Start: start, Len: 1}
		if err := syscall.FcntlFlock(f.Fd(), cmd, &lk); err != nil {
			t.Fatal(err)
		}
	}
	const ofdSetLk = 37 // F_OFD_SETLK
	if err := syscall.Flock(int(open("flocked").Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	lockRange(open("posix"), syscall.F_SETLK, syscall.F_WRLCK, 1073741826+100)
	lockRange(open("ofd"), ofdSetLk, syscall.F_WRLCK, 1073742335)
	lockRange(open("reader"), syscall.F_SETLK, syscall.F_RDLCK, 1073741826)
	lockRange(open("elsewhere"), syscall.F_SETLK, syscall.F_WRLCK, 0)
	open("plain")
	want := map[string][2]bool{"flocked": {true, false}, "posix": {false, true}, "ofd": {false, true},
		"reader": {false, false}, "elsewhere": {false, false}, "plain": {false, false}}
	check := func(how string, files []openFile) {
		t.Helper()
		got := map[string][2]bool{}
		for _, f := range files {
			if filepath.Dir(f.path) == dir {
				got[filepath.Base(f.path)] = [2]bool{f.flock, f.excludesReaders}
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: [flock, excludes readers] %v, want %v", how, got, want)
		}
	}
	files, _ := openFilesOf([]string{"/proc/self/fd"})
	check("openFilesOf", files)
	var out strings.Builder
	if code := openFilesMain([]string{strconv.Itoa(os.Getpid())}, &out); code != 0 {
		t.Fatalf("open-files exit %d", code)
	}
	var wire []wireFile
	if err := json.Unmarshal([]byte(out.String()), &wire); err != nil {
		t.Fatal(err)
	}
	files = nil
	for _, w := range wire {
		files = append(files, w.openFile())
	}
	check("open-files", files)
}
