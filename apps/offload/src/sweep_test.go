package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// plant writes an empty file at dir/rel, making its parents.
func plant(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The sweep removes silly-rename files at any depth of each directory it is given, and
// nothing else, even a dotfile that merely starts with .nfs.
func TestSweepRemovesOnlySillyRenameFiles(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	gone := []string{
		plant(t, a, ".nfs00000000000000000000abcd"),
		plant(t, a, "deep/er/.nfs0123456789abcdef01234567"),
		plant(t, b, ".nfs00000000000000000000abce"),
	}
	kept := []string{
		plant(t, a, ".nfsrc"),
		plant(t, a, ".nfs00000000000000000000abc"),   // 23 digits
		plant(t, a, ".nfs00000000000000000000ABCD"),  // the client writes lower case
		plant(t, a, ".nfs00000000000000000000abcd~"), // a longer name
		plant(t, a, "x.nfs00000000000000000000abcd"),
		plant(t, a, ".nfs0000000000000000000000zz"), // not hex
	}
	// A directory of that name is no silly-rename file: the client renames files only.
	dir := filepath.Join(a, ".nfs00000000000000000000abcf")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, dir)
	lock, err := holdJobsLock(filepath.Join(t.TempDir(), "jobs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if !sweepSillyRenames(lock, a, "", b) {
		t.Fatal("the sweep did not run with no other holder of the jobs lock")
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep (%v)", p, err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s did not survive the sweep: %v", p, err)
		}
	}
}

// The sweep gives up the lock when it ends: a sweeper then blocked (a full stderr pipe,
// Ctrl-Z) must not hold off every other wrapper.
func TestSweepReleasesTheJobsLockWhenItEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.lock")
	lock, err := holdJobsLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if !sweepSillyRenames(lock, t.TempDir()) {
		t.Fatal("the sweep did not run")
	}
	other, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Errorf("another wrapper cannot share the jobs lock after the sweep: %v", err)
	}
}

// sigMasks is the process's ignored and caught signal masks, from /proc/self/status.
func sigMasks(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "SigIgn:") || strings.HasPrefix(l, "SigCgt:") {
			out = append(out, l)
		}
	}
	return strings.Join(out, " ")
}

// The sweep leaves every signal's disposition as it found it: a signal left ignored would
// keep a wrapper blocked after the sweep from being stopped by anything but SIGKILL.
func TestSweepRestoresSignalDispositions(t *testing.T) {
	lock, err := holdJobsLock(filepath.Join(t.TempDir(), "jobs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before := sigMasks(t)
	if !sweepSillyRenames(lock, t.TempDir()) {
		t.Fatal("the sweep did not run")
	}
	if after := sigMasks(t); after != before {
		t.Errorf("signal masks %s before the sweep, %s after", before, after)
	}
}

// A wrapper waits out a sweep for jobsLockWait at most, then fails, so it runs the job
// here rather than hang behind a stuck sweeper.
func TestHoldingTheJobsLockGivesUpOnAStuckSweeper(t *testing.T) {
	defer func(w time.Duration) { jobsLockWait = w }(jobsLockWait)
	jobsLockWait = 200 * time.Millisecond
	path := filepath.Join(t.TempDir(), "jobs.lock")
	sweeper, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer sweeper.Close()
	if err := syscall.Flock(int(sweeper.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if f, err := holdJobsLock(path); err == nil {
		f.Close()
		t.Fatal("took the jobs lock while a sweeper held it exclusively")
	}
	if took := time.Since(t0); took < 200*time.Millisecond || took > 5*time.Second {
		t.Errorf("gave up after %v, want about 200ms", took)
	}
}

// A lock file created under a strict umask still lets other accounts share it.
func TestTheJobsLockIsReadableByEveryAccount(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "jobs.lock")
	f, err := holdJobsLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("jobs lock mode %v, want 0644", st.Mode().Perm())
	}
}

// Another wrapper's job may hold a silly-rename file open, so while any other wrapper holds
// the jobs lock the sweep leaves everything alone.
func TestSweepIsSkippedWhileAnotherJobHoldsTheJobsLock(t *testing.T) {
	dir, path := t.TempDir(), filepath.Join(t.TempDir(), "jobs.lock")
	held := plant(t, dir, ".nfs00000000000000000000abcd")
	other, err := holdJobsLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lock, err := holdJobsLock(path)
	if err != nil {
		t.Fatal("a second job could not share the jobs lock:", err)
	}
	defer lock.Close()
	if sweepSillyRenames(lock, dir) {
		t.Error("the sweep ran while another job held the jobs lock")
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("the sweep removed a file another job may hold: %v", err)
	}
}

// A sweep cut off by its bound says so, so files it left behind are not a silent surprise.
func TestASweepThatRunsOutOfTimeSaysSo(t *testing.T) {
	defer func(d time.Duration) { sweepTimeout = d }(sweepTimeout)
	sweepTimeout = time.Nanosecond
	lock, err := holdJobsLock(filepath.Join(t.TempDir(), "jobs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func(f *os.File) { os.Stderr = f }(os.Stderr)
	os.Stderr = w
	dir := t.TempDir()
	sweepSillyRenames(lock, dir)
	w.Close()
	b, _ := io.ReadAll(r)
	if !strings.Contains(string(b), "ran past") || !strings.Contains(string(b), dir) {
		t.Errorf("stderr %q: want the timeout and the directory named", b)
	}
}

// A wrapper sweeping when the terminal sends Ctrl-C, Ctrl-\ or Ctrl-Z to its group, or a
// SIGQUIT reaches it alone, finishes the sweep and goes on to report the job: none of them
// ends it, stops it or cuts find off, since find runs in a group of its own.
func TestASweepOutlivesTheSignalsItDrops(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	pgid := filepath.Join(t.TempDir(), "pgid")
	shim := "#!/bin/sh\nps -o pgid= $$ > " + pgid + "\nsleep 2\nexec /usr/bin/find \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGQUIT, syscall.SIGINT, syscall.SIGTSTP} {
		for _, group := range []bool{false, true} {
			os.Remove(pgid)
			left := plant(t, dir, ".nfs00000000000000000000abcd")
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "OFFLOAD_TEST_QUIT=sweep", "PATH="+bin+":"+os.Getenv("PATH"),
				"OFFLOAD_TEST_LOCK="+filepath.Join(t.TempDir(), "jobs.lock"), "OFFLOAD_TEST_DIR="+dir)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			io.ReadFull(out, make([]byte, len("ready\n")))
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if b, _ := os.ReadFile(pgid); len(strings.TrimSpace(string(b))) > 0 {
					break
				}
			}
			target := cmd.Process.Pid
			if group {
				target = -target
			}
			syscall.Kill(target, sig)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				t.Errorf("%v to the wrapper (group %v): the sweep did not end", sig, group)
				continue
			}
			if code := cmd.ProcessState.ExitCode(); code != 7 {
				t.Errorf("%v to the wrapper (group %v): exit %d (%v), want the sweep to finish and the run to go on; stderr %q", sig, group, code, cmd.ProcessState, stderr.String())
			}
			if _, err := os.Stat(left); !os.IsNotExist(err) {
				t.Errorf("%v to the wrapper (group %v): the sweep left %s (%v); stderr %q", sig, group, left, err, stderr.String())
			}
			if b, _ := os.ReadFile(pgid); strings.TrimSpace(string(b)) == strconv.Itoa(cmd.Process.Pid) {
				t.Errorf("find ran in the wrapper's process group %s", b)
			}
		}
	}
}

