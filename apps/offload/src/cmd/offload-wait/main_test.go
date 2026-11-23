package main

// Each case runs this test binary as offload-wait, in a process of its own as the launcher
// runs it: TestMain sees waitEnv, replaces the steps the case's other variables name, and
// runs the wait's main on its arguments.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const waitEnv = "OFFLOAD_WAIT_UNDER_TEST"

func TestMain(m *testing.M) {
	if os.Getenv(waitEnv) == "" {
		os.Exit(m.Run())
	}
	report := replaceSteps()
	code := run(os.Args[1:])
	report()
	os.Exit(code)
}

// replaceSteps replaces the steps the case names in its environment, and returns what
// prints the counts it asked for on stderr once the wait has run.
func replaceSteps() func() {
	var report []func()
	byName := func(name string) syscall.Signal {
		if name == "PIPE" {
			return syscall.SIGPIPE
		}
		if s, ok := ending[name]; ok {
			return s
		}
		return informational[name]
	}
	// The runtime may handle a signal on another thread after its sender goes on, so a
	// replaced step waits for the handler: the signal lands in that step, not after it.
	raise := func(name string) {
		_ = syscall.Kill(os.Getpid(), byName(name))
		time.Sleep(50 * time.Millisecond)
	}
	switch v := os.Getenv("OW_SYNCFS"); v {
	case "EIO":
		syncfs = func(int) syscall.Errno { return syscall.EIO }
	case "count":
		calls, real := 0, syncfs
		syncfs = func(fd int) syscall.Errno { calls++; return real(fd) }
		report = append(report, func() { fmt.Fprintln(os.Stderr, calls) })
	}
	if v := os.Getenv("OW_FLUSH_EVERY"); v != "" {
		flushEvery, _ = time.ParseDuration(v)
		var calls int64
		real := fsync
		fsync = func(fd int) error { calls++; return real(fd) }
		report = append(report, func() { fmt.Fprintln(os.Stderr, calls) })
	}
	if v := os.Getenv("OW_CHDIR_RAISES"); v != "" {
		real := chdir
		chdir = func(p string) error { raise(v); return real(p) }
	}
	if v := os.Getenv("OW_WATCH_RAISES"); v != "" {
		real := watchPaths
		watchPaths = func(p []string) ([]watchedFS, error) { raise(v); return real(p) }
	}
	if v := os.Getenv("OW_FORWARDED_RAISES"); v != "" {
		readForwarded = func() []syscall.Signal { raise(v); return nil }
	}
	if v := os.Getenv("OW_START_FAILS"); v != "" {
		startJob = func(*exec.Cmd) error {
			_ = os.WriteFile(v, nil, 0o600)
			return &os.PathError{Op: "start", Path: "job", Err: syscall.EPERM}
		}
	}
	if v := os.Getenv("OW_START_RAISES"); v != "" {
		startJob = func(c *exec.Cmd) error {
			err := c.Start()
			for _, n := range strings.Split(v, ",") {
				raise(n)
			}
			return err
		}
	}
	if v := os.Getenv("OW_SENT"); v != "" {
		real := sendSignal
		sendSignal = func(p *os.Process, s syscall.Signal) {
			f, _ := os.OpenFile(v, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			fmt.Fprintf(f, "%d ", s)
			f.Close()
			real(p, s)
		}
	}
	return func() {
		for _, r := range report {
			r()
		}
	}
}

// waitArgs is the wait's command line: cwd, writable, private, plan, then the job.
func waitArgs(cwd, writable, private, plan string, job ...string) []string {
	return append([]string{cwd, writable, private, plan, "--"}, job...)
}

// waitCmd is the wait on args, with env (`OW_…=…`) naming the steps it replaces and each
// file in fds open on its descriptor.
func waitCmd(t *testing.T, env []string, fds map[int]*os.File, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append(os.Environ(), waitEnv+"=1"), env...)
	for fd, f := range fds {
		for len(cmd.ExtraFiles) <= fd-3 {
			cmd.ExtraFiles = append(cmd.ExtraFiles, nil)
		}
		cmd.ExtraFiles[fd-3] = f
	}
	return cmd
}

// wait runs the wait and returns its exit code, stdout and stderr.
func wait(t *testing.T, env []string, fds map[int]*os.File, args ...string) (int, string, string) {
	t.Helper()
	cmd := waitCmd(t, env, fds, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return exitStatus(t, cmd.Run()), stdout.String(), stderr.String()
}

// waitRun is the wait with cwd and writable, no private directories and no plan.
func waitRun(t *testing.T, env []string, cwd, writable string, job ...string) (int, string, string) {
	t.Helper()
	return wait(t, env, nil, waitArgs(cwd, writable, "", "", job...)...)
}

// waitRunStdio is the wait with plan as the STDIO plan and offsetFD open on a file, whose
// content it also returns.
func waitRunStdio(t *testing.T, cwd, plan string, job ...string) (code int, stdout, stderr, offsets string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "offsets")
	f := create(t, record)
	code, stdout, stderr = wait(t, nil, map[int]*os.File{offsetFD: f}, waitArgs(cwd, cwd, "", plan, job...)...)
	b, _ := os.ReadFile(record)
	return code, stdout, stderr, string(b)
}

