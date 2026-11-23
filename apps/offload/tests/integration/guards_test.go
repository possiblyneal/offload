package integration

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sillyRename is the name the kernel gives a file deleted while open: .nfs plus 24 hex digits.
var sillyRename = regexp.MustCompile(`^\.nfs[0-9a-f]{24}$`)

func sillyFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if sillyRename.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// scopeOfPattern is the offload job scope holding the process the pattern matches.
func scopeOfPattern(t *testing.T, pattern string) string {
	t.Helper()
	unit := strings.TrimSpace(mustSh(t, true,
		`for p in $(pgrep -f '`+pattern+`'); do grep -o 'offload-job-[0-9]*\.scope' /proc/$p/cgroup; done | head -1`))
	if unit == "" {
		t.Fatalf("no job scope holds %q", pattern)
	}
	return unit
}

// A job killed with files open leaves silly-rename files on dev; the sweep removes those and
// nothing else, so a dotfile that merely starts with .nfs survives.
func TestSweepRemovesSillyRenameFilesAndKeepsOtherNfsDotfiles(t *testing.T) {
	dir := repo(t)
	// The sweep leaves everything alone while any other job runs, as it should.
	waitForNoOtherJobs(t)
	for name, content := range map[string]string{".nfsrc": "keep\n", ".nfs00000000000000000000abcd": "x\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Dev's /tmp and /var/tmp are swept too, into their subdirectories: /tmp is a submount.
	var tmpSubs []string
	for _, root := range []string{"/tmp", "/var/tmp"} {
		sub, err := os.MkdirTemp(root, "offload-it-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sub) })
		if err := os.WriteFile(filepath.Join(sub, ".nfs00000000000000000000abce"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		tmpSubs = append(tmpSubs, sub)
	}
	killJobHoldingADeletedFile(t, offloadBin, dir)
	// The wrapper sweeps before it exits, so nothing is left once it has.
	if left := sillyFiles(dir); len(left) != 0 {
		t.Errorf("the sweep left %v in %s", left, dir)
	}
	for _, sub := range tmpSubs {
		if left := sillyFiles(sub); len(left) != 0 {
			t.Errorf("the sweep left %v in %s", left, sub)
		}
	}
	time.Sleep(2 * time.Second)
	if b, err := os.ReadFile(filepath.Join(dir, ".nfsrc")); err != nil || string(b) != "keep\n" {
		t.Errorf(".nfsrc did not survive the sweep: %q %v", b, err)
	}
}

// killJobHoldingADeletedFile runs a job that holds a deleted file open in dir, beside the
// silly-rename file planted there, then kills its scope and waits for the wrapper.
func killJobHoldingADeletedFile(t *testing.T, bin, dir string) {
	t.Helper()
	cmd := exec.Command(bin, "--where", "server", "--", "sh", "-c",
		"echo x > held && exec 3<held && rm held && echo ready && exec sleep 6174")
	cmd.Dir = dir
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	buf := make([]byte, 6)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "ready\n" {
		t.Fatalf("job did not start: %q %v", buf, err)
	}
	// The probe: the kernel's own silly-rename file stands beside the planted one.
	waitFor(t, func() bool { return len(sillyFiles(dir)) >= 2 }, 10*time.Second, "the job's open deleted file to show on dev")
	mustSh(t, true, "sudo systemctl kill --signal=SIGKILL "+scopeOfPattern(t, "[s]leep 6174"))
	if err := cmd.Wait(); err == nil {
		t.Error("a killed job exited 0")
	}
}

// jobsLock is dev's jobs lock, the same for every wrapper whatever its HOME.
const jobsLock = "/run/lock/offload-jobs.lock"

// flockJobsLock tries to take dev's jobs lock with how (syscall.LOCK_SH or LOCK_EX) at
// once, and returns the file holding it, or nil.
func flockJobsLock(t *testing.T, how int) *os.File {
	t.Helper()
	f, err := os.OpenFile(jobsLock, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB) != nil {
		f.Close()
		return nil
	}
	return f
}

// waitForNoOtherJobs waits until no wrapper on dev holds the jobs lock, so a sweep can run.
func waitForNoOtherJobs(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool {
		f := flockJobsLock(t, syscall.LOCK_EX)
		f.Close()
		return f != nil
	}, 120*time.Second, "other offload jobs on dev to end")
}

// Every wrapper whose job runs on a remote holds dev's jobs lock shared for its whole life,
// so a sweep, which takes it exclusively, never runs while another remote job runs.
func TestAWrapperHoldsTheJobsLockSharedWhileItsJobRuns(t *testing.T) {
	startSleeper(t)
	if f := flockJobsLock(t, syscall.LOCK_EX); f != nil {
		f.Close()
		t.Error("the jobs lock could be taken exclusively while a job ran")
	}
	f := flockJobsLock(t, syscall.LOCK_SH)
	if f == nil {
		t.Fatal("a second wrapper could not share the jobs lock")
	}
	f.Close()
}

// A wrapper that cannot take the lock exclusively at once leaves the sweep undone: the
// files may be held open by another wrapper's job.
func TestSweepIsSkippedWhileAnotherWrapperHoldsTheJobsLock(t *testing.T) {
	dir := repo(t)
	waitForNoOtherJobs(t)
	planted := filepath.Join(dir, ".nfs00000000000000000000abce")
	if err := os.WriteFile(planted, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := flockJobsLock(t, syscall.LOCK_SH)
	if other == nil {
		t.Fatal("could not share the jobs lock")
	}
	defer other.Close()
	killJobHoldingADeletedFile(t, offloadBin, dir)
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("the sweep ran while another wrapper held the jobs lock: %v", err)
	}
}

// sshd leaves a forward's socket behind when its connection ends. A launcher removes the
// ones in /tmp that nothing listens on, and keeps one that is listened on, whatever its age.
func TestLauncherRemovesForwardSocketsNobodyListensOn(t *testing.T) {
	const stale, live = "/tmp/offload-it000000-1.sock", "/tmp/offload-it000001-1.sock"
	t.Cleanup(func() { sh(t, true, "rm -f "+stale+" "+live) })
	old := "import os, socket, sys, time\n" +
		"s = socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); os.utime(sys.argv[1], (time.time() - 600,) * 2)\n"
	mustSh(t, true, "rm -f "+stale+" "+live+" && python3 -c "+shq(old)+" "+stale)
	listener := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr,
		"exec python3 -c "+shq(old+"s.listen(); print('up', flush=True); sys.stdin.read()")+" "+live)
	in, _ := listener.StdinPipe()
	out, _ := listener.StdoutPipe()
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); listener.Wait() })
	buf := make([]byte, 3)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "up\n" {
		t.Fatalf("the listening socket did not come up: %q %v", buf, err)
	}
	if r := execIn(t, repo(t), true, "true"); r.code != 0 {
		t.Fatalf("job: code %d, stderr %q", r.code, r.stderr)
	}
	if _, err := sh(t, true, "test -S "+stale); err == nil {
		t.Error("the socket nothing listens on is still there")
	}
	if _, err := sh(t, true, "test -S "+live); err != nil {
		t.Error("the listened-on socket was removed")
	}
}