// A find killed by a signal mid-walk is reported, not taken for a finished sweep.
func TestASweepWhoseFindIsKilledSaysSo(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte("#!/bin/sh\nkill -KILL $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	lock, err := holdJobsLock(filepath.Join(t.TempDir(), "jobs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func(f *os.File) { os.Stderr = f }(os.Stderr)
	os.Stderr = w
	dir := t.TempDir()
	sweepSillyRenames(lock, dir)
	w.Close()
	b, _ := io.ReadAll(r)
	if !strings.Contains(string(b), "cut off by killed") || !strings.Contains(string(b), dir) {
		t.Errorf("stderr %q: want the cut-off sweep and the directory named", b)
	}
}

// sweepChild runs a sweep whose find cannot start, so it ends on a message, in a child
// whose stderr is w, holding the jobs lock at path.
func sweepChild(t *testing.T, path string, w *os.File) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_SWEEP="+path, "OFFLOAD_TEST_DIR="+t.TempDir())
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// A sweep's message to a reader that has gone leaves the wrapper to go on to its outcome
// and its record, not to die of SIGPIPE.
func TestASweepOutlivesItsReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	cmd := sweepChild(t, filepath.Join(t.TempDir(), "jobs.lock"), w)
	w.Close()
	if err := cmd.Wait(); err == nil || err.(*exec.ExitError).ExitCode() != 7 {
		t.Errorf("want exit 7, got %v", err)
	}
}

// A sweeper blocked writing its message to a full pipe has already given up the jobs lock.
func TestASweepBlockedOnItsMessageHoldsNoLock(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fd := int(w.Fd()) // Fd makes it blocking
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for { // fill the pipe, so the child's message blocks
		if _, err := syscall.Write(fd, make([]byte, 4096)); err != nil {
			break
		}
	}
	path := filepath.Join(t.TempDir(), "jobs.lock")
	cmd := sweepChild(t, path, w)
	w.Close()
	defer func() { go io.Copy(io.Discard, r); cmd.Wait() }()
	f, err := os.Open(path)
	for i := 0; err != nil && i < 50; i++ { // the child creates it
		time.Sleep(20 * time.Millisecond)
		f, err = os.Open(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweeper still holds the jobs lock while its message waits")
		}
	}
}