// create is a new file at path, open for writing, closed with the test.
func create(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// record is a file at path holding content, open for reading, closed with the test.
func record(t *testing.T, path, content string) *os.File {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatal(err)
	return 0
}

// needNamespaces skips the test, saying why, where `unshare <flags>` cannot run probe: an
// unprivileged user namespace is refused on some hosts (Ubuntu's AppArmor default).
func needNamespaces(t *testing.T, flags, probe string) {
	t.Helper()
	if out, err := exec.Command("unshare", flags, "sh", "-c", probe).CombinedOutput(); err != nil {
		t.Skipf("needs `unshare %s` able to run %q: %v %s", flags, probe, err, out)
	}
}

// inNamespace is cmd run as the init of a pid namespace of its own, as the launcher runs
// the wait.
func inNamespace(cmd *exec.Cmd, flags ...string) *exec.Cmd {
	ns := exec.Command("unshare", append(append([]string{"-rpf", "--mount-proc"}, flags...), cmd.Args...)...)
	ns.Env, ns.ExtraFiles = cmd.Env, cmd.ExtraFiles
	return ns
}

// slowReader is a shell pipeline's tail that takes 4 KiB every 2 ms and prints the count.
const slowReader = `python3 -c 'import sys, time
n = 0
while b := sys.stdin.buffer.read1(4096):
    n += len(b); time.sleep(0.002)
print(n)'`

// An output file in the plan is the job's own stream: written from the caller's offset
// (over what follows it, as a local run's would be, or at the end when appending), never
// relayed, and offload-wait reports where the job left it.
func TestWaitGivesTheJobItsOutputFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out log") // a path with a space: the plan's path is the line's rest
	for _, c := range []struct {
		name, plan, script, want, offsets string
	}{
		{"overwrite at the offset", "out file w 3 " + f, "printf XY", "abcXYfgh", "out 5\n"},
		{"append", "out file a 2 " + f, "printf XY", "abcdefghXY", "out 10\n"},
		{"nothing written keeps the offset", "out file a 2 " + f, "true", "abcdefgh", "out 2\n"},
		{"a read-write open reads back", "out file rw 0 " + f, `exec 3>&1; printf XY; v=$(head -c 3 <&3); printf "$v"`, "XYcdecde", "out 8\n"},
		{"stderr too", "err file w 0 " + f, "printf XY >&2", "XYcdefgh", "err 2\n"},
		{"merged stderr writes stdout's file", "err merged\nout file w 0 " + f, "printf A; printf B >&2; printf C", "ABCdefgh", "out 3\n"},
	} {
		if err := os.WriteFile(f, []byte("abcdefgh"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr, offsets := waitRunStdio(t, dir, c.plan, "sh", "-c", c.script)
		got, _ := os.ReadFile(f)
		if code != 0 || stdout != "" || stderr != "" || string(got) != c.want || offsets != c.offsets {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, file %q, offsets %q; want 0, nothing relayed, %q and %q",
				c.name, code, stdout, stderr, got, offsets, c.want, c.offsets)
		}
	}
}

// A failed write of the offsets record is reported and leaves the job's code, whether the
// job ran or offload-wait failed before it: the wrapper then leaves the descriptor at the end.
func TestWaitKeepsTheJobsCodeWhenItsOffsetsCannotBeRecorded(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	for _, c := range []struct {
		plan string
		code int
	}{{"out file w 0 " + f, 3}, {"out file w 0 " + filepath.Join(dir, "missing", "f"), 125}} {
		code, _, stderr := wait(t, nil, map[int]*os.File{offsetFD: full}, waitArgs(dir, dir, "", c.plan, "sh", "-c", "exit 3")...)
		if code != c.code || !strings.Contains(stderr, "recording where the job left its output failed: No space left on device") {
			t.Errorf("plan %q: exit %d, stderr %q; want %d and a message", c.plan, code, stderr, c.code)
		}
	}
}

// The other stream is still relayed beside an output file.
func TestWaitRelaysTheStreamThatIsNoFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr, _ := waitRunStdio(t, dir, "out file w 0 "+f, "sh", "-c", "echo to-file; echo relayed >&2")
	got, _ := os.ReadFile(f)
	if code != 0 || stdout != "" || stderr != "relayed\n" || string(got) != "to-file\n" {
		t.Errorf("exit %d, stdout %q, stderr %q, file %q", code, stdout, stderr, got)
	}
}

// An output file that will not open, or a plan line offload-wait cannot read, fails the
// run before the job, loudly: the job's output would otherwise go nowhere. The offsets it
// reports are the plan's, where a job that never ran leaves the caller's descriptor.
func TestWaitFailsBeforeTheJobWhenItsOutputFileWillNotOpen(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	missing := filepath.Join(dir, "missing", "f")
	for _, c := range []struct{ plan, message, offsets string }{
		{"out file rw 4 " + missing, missing + ": No such file or directory", "out 4\n"},
		{"out pipe", "out pipe: not a stdio plan line", ""},
	} {
		code, _, stderr, offsets := waitRunStdio(t, dir, c.plan, "touch", marker)
		if want := "offload-launch: " + c.message + "; the job did not run\n"; code != 125 || stderr != want || offsets != c.offsets {
			t.Errorf("plan %q: exit %d, stderr %q, offsets %q; want 125, %q and %q", c.plan, code, stderr, offsets, want, c.offsets)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("plan %q: the job ran", c.plan)
		}
	}
}