// A job killed while it is being set up leaves no directory in the server's /run: its
// mount points all sit in the job's own directory there, which the launcher removes.
func TestJobKilledDuringSetupLeavesNoMountPointInRun(t *testing.T) {
	const leftovers = "ls -d /run/offload-job.* /run/offload-rw.* /run/offload-ro.* /run/offload-sweep.* 2>/dev/null || true"
	waitFor(t, func() bool {
		out, _ := sh(t, true, "systemctl list-units --state=active --no-legend 'offload-job-*.scope'")
		return strings.TrimSpace(out) == ""
	}, 120*time.Second, "other jobs on the server to end")
	if out := mustSh(t, true, leftovers); out != "" {
		t.Fatalf("/run already holds %q", out)
	}
	bin, armed := heldLauncher(t, "setup-kill", `umount -l "$rw" "$ro"`, 6175)
	cmd, _, _ := start(t, bin, "server", repo(t), nil, "true")
	awaitArmed(t, armed)
	mustSh(t, true, "sudo systemctl kill --signal=SIGKILL "+scopeOfPattern(t, "^[s]leep 6175$"))
	cmd.Wait()
	waitFor(t, func() bool { return mustSh(t, true, leftovers) == "" }, 15*time.Second, "the launcher to remove its directories from /run")
}