// A run a recorded signal ends before the job still reports the plan's offsets, so the
// wrapper leaves the caller's descriptor where a job that never ran leaves it.
func TestWaitReportsThePlansOffsetsWhenASignalEndsTheRunBeforeTheJob(t *testing.T) {
	dir := t.TempDir()
	offsets := filepath.Join(dir, "offsets")
	fds := map[int]*os.File{abortFD: record(t, filepath.Join(dir, "abort-sig"), "INT\n"), offsetFD: create(t, offsets)}
	code, _, errOut := wait(t, nil, fds, waitArgs(dir, dir, "", "out file w 7 "+filepath.Join(dir, "log"), "true")...)
	if got, _ := os.ReadFile(offsets); code != 130 || string(got) != "out 7\n" {
		t.Errorf("exit %d (stderr %q), offsets %q; want 130 and \"out 7\\n\"", code, errOut, got)
	}
}

// What a job leaves in a directory that ends with it on the remote (/dev/shm, /dev/mqueue)
// a local run would have kept, so it fails the run, naming each file; a job that already
// failed keeps its code. An empty or missing one is fine, as is podman's lock table; one it
// cannot list fails too.
func TestWaitFailsAJobThatLeavesFilesInAPrivateDirectory(t *testing.T) {
	dir, shm, mq := t.TempDir(), t.TempDir(), t.TempDir()
	private := shm + "\n" + mq + "\n" + filepath.Join(dir, "absent")
	run := func(job ...string) (int, string) {
		code, _, errOut := wait(t, nil, nil, waitArgs(dir, dir, private, "", job...)...)
		return code, errOut
	}
	if code, errOut := run("true"); code != 0 || errOut != "" {
		t.Errorf("nothing left: exit %d, stderr %q, want a silent 0", code, errOut)
	}
	if code, errOut := run("touch", shm+"/libpod_rootless_lock_1000"); code != 0 || errOut != "" {
		t.Errorf("podman's lock table left: exit %d, stderr %q, want a silent 0", code, errOut)
	}
	if code, errOut := run("touch", shm+"/libpod_rootless_lock_1000.keep"); code != 1 || !strings.Contains(errOut, "libpod_rootless_lock_1000.keep") {
		t.Errorf("a file named past podman's lock table: exit %d, stderr %q, want 1 naming it", code, errOut)
	}
	os.Remove(shm + "/libpod_rootless_lock_1000.keep")
	code, errOut := run("touch", shm+"/b", shm+"/a", shm+"/libpod_rootless_lock_1000")
	want := "offload: the job left files in a directory private to the remote, deleted as the job ends: " + shm + ": a, b; run it with --where local to keep them\n"
	if code != 1 || errOut != want {
		t.Errorf("files left: exit %d, stderr %q, want 1 and %q", code, errOut, want)
	}
	os.Remove(shm + "/a")
	if code, errOut := run("sh", "-c", "exit 4"); code != 4 || !strings.Contains(errOut, shm+": b") {
		t.Errorf("a failed job with a file left: exit %d, stderr %q, want its own 4 and the message", code, errOut)
	}
	os.Remove(shm + "/b")
	if err := os.Chmod(mq, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(mq, 0o755) })
	if code, errOut := run("true"); code != 1 || !strings.Contains(errOut, mq+": cannot be listed (Permission denied)") {
		t.Errorf("an unlistable directory: exit %d, stderr %q, want 1 and a message", code, errOut)
	}
}

// A caller's stdout set non-blocking upstream fills as any pipe does: the relay waits for
// its reader, as a local job's write would block, and passes every byte on.
func TestWaitRelaysOntoANonBlockingStdoutWhoseReaderLags(t *testing.T) {
	dir := t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := syscall.SetNonblock(int(w.Fd()), true); err != nil { // shared with the wait's copy
		t.Fatal(err)
	}
	cmd := waitCmd(t, nil, nil, waitArgs(dir, dir, "", "", "head", "-c", "1000000", "/dev/zero")...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	time.Sleep(300 * time.Millisecond) // the pipe fills meanwhile
	n, _ := io.Copy(io.Discard, r)
	if code := exitStatus(t, cmd.Wait()); code != 0 || n != 1000000 {
		t.Errorf("exit %d, %d bytes relayed, stderr %q; want 0 and 1000000", code, n, stderr.String())
	}
}

// With the caller's reader of stderr gone, the notices offload-wait ends on go nowhere and
// the job's own code stands.
func TestWaitKeepsTheJobsCodeWhenItsNoticesHaveNoReader(t *testing.T) {
	dir, shm := t.TempDir(), t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	cmd := waitCmd(t, nil, nil, waitArgs(dir, dir, shm, "", "sh", "-c", "touch "+shm+"/a; exit 3")...)
	cmd.Stderr = w
	if code := exitStatus(t, cmd.Run()); code != 3 {
		t.Errorf("exit %d, want the job's 3", code)
	}
}

func TestWaitPassesTheJobsExitCodeOn(t *testing.T) {
	dir := t.TempDir()
	for script, want := range map[string]int{"exit 0": 0, "exit 3": 3, "kill -9 $$": 137, "kill -TERM $$": 143} {
		if code, _, errOut := waitRun(t, nil, dir, dir, "sh", "-c", script); code != want {
			t.Errorf("%q: exit %d (%s), want %d", script, code, errOut, want)
		}
	}
}

// The job runs in cwd with its standard files alone: none of the launcher's control channel
// (fds 9 to 13 here) and none of the wait's own.
func TestWaitRunsTheJobInCwdWithOnlyItsStandardFiles(t *testing.T) {
	dir := t.TempDir()
	fds := map[int]*os.File{abortFD: record(t, filepath.Join(dir, "abort-sig"), ""), startedFD: create(t, filepath.Join(dir, "started"))}
	// Straight to stdout: through a pipe ls could list the shell's ends of it, not yet
	// closed, and a redirect has the shell save its stdout on fd 10.
	code, out, errOut := wait(t, nil, fds, waitArgs(dir, dir, "", "", "sh", "-c", "pwd; ls /proc/$$/fd")...)
	if code != 0 || out != dir+"\n0\n1\n2\n" {
		t.Errorf("exit %d, stdout %q (%s), want %q", code, out, errOut, dir+"\n0\n1\n2\n")
	}
}

// A write error NFS reports only at writeback fails the run, naming the paths, and a job
// that already failed keeps its own code. The error is the seam: syncfs is replaced.
func TestWaitTurnsAWritebackErrorIntoAFailure(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	eio := []string{"OW_SYNCFS=EIO"}
	code, _, errOut := waitRun(t, eio, dir, dir+"\n"+dir+"/missing", "true")
	want := "offload: writing to dev failed under " + dir + ": Input/output error; files the job wrote there may be incomplete\n"
	if code != 1 || errOut != want {
		t.Errorf("exit %d, stderr %q: want 1 and %q", code, errOut, want)
	}
	if code, _, errOut := waitRun(t, eio, dir, dir+"\n"+other, "sh", "-c", "exit 3"); code != 3 || !strings.Contains(errOut, "may be incomplete") {
		t.Errorf("exit %d, stderr %q: want the job's own 3 and the message", code, errOut)
	}
}

// syncfs reports only errors met after its descriptor was opened, so the descriptor is open
// before the job starts: the job itself finds it among its parent's, or exits 9. The real
// syncfs then succeeds on a healthy disk, once for the one filesystem.
func TestWaitOpensItsDescriptorsBeforeTheJobAndSyncsEachFilesystemOnce(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	const parentHolds = `for f in /proc/$PPID/fd/*; do [ "$(readlink "$f")" = "$1" ] && exit 0; done; exit 9`
	code, _, errOut := waitRun(t, []string{"OW_SYNCFS=count"}, dir, dir+"\n"+dir+"/sub", "sh", "-c", parentHolds, "sh", dir)
	if code != 0 || strings.TrimSpace(errOut) != "1" {
		t.Errorf("exit %d, stderr %q: want 0 (9: no descriptor on %s while the job ran) and one syncfs for one filesystem", code, errOut, dir)
	}
}

// The seam the writeback tests replace calls the real syncfs: an error comes back as its
// errno, success as 0.
func TestWaitsSyncfsReportsTheKernelsErrno(t *testing.T) {
	d, err := os.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if bad, ok := realSyncfs(-1), realSyncfs(int(d.Fd())); bad != syscall.EBADF || ok != 0 {
		t.Errorf("syncfs(-1) = %v, syncfs(.) = %v; want EBADF and 0", bad, ok)
	}
}

// An output file is flushed while the job runs, not only once it ends, so a reader on dev
// follows it as it grows.
func TestWaitFlushesAnOutputFileWhileTheJobRuns(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fds := map[int]*os.File{offsetFD: create(t, filepath.Join(t.TempDir(), "offsets"))}
	code, _, errOut := wait(t, []string{"OW_FLUSH_EVERY=50ms"}, fds, waitArgs(dir, dir, "", "out file w 0 "+f, "sh", "-c", "echo a; sleep 0.5")...)
	n, err := strconv.Atoi(strings.TrimSpace(errOut))
	if code != 0 || err != nil || n < 2 {
		t.Errorf("exit %d, stderr %q; want 0 and the file flushed several times while the job ran", code, errOut)
	}
}

// A socket among the writable paths cannot be opened (ENXIO); another path on its
// filesystem serves for the syncfs, and the job runs.
func TestWaitWatchesASocketsFilesystemThroughAnotherPath(t *testing.T) {
	dir, err := os.MkdirTemp("", "ow") // short: a socket's path is at most 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if code, _, errOut := waitRun(t, nil, dir, dir+"/s\n"+dir, "true"); code != 0 || errOut != "" {
		t.Errorf("exit %d, stderr %q; want a silent 0", code, errOut)
	}
}

func TestWaitRefusesToStartWhenItCannotEnterCwdOrWatchAPath(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	gone := filepath.Join(dir, "gone")
	for name, c := range map[string][3]string{
		"missing cwd":          {gone, dir, gone + ": No such file or directory"},
		"unreadable writable":  {dir, locked, locked + ": Is a directory"}, // the write-only open's error
		"cwd the user may not": {locked, dir, locked + ": Permission denied"},
	} {
		if os.Geteuid() == 0 && name != "missing cwd" {
			continue // root enters and reads every directory
		}
		marker := filepath.Join(t.TempDir(), "ran") // one per case: a run in one case fails only that one
		code, _, errOut := waitRun(t, nil, c[0], c[1], "touch", marker)
		if want := "offload-launch: " + c[2] + "; the job did not run\n"; code != 125 || errOut != want {
			t.Errorf("%s: exit %d, stderr %q, want 125 and %q", name, code, errOut, want)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: the job ran", name)
		}
	}
}

// A job that is no program fails the run before it, as a step of setup does.
func TestWaitRefusesAJobItCannotStart(t *testing.T) {
	dir := t.TempDir()
	for job, reason := range map[string]string{"no-such-program": "No such file or directory", dir: "Permission denied"} {
		code, _, errOut := waitRun(t, nil, dir, dir, job)
		if want := "offload-launch: " + job + ": " + reason + "; the job did not run\n"; code != 125 || errOut != want {
			t.Errorf("job %q: exit %d, stderr %q, want 125 and %q", job, code, errOut, want)
		}
	}
}