// startSleeper runs a job holding an NFS mount of dev and returns the pid on the server of
// its process, which is in the job's mount namespace.
func startSleeper(t *testing.T) string {
	t.Helper()
	start(t, offloadBin, "server", repo(t), nil, "sh", "-c", "exec sleep 6173")
	waitOnServer(t, "^[s]leep 6173$", true, 30*time.Second)
	t.Cleanup(func() { sh(t, true, "sudo systemctl kill --signal=SIGKILL "+scopeOfPattern(t, "^[s]leep 6173$")) })
	return strings.TrimSpace(mustSh(t, true, "pgrep -f '^[s]leep 6173$' | head -1"))
}

// shutdownOf is the value of the shutdown switch of each NFS superblock the pid mounts.
func shutdownOf(t *testing.T, pid string) string {
	t.Helper()
	out := mustSh(t, true, `for sb in $(sudo awk '$0 ~ / - nfs4 / { print $3 }' /proc/`+pid+`/mountinfo | sort -u); do sudo cat /sys/fs/nfs/$sb/shutdown; done`)
	return strings.Join(strings.Fields(out), "")
}

func TestCutShutsTheSuperblocksOfAJobsOwnMountNamespace(t *testing.T) {
	launcherUnderTest(t, "cut") // places the copy on the server
	pid := startSleeper(t)
	if got := shutdownOf(t, pid); got == "" || strings.Contains(got, "1") {
		t.Fatalf("before the cut the job's superblocks read %q, want only 0", got)
	}
	mustSh(t, true, "sudo /usr/local/libexec/offload-launch-it-cut --cut")
	if got := shutdownOf(t, pid); got == "" || strings.Contains(got, "0") {
		t.Errorf("after the cut the job's superblocks read %q, want only 1", got)
	}
}

// A mount of the host's own mount namespace is not a job's, even when the process holding it
// sits in a job scope (offload-forward does). The host namespace is stood in for by the job's
// own through OFFLOAD_HOST_PID, since dev's filesystem is never mounted in the real one.
func TestCutLeavesAMountOfTheHostNamespaceUntouched(t *testing.T) {
	launcherUnderTest(t, "cut-host")
	pid := startSleeper(t)
	mustSh(t, true, "sudo env OFFLOAD_HOST_PID="+pid+" /usr/local/libexec/offload-launch-it-cut-host --cut")
	if got := shutdownOf(t, pid); got == "" || strings.Contains(got, "1") {
		t.Errorf("a mount of the host namespace was cut: superblocks read %q, want only 0", got)
	}
}

// A deny entry that is a symlink to an absolute path still blocks writes to its target:
// the launcher resolves it inside dev's tree, not against the server's own root.
func TestDenyEntryThatIsASymlinkStillBlocksItsTarget(t *testing.T) {
	dir := codeRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "other", "real.md")
	if err := os.WriteFile(target, []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "TASKS.md"))
	if err := os.Symlink(target, filepath.Join(dir, "TASKS.md")); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"echo x >> TASKS.md", "echo x >> other/real.md"} {
		r := execIn(t, dir, true, "sh", "-c", write)
		if r.code == 0 || !strings.Contains(r.stderr, "Read-only file system") {
			t.Errorf("%q: code %d, stderr %q, want a read-only failure", write, r.code, r.stderr)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "real\n" {
		t.Errorf("the symlink's target changed on dev: %q", b)
	}
}