// A signal sent to the job's process group (the launcher forwards INT, TERM and HUP there)
// reaches the job, while the parent stays to flush and report the job's own exit.
func TestWaitOutlivesASignalToTheGroupAndReportsTheJobsExit(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		os.Remove(ready)
		cmd := waitCmd(t, nil, nil, waitArgs(dir, dir, "", "",
			"sh", "-c", `trap 'exit 7' INT TERM HUP; touch `+ready+`; while :; do sleep 0.05; done`)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		syscall.Kill(-cmd.Process.Pid, sig)
		if code := exitStatus(t, cmd.Wait()); code != 7 {
			t.Errorf("%v to the group: exit %d, want the job's 7", sig, code)
		}
	}
}

// A writable path under a file cannot exist, so it is skipped, as offload-bind skips it,
// rather than failing the job before it runs.
func TestWaitSkipsAWritablePathUnderAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := waitRun(t, nil, dir, dir+"\n"+dir+"/file/x", "true"); code != 0 || errOut != "" {
		t.Errorf("exit %d, stderr %q, want a silent 0", code, errOut)
	}
}

// A writable file the job may write but not read (mode 0200) still has its filesystem
// synced, rather than failing the job before it runs. (A directory it cannot read opens
// neither way, so it still fails loudly, unless another path of its filesystem opens.)
func TestWaitWatchesAWritableFileItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o200); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := waitRun(t, nil, dir, file, "true"); code != 0 || errOut != "" {
		t.Errorf("exit %d, stderr %q, want a silent 0", code, errOut)
	}
}

// As the init of the job's pid namespace, offload-wait names each process the job left
// running, which dies with it where a local run would have kept it, and keeps the exit
// code; with none left it says nothing.
func TestWaitNamesTheProcessesTheJobLeftRunning(t *testing.T) {
	needNamespaces(t, "-rpf", "true")
	dir := t.TempDir()
	run := func(script string) (int, string) {
		cmd := inNamespace(waitCmd(t, nil, nil, waitArgs(dir, dir, "", "", "sh", "-c", script)...))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		return exitStatus(t, cmd.Run()), stderr.String()
	}
	if code, errOut := run("sleep 0.1; exit 0"); code != 0 || errOut != "" {
		t.Errorf("nothing left: exit %d, stderr %q, want a silent 0", code, errOut)
	}
	// The pause lets the background shell exec sleep, so the name is the program's, not sh.
	code, errOut := run("sleep 60 & sleep 0.3; exit 3")
	const notice = "offload: the job left processes running, stopped as it ends (a local run would have kept them): "
	if code != 3 || !strings.HasPrefix(errOut, notice) || !strings.HasSuffix(errOut, " sleep\n") {
		t.Errorf("a sleep left: exit %d, stderr %q, want the job's 3 and the sleep named", code, errOut)
	}
}

// A signal the launcher forwards before the job starts (offload-wait is still watching the
// writable paths or entering the cwd over NFS) ends the run as that signal, the job unrun;
// one that arrives while the job is being forked is passed on to it.
func TestWaitHonoursASignalThatArrivesBeforeTheJob(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	code, _, errOut := waitRun(t, []string{"OW_CHDIR_RAISES=INT", "OW_START_FAILS=" + marker}, dir, dir, "true")
	if code != 130 {
		t.Errorf("exit %d (stderr %q), want 130", code, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the job ran after the signal")
	}
	start := time.Now()
	code, _, errOut = waitRun(t, []string{"OW_START_RAISES=TERM"}, dir, dir, "sleep", "5")
	if code != 143 || time.Since(start) > 4*time.Second {
		t.Errorf("a signal during the fork: exit %d after %s (stderr %q), want 143 at once", code, time.Since(start), errOut)
	}
}

// SIGUSR1 and SIGUSR2 inform a job (dd's progress); one that lands before the job exists
// informs no one and must not end the run, and each signal that lands while the job is
// forked reaches the job, not only the first.
func TestWaitDropsAnInformationalSignalBeforeTheJobAndPassesOnEachDuringTheFork(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	code, _, errOut := waitRun(t, []string{"OW_CHDIR_RAISES=USR1"}, dir, dir, "touch", marker)
	if code != 0 {
		t.Errorf("a USR1 before the job: exit %d (stderr %q), want 0", code, errOut)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("a USR1 before the job kept it from running")
	}
	// A job still starting has default handlers, so the passing on is what is recorded.
	sent := filepath.Join(dir, "sent")
	waitRun(t, []string{"OW_START_RAISES=USR1,TERM", "OW_SENT=" + sent}, dir, dir, "sleep", "5")
	if b, _ := os.ReadFile(sent); string(b) != "10 15 " {
		t.Errorf("USR1 then TERM during the fork passed on %q, want both (10 15)", b)
	}
}

// A signal the launcher forwarded before offload-wait's handlers existed was discarded (an
// init ignores what it does not handle); the launcher records each on abortFD first, and
// offload-wait ends the run by the first that ends one, with the job unrun.
func TestWaitEndsTheRunByASignalTheLauncherRecorded(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	abort := filepath.Join(dir, "abort-sig")
	fds := map[int]*os.File{abortFD: record(t, abort, "USR1\nHUP\nTERM\n")}
	code, _, errOut := wait(t, nil, fds, waitArgs(dir, dir, "", "", "touch", marker)...)
	if code != 129 {
		t.Errorf("exit %d (stderr %q), want 129 (HUP)", code, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the job ran after a recorded signal")
	}
	fds = map[int]*os.File{abortFD: record(t, abort, "USR2\n")}
	if code, _, errOut := wait(t, nil, fds, waitArgs(dir, dir, "", "", "touch", marker)...); code != 0 {
		t.Errorf("only USR2 recorded: exit %d (stderr %q), want 0", code, errOut)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("a recorded USR2 kept the job from running")
	}
}

// The launcher records each ending signal before it sends it, so its record holds them in
// the order they were sent: a later signal caught here does not decide the run's end.
func TestWaitEndsTheRunByTheFirstSignalTheLauncherSent(t *testing.T) {
	dir := t.TempDir()
	fds := map[int]*os.File{abortFD: record(t, filepath.Join(dir, "abort-sig"), "TERM\nINT\n")}
	if code, _, errOut := wait(t, []string{"OW_WATCH_RAISES=INT"}, fds, waitArgs(dir, dir, "", "", "true")...); code != 143 {
		t.Errorf("exit %d (stderr %q), want 143 (TERM, sent first)", code, errOut)
	}
}

// A signal that lands after offload-wait judged the ones before the job, but before the
// fork, is passed on to the job rather than lost.
func TestWaitPassesOnASignalThatLandsAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	code, _, errOut := waitRun(t, []string{"OW_FORWARDED_RAISES=TERM"}, dir, dir, "sleep", "5")
	if code != 143 || time.Since(start) > 4*time.Second {
		t.Errorf("exit %d after %s (stderr %q), want 143 at once", code, time.Since(start), errOut)
	}
}

// The launcher signals the job's process group; a job that leaves it (`timeout`,
// `setsid`) gets the signal from offload-wait instead, and one still in it gets it once.
func TestWaitPassesASignalOnToAJobThatLeftItsGroup(t *testing.T) {
	dir := t.TempDir()
	// The wait runs in a group of its own, as it runs in the launcher's.
	run := func(job ...string) (int, time.Duration, string) {
		cmd := waitCmd(t, nil, nil, waitArgs(dir, dir, "", "", job...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		start := time.Now()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(500*time.Millisecond, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) })
		code := exitStatus(t, cmd.Wait())
		return code, time.Since(start), stderr.String()
	}
	if code, took, errOut := run("setsid", "sleep", "5"); code != 143 || took > 4*time.Second {
		t.Errorf("a job in its own group: exit %d after %s (stderr %q), want 143 at once", code, took, errOut)
	}
	count := filepath.Join(dir, "count")
	if code, took, errOut := run("sh", "-c", "trap 'echo x >>"+count+"' TERM; sleep 2 & wait; sleep 1"); code != 0 || took > 4*time.Second {
		t.Errorf("a job in the group: exit %d after %s (stderr %q), want 0", code, took, errOut)
	}
	if b, _ := os.ReadFile(count); string(b) != "x\n" {
		t.Errorf("a job in the group got the signal %q times, want once", b)
	}
}

// offload-wait cannot die by a signal itself (it is the namespace's init), so it writes
// the signal a job died by to signalFD for the launcher's `status signal`; a job that
// exits, even with 128+n, writes nothing.
func TestWaitReportsTheSignalTheJobDiedBy(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "signal")
	run := func(script string) (int, string, string) {
		return wait(t, nil, map[int]*os.File{signalFD: create(t, report)}, waitArgs(dir, dir, "", "", "sh", "-c", script)...)
	}
	if code, _, errOut := run("kill -TERM $$"); code != 143 {
		t.Errorf("exit %d (stderr %q), want 143", code, errOut)
	}
	if b, _ := os.ReadFile(report); string(b) != "15" {
		t.Errorf("reported %q, want 15", b)
	}
	if code, _, errOut := run("exit 143"); code != 143 {
		t.Errorf("exit %d (stderr %q), want 143", code, errOut)
	}
	if b, _ := os.ReadFile(report); len(b) != 0 {
		t.Errorf("a job that exited 143 reported signal %q", b)
	}
	// Without the descriptor (run outside the launcher) the code is the same.
	if code, _, errOut := waitRun(t, nil, dir, dir, "sh", "-c", "kill -TERM $$"); code != 143 || errOut != "" {
		t.Errorf("without signalFD: exit %d, stderr %q, want a silent 143", code, errOut)
	}
}

// namespacedWait is offload-wait as the launcher runs it: the init of a pid namespace of
// its own and, through setsid, its process group's leader, so a signal to the group reaches
// it and the job but not unshare. dir/abort-sig, empty, is its abortFD: the launcher's
// record of the ending signals it forwarded, appended to before it sends one.
func namespacedWait(t *testing.T, dir string, job ...string) *exec.Cmd {
	t.Helper()
	fds := map[int]*os.File{abortFD: record(t, filepath.Join(dir, "abort-sig"), "")}
	// --kill-child: a test that kills unshare ends the namespace, not just its parent.
	return inNamespace(waitCmd(t, nil, fds, waitArgs(dir, dir, "", "", job...)...), "--kill-child", "setsid")
}

// runWithin runs cmd and fails the test when it takes longer than limit.
func runWithin(t *testing.T, cmd *exec.Cmd, limit time.Duration) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(limit, func() { cmd.Process.Kill() })
	err := cmd.Wait()
	if !timer.Stop() {
		t.Fatalf("still running after %s (stderr %q)", limit, stderr.String())
	}
	return exitStatus(t, err), stderr.String()
}

// A signal that ends the run reaches the job's children as well as the job: when the job
// dies first, offload-wait lets the children finish their handlers, as a local run does,
// rather than ending the namespace under them.
func TestWaitLetsTheChildrenFinishTheirHandlersAfterTheJobDies(t *testing.T) {
	needNamespaces(t, "-rpf", "true")
	dir := t.TempDir()
	child := `import signal, sys, time
def bye(*a):
    time.sleep(0.5)
    open(sys.argv[1], "w").write("handled")
    sys.exit(0)
signal.signal(signal.SIGINT, bye)
open(sys.argv[1] + ".up", "w").write("up")
time.sleep(60)`
	script := fmt.Sprintf(`python3 -c '%s' %s & wait`, child, filepath.Join(dir, "h"))
	cmd := namespacedWait(t, dir, "sh", "-c", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "h.up")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", cmd.Process.Pid, cmd.Process.Pid))
	waitPid, convErr := strconv.Atoi(strings.TrimSpace(string(children)))
	if err != nil || convErr != nil {
		cmd.Process.Kill()
		t.Fatalf("offload-wait's pid: %q, %v", children, err)
	}
	// As the launcher forwards a Ctrl-C: recorded, then sent to the job's group.
	if err := os.WriteFile(filepath.Join(dir, "abort-sig"), []byte("INT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(-waitPid, syscall.SIGINT)
	err = cmd.Wait()
	if b, _ := os.ReadFile(filepath.Join(dir, "h")); string(b) != "handled" {
		t.Errorf("the child's handler did not finish (exit %d, stderr %q)", exitStatus(t, err), stderr.String())
	}
	if strings.Contains(stderr.String(), "left processes running") {
		t.Errorf("a child finishing its handler was reported as left running: %q", stderr.String())
	}
}

// A process the job left to the init that has ended is collected, as a local init
// collects it: a zombie would pass `kill -0` for good, and a job waiting for it to go
// (a stop script, a test harness) would never end.
func TestWaitCollectsAProcessTheJobLeftOnceItEnds(t *testing.T) {
	needNamespaces(t, "-rpf", "true")
	dir := t.TempDir()
	pf := filepath.Join(dir, "pf")
	cmd := namespacedWait(t, dir, "sh", "-c",
		`(sleep 100 & echo $! >"$1"); kill "$(cat "$1")"; while kill -0 "$(cat "$1")" 2>/dev/null; do sleep 0.1; done`, "sh", pf)
	if code, errOut := runWithin(t, cmd, 10*time.Second); code != 0 {
		t.Errorf("exit %d (stderr %q), want 0", code, errOut)
	}
}

// A signal the job sends its own group reaches offload-wait too, but starts no grace kill
// on the remote, so it must not hold the run open until every process the job left ends:
// one that never does (a daemon) would hold it for good, where a local run ends at once.
func TestASignalTheJobSendsItsGroupDoesNotWaitForItsLeftovers(t *testing.T) {
	needNamespaces(t, "-rpf", "true")
	dir := t.TempDir()
	cmd := namespacedWait(t, dir, "sh", "-c", `trap "" TERM; setsid sleep 60 & sleep 0.3; kill -TERM 0`)
	if code, errOut := runWithin(t, cmd, 10*time.Second); code != 0 {
		t.Errorf("exit %d (stderr %q), want 0", code, errOut)
	}
}

// eowRun runs offload-wait with each stream in streams recorded on eowFD, as the launcher
// records a stream whose reader the wrapper lost, and readerGoneSignal sent to it 0.2 s in,
// as the launcher sends it. stdout and stderr are one pipe when merged. It returns the exit
// code and what reached the caller.
func eowRun(t *testing.T, dir, streams string, merged bool, script string) (int, string) {
	t.Helper()
	// Empty as the job starts, so only the signal that follows the record closes the
	// stream, as when a reader goes mid-run.
	eow := filepath.Join(dir, "eow")
	plan := ""
	if merged {
		plan = "err merged" // the wrapper's plan when the caller's stdout and stderr are one file
	}
	cmd := waitCmd(t, nil, map[int]*os.File{eowFD: record(t, eow, "")}, waitArgs(dir, dir, "", plan, "sh", "-c", script)...)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	// Started with SIGPIPE ignored, as the launcher's job half starts it.
	cmd.Args = append([]string{"/bin/sh", "-c", `trap '' PIPE; exec "$0" "$@"`, cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so a stuck run is killed whole
	cmd.Stdout = w
	if merged {
		cmd.Stderr = w
	} else {
		cmd.Stderr = &errBuf
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	lost := time.AfterFunc(200*time.Millisecond, func() {
		os.WriteFile(eow, []byte(streams), 0o600)
		cmd.Process.Signal(readerGoneSignal)
	})
	defer lost.Stop()
	stuck := time.AfterFunc(10*time.Second, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer stuck.Stop()
	b, _ := io.ReadAll(r)
	code := exitStatus(t, cmd.Wait())
	return code, string(b) + errBuf.String()
}

// The caller's reader of a stream gone, offload-wait closes its relay of that stream alone:
// the job's next write to it fails as it would have locally, nothing in the job is
// signalled, and the other stream still reaches the caller. With both streams one file,
// closing either closes both, as one reader gone would locally: the job's last write
// to stderr then fails too.
func TestWaitClosesTheStreamWhoseReaderIsGone(t *testing.T) {
	for _, c := range []struct {
		name, streams string
		merged        bool
		script, want  string
		code          int
	}{
		{"stdout gone", "out\n", false, `sleep 0.5; yes; echo cleanup >ran; echo still >&2`, "still\n", 0},
		{"stderr gone", "err\n", false, `sleep 0.5; yes >&2; echo cleanup >ran; echo still`, "still\n", 0},
		{"merged, stderr gone", "err\n", true, `sleep 0.5; yes; echo cleanup >ran; echo still >&2`, "", 141},
	} {
		dir := t.TempDir()
		start := time.Now()
		code, got := eowRun(t, dir, c.streams, c.merged, c.script)
		ran, _ := os.ReadFile(filepath.Join(dir, "ran"))
		if code != c.code || string(ran) != "cleanup\n" || got != c.want || time.Since(start) > 10*time.Second {
			t.Errorf("%s: exit %d after %s, ran %q, the caller got %q; want %d at once, the job's cleanup run and %q",
				c.name, code, time.Since(start), ran, truncate(got), c.code, c.want)
		}
	}
}

// A stream whose reader went before offload-wait's handler or relays existed is closed as
// the job starts, with no signal to say so: the launcher's went before there was one.
func TestWaitClosesAStreamWhoseReaderWentBeforeTheJob(t *testing.T) {
	dir := t.TempDir()
	cmd := waitCmd(t, nil, map[int]*os.File{eowFD: record(t, filepath.Join(dir, "eow"), "out\n")},
		waitArgs(dir, dir, "", "", "sh", "-c", "yes; echo cleanup >ran")...)
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stuck := time.AfterFunc(10*time.Second, func() { cmd.Process.Kill() })
	code := exitStatus(t, cmd.Wait())
	stuck.Stop()
	if ran, _ := os.ReadFile(filepath.Join(dir, "ran")); code != 0 || string(ran) != "cleanup\n" {
		t.Errorf("exit %d, ran %q; want 0 at once and the job's cleanup run", code, ran)
	}
}

// A job's output reaches the caller whole: the relay passes on what is left in its pipe
// after the job ends, before offload-wait reports.
func TestWaitRelaysAllOfTheJobsOutput(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := waitRun(t, nil, dir, dir, "sh", "-c", "head -c 3000000 /dev/zero; echo e >&2")
	if code != 0 || len(out) != 3000000 || errOut != "e\n" {
		t.Errorf("exit %d, %d bytes out, stderr %q; want 0, 3000000 bytes and e", code, len(out), errOut)
	}
}

// A caller that reads slowly still gets all of the job's output: the drain after the job
// ends waits for a full destination rather than taking it for the end. The reader takes
// 4 KiB every 2 ms, so the pipe to it is full when the job exits.
func TestWaitRelaysAllOfTheJobsOutputToASlowReader(t *testing.T) {
	dir := t.TempDir()
	w := waitCmd(t, nil, nil, waitArgs(dir, "", "", "", "head", "-c", "1000000", "/dev/zero")...)
	cmd := exec.Command("sh", append([]string{"-c", `"$@" | ` + slowReader, "sh"}, w.Args...)...)
	cmd.Dir, cmd.Env = dir, w.Env
	b, err := cmd.Output()
	if code := exitStatus(t, err); code != 0 || strings.TrimSpace(string(b)) != "1000000" {
		t.Errorf("exit %d, %s bytes read; want 0 and 1000000", code, strings.TrimSpace(string(b)))
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// A process the job left writing does not hold the run open: offload-wait stops it as the
// job ends, passes on what it wrote, however slowly the reader takes it, and names it.
func TestWaitEndsWithAStragglerStillWritingToASlowReader(t *testing.T) {
	needNamespaces(t, "-rpf", "true")
	dir := t.TempDir()
	inner := inNamespace(waitCmd(t, nil, nil, waitArgs(dir, dir, "", "", "sh", "-c", "yes & sleep 0.5; exit 3")...))
	sh := exec.Command("sh", append([]string{"-c", `"$@" | ` + slowReader, "sh"}, inner.Args...)...)
	sh.Env = inner.Env
	var stderr bytes.Buffer
	sh.Stderr = &stderr
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = sh.Output()
		done <- err
	}()
	select {
	case err := <-done:
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		if exitStatus(t, err) != 0 || n == 0 || !strings.Contains(stderr.String(), " yes") {
			t.Errorf("exit %v, %d bytes, stderr %q: want the yes output and the yes named", err, n, stderr.String())
		}
	case <-time.After(30 * time.Second):
		sh.Process.Kill()
		t.Fatal("the run did not end with the straggler still writing")
	}
}

// offload-wait records on startedFD that the job runs (1) and then ended (2), and nothing
// for a run that fails before it: the launcher reports an exit with no such record as the
// job not run.
func TestWaitRecordsThatTheJobStarted(t *testing.T) {
	for _, c := range []struct {
		name, cwd string
		code      int
		want      string
	}{
		{"runs", "", 3, "12"},
		{"cwd missing", "/nonexistent", 125, ""},
	} {
		dir := t.TempDir()
		cwd := dir
		if c.cwd != "" {
			cwd = c.cwd
		}
		started := filepath.Join(dir, "started")
		code, _, _ := wait(t, nil, map[int]*os.File{startedFD: create(t, started)}, waitArgs(cwd, dir, "", "", "sh", "-c", "exit 3")...)
		if b, _ := os.ReadFile(started); code != c.code || string(b) != c.want {
			t.Errorf("%s: exit %d, record %q; want %d and %q", c.name, code, b, c.code, c.want)
		}
	}
}
