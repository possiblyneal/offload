package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// binWithConfig builds an offload whose config/ is a copy of the unit's with extra
// lines appended, so a test can set caps, a grace period or a second remote.
func binWithConfig(t *testing.T, extra string) string {
	t.Helper()
	conf, err := filepath.Abs("../../config")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratchRoot, "offload-conf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, name := range []string{"offload.conf", "remote-local-paths"} {
		b, err := os.ReadFile(filepath.Join(conf, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "offload.conf" {
			b = append(b, []byte("\n"+extra+"\n")...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "offload")
	build := exec.Command("go", "build", "-ldflags", ldflags(dir), "-o", bin, "devex/offload")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build offload: %v\n%s", err, out)
	}
	return bin
}

// start runs bin --where where -- argv in dir without waiting for it.
func start(t *testing.T, bin, where, dir string, env []string, argv ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"--where", where, "--"}, argv...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second // a stray ssh holding the pipes must not hang the test
	// A group of its own, so the cleanup also kills a local fallback the wrapper started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
	return cmd, &stdout, &stderr
}

// exitOf is the code a shell reports for a process that ended with err: 128+n for one
// killed by signal n, as the wrapper is when its command was.
func exitOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if s := signalOf(ee.ProcessState); s != 0 {
			return 128 + s
		}
		return ee.ExitCode()
	}
	return -1
}

// signalOf is the signal the process died by, or 0.
func signalOf(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

// onServer reports whether a process whose command line matches the pattern runs
// on the server. The pattern must be written so the probe's own command line does not
// match it: "[s]leep 7311".
func onServer(t *testing.T, pattern string) bool {
	t.Helper()
	_, err := sh(t, true, "pgrep -f '"+pattern+"'")
	return err == nil
}

func waitOnServer(t *testing.T, pattern string, want bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if onServer(t, pattern) == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("after %v, process %q running on server = %v, want %v", within, pattern, !want, want)
}

func TestKillNineOfTheJobExits137AndIsNotRerun(t *testing.T) {
	dir := repo(t)
	script := `import os; open("ran", "a").write("x"); os.kill(os.getpid(), 9)`
	r := execIn(t, dir, true, "python3", "-c", script)
	if r.code != 137 || r.signal != 9 {
		t.Errorf("exit code %d (signal %d), want killed by 9; stderr %q", r.code, r.signal, r.stderr)
	}
	if got := r.files["ran"].content; got != "x" {
		t.Errorf("job ran %d times, want once (file %q)", len(got), got)
	}
}

func TestJobOverItsMemoryCapIsReportedAsAnOOMKill(t *testing.T) {
	bin := binWithConfig(t, "memory-max server 256M")
	cmd, _, stderr := start(t, bin, "server", repo(t), nil,
		"python3", "-c", `b = b"x" * (2 << 30); print(len(b))`)
	err := cmd.Wait()
	if exitOf(err) != 137 {
		t.Errorf("exit code %d, want 137", exitOf(err))
	}
	if !strings.Contains(stderr.String(), "oom-kill") {
		t.Errorf("stderr does not name the OOM kill: %q", stderr.String())
	}
}

// cgroupLimits reads the job's own scope limits on the remote. The /sys path is built at
// run time: an argument naming /sys keeps the job on dev, whose /sys a remote does not share.
const cgroupLimits = `import os
cg = open("/proc/self/cgroup").read().strip().split(":")[-1]
for name in ("memory.max", "cpu.max"):
    print(name, open(os.path.join(os.sep, "sys", "fs", "cgroup" + cg, name)).read().strip())`

func limitsOf(t *testing.T, bin string) map[string]string {
	t.Helper()
	cmd, stdout, stderr := start(t, bin, "server", repo(t), nil, "python3", "-c", cgroupLimits)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	if strings.Contains(stderr.String(), "running locally") {
		t.Fatalf("the probe ran on dev, not in a job scope: %s", stderr)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		k, v, _ := strings.Cut(line, " ")
		got[k] = v
	}
	return got
}

func TestJobScopeDefaultsToAllButOneCoreAndThreeQuartersOfRAM(t *testing.T) {
	got := limitsOf(t, offloadBin)
	threads, _ := strconv.Atoi(strings.TrimSpace(mustSh(t, true, "nproc")))
	if want := fmt.Sprintf("%d 100000", (threads-1)*100000); got["cpu.max"] != want {
		t.Errorf("cpu.max = %q, want %q", got["cpu.max"], want)
	}
	kb, _ := strconv.Atoi(strings.Fields(mustSh(t, true, "grep MemTotal /proc/meminfo"))[1])
	max, _ := strconv.ParseFloat(got["memory.max"], 64)
	if want := float64(kb) * 1024 * 0.75; max < want*0.99 || max > want*1.01 {
		t.Errorf("memory.max = %v, want about %v", max, want)
	}
}

func TestJobScopeCapsComeFromTheConfig(t *testing.T) {
	got := limitsOf(t, binWithConfig(t, "cpu-quota server 150%\nmemory-max server 512M"))
	if got["cpu.max"] != "150000 100000" {
		t.Errorf("cpu.max = %q", got["cpu.max"])
	}
	if got["memory.max"] != strconv.Itoa(512<<20) {
		t.Errorf("memory.max = %q", got["memory.max"])
	}
}

func TestInterruptReachesTheChildrensHandlersAndLeavesNothingBehind(t *testing.T) {
	dir := repo(t)
	// Two children, each with a SIGINT handler that leaves a file; the parent waits as
	// make -j does. Python sets its handler explicitly, since a shell's background jobs
	// start with SIGINT ignored.
	child := `import signal, sys, time
def bye(*a):
    open(sys.argv[1], "w").write("handled")
    sys.exit(0)
signal.signal(signal.SIGINT, bye)
open(sys.argv[1] + ".up", "w").write("up")
time.sleep(7315)`
	script := fmt.Sprintf(`python3 -c '%[1]s' h1 & python3 -c '%[1]s' h2 & wait`, child)
	cmd, _, stderr := start(t, offloadBin, "server", dir, nil, "sh", "-c", script)
	for _, f := range []string{"h1.up", "h2.up"} {
		waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }, 30*time.Second, f)
	}

	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	// Runs before start's cleanup, whose own Wait must not run beside this one.
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	select {
	case <-done:
	case <-time.After(8 * time.Second): // inside the 10 s grace: the handlers did it
		t.Fatalf("offload still running 8 s after SIGINT; stderr %q", stderr)
	}
	for _, f := range []string{"h1", "h2"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != "handled" {
			t.Errorf("child %s's SIGINT handler did not run", f)
		}
	}
	waitOnServer(t, "[t]ime.sleep.7315", false, 5*time.Second)
}

// USR1 on the wrapper reaches the remote job's handler and the job runs on to its own
// exit, as dd's progress report does on a local run.
func TestUSR1ReachesTheJobsHandlerAndTheJobRunsOn(t *testing.T) {
	dir := repo(t)
	cmd, _, stderr := start(t, offloadBin, "server", dir, nil, "sh", "-c",
		`trap 'echo got >usr1' USR1; touch up; while [ ! -e usr1 ]; do sleep 0.1; done; sleep 1; echo ran-on >after`)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	time.Sleep(time.Second) // the started record is on its way to the wrapper
	cmd.Process.Signal(syscall.SIGUSR1)
	err := cmd.Wait()
	if code := exitOf(err); code != 0 {
		t.Errorf("exit %d, want the job's own 0; stderr %q", code, stderr)
	}
	for f, want := range map[string]string{"usr1": "got\n", "after": "ran-on\n"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != want {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
}

// `timeout` moves the job into a process group of its own, which the launcher's signal to
// the job's group no longer reaches; offload-wait passes it on, so the run ends at once
// as a local one does, never after the grace (30 s here).
func TestSigtermReachesAJobThatLeftItsProcessGroup(t *testing.T) {
	bin := binWithConfig(t, "grace 30")
	dir := repo(t)
	cmd, _, stderr := start(t, bin, "server", dir, nil,
		"sh", "-c", `touch up; exec timeout 60 sleep 7317`)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	time.Sleep(time.Second) // the started record is on its way to the wrapper
	t0 := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	if took := time.Since(t0); took > 20*time.Second {
		t.Errorf("stopped after %v, want well before the 30 s grace", took)
	}
	if code := exitOf(err); code != 143 {
		t.Errorf("exit %d, want death by SIGTERM; stderr %q", code, stderr)
	}
	waitOnServer(t, "[s]leep 7317", false, 5*time.Second)
}

func waitFor(t *testing.T, ok func() bool, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

func TestSigtermStopsAJobThatIgnoresItAfterTheGrace(t *testing.T) {
	bin := binWithConfig(t, "grace 3")
	dir := repo(t)
	cmd, _, stderr := start(t, bin, "server", dir, nil,
		"sh", "-c", `trap "" INT TERM HUP; touch up; while :; do sleep 7316; done`)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	// A job placed here would ignore the TERM and never end.
	waitOnServer(t, "^sleep 7316", true, 5*time.Second)
	time.Sleep(time.Second) // the started record is on its way to the wrapper
	t0 := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	// Runs before start's cleanup, whose own Wait must not run beside this one.
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	// Anchored: the launcher's own command line names it too.
	waitOnServer(t, "^sleep 7316", false, 8*time.Second)
	// t0 is before the TERM reaches the launcher, so the full grace has passed by the kill.
	if took := time.Since(t0); took < 3*time.Second {
		t.Errorf("job gone after %v, before the 3 s grace ended", took)
	}
	// The killed job's wrapper then sweeps silly-rename files on dev.
	select {
	case <-done:
		// As a local command that ignored the TERM would have been killed: by the TERM,
		// never an exit of 137 the job did not choose.
		if got := signalOf(cmd.ProcessState); got != int(syscall.SIGTERM) {
			t.Errorf("wrapper ended by signal %d (exit %d), want SIGTERM", got, exitOf(waitErr))
		}
		if !strings.Contains(stderr.String(), "grace period") {
			t.Errorf("stderr %q does not say the remote killed the job after the grace", stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Errorf("wrapper still running %v after the TERM", time.Since(t0))
	}
}

// The job's own process dies by the TERM, but a child it left ignores it: the grace kill
// still reports the grace, not the job's own signal as if it alone had run.
func TestSigtermGraceKillOfAStragglerSaysTheGraceEnded(t *testing.T) {
	bin := binWithConfig(t, "grace 2")
	dir := repo(t)
	cmd, _, stderr := start(t, bin, "server", dir, nil,
		"sh", "-c", `(trap "" INT TERM HUP; exec sleep 7317) & touch up; exec sleep 7318`)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	waitOnServer(t, "^sleep 7317", true, 5*time.Second)
	time.Sleep(time.Second) // the started record is on its way to the wrapper
	cmd.Process.Signal(syscall.SIGTERM)
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	waitOnServer(t, "^sleep 7317", false, 8*time.Second)
	select {
	case <-done:
		if got := signalOf(cmd.ProcessState); got != int(syscall.SIGTERM) {
			t.Errorf("wrapper ended by signal %d (exit %d), want SIGTERM", got, exitOf(waitErr))
		}
		if !strings.Contains(stderr.String(), "grace period") {
			t.Errorf("stderr %q does not say the remote killed what the job left after the grace", stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Error("wrapper still running 20 s after the straggler was killed")
	}
}

// startRunning offloads `sleep <secs>` and returns once the wrapper has had the launcher's
// "started" record: a wrapper cut off before that falls back to a local run.
func startRunning(t *testing.T, secs int) *exec.Cmd {
	cmd, _ := startRunningWithStderr(t, secs)
	return cmd
}

func startRunningWithStderr(t *testing.T, secs int) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	dir := repo(t)
	cmd, _, stderr := start(t, offloadBin, "server", dir, nil,
		"sh", "-c", fmt.Sprintf("touch up; exec sleep %d", secs))
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	time.Sleep(time.Second)
	return cmd, stderr
}

func TestSshKilledMidJobLeavesNoProcessOnTheServer(t *testing.T) {
	cmd := startRunning(t, 7311)
	if out, err := exec.Command("pkill", "-9", "-P", strconv.Itoa(cmd.Process.Pid), "ssh").CombinedOutput(); err != nil {
		t.Fatalf("pkill: %v %s", err, out)
	}
	// The pattern also matches the launcher's own command line, which lives on through its
	// cleanup after a killed job; the job's scope is stopped within 15 s (CLAUDE.md, Liveness).
	waitOnServer(t, "[s]leep 7311", false, 15*time.Second)
	cmd.Wait()
}

// The control ssh dying after `go` was sent but before the wrapper heard `started` must
// not run the command again on dev: the launcher may have exec'd it already. A copy of the
// launcher holds the job between `go` and the pivot, so the cut lands inside that window.
func TestControlCutBetweenGoAndStartedDoesNotRerunLocally(t *testing.T) {
	bin, armed := heldLauncher(t, "hold", `cd "$root"`, 8)
	dir := repo(t)
	cmd, _, stderr := start(t, bin, "server", dir, nil, "sh", "-c", "echo x >> ran")
	awaitArmed(t, armed)
	if out, err := exec.Command("pkill", "-9", "-P", strconv.Itoa(cmd.Process.Pid), "-f", "--", "--control").CombinedOutput(); err != nil {
		t.Fatalf("pkill: %v %s", err, out)
	}
	werr := cmd.Wait()
	if exitOf(werr) != 255 {
		t.Errorf("exit code %d, want 255; stderr %q", exitOf(werr), stderr)
	}
	if !strings.Contains(stderr.String(), "may still land") {
		t.Errorf("stderr lacks the lost-contact warning: %q", stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ran")); err == nil {
		t.Errorf("the command ran (%q) after the control channel was cut", b)
	}
}

// A launcher step that fails after `go`, when the wrapper may no longer run the command
// itself, ends the run with 125 and says the job did not run, so the failure cannot pass
// for the job's own exit code.
func TestSetupFailingAfterGoExits125AndSaysTheJobDidNotRun(t *testing.T) {
	bin := patchedLauncher(t, "pivot", "\npivot_root . .\n", "\npivot_root . /nonexistent-offload-it\n")
	dir := repo(t)
	code, _, stderr := runOffload(t, bin, dir, nil, "--where", "server", "--", "sh", "-c", "echo x >> ran")
	if code != 125 || !strings.Contains(stderr, "the job did not run") {
		t.Errorf("exit %d, stderr %q: want 125 and the message", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the command ran")
	}
}

// The same for the last step, entering the job's directory, which dev may have removed
// since the wrapper started.
func TestCwdRemovedBeforeTheJobStartsExits125AndSaysSo(t *testing.T) {
	bin, armed := heldLauncher(t, "cwd", `cd "$root"`, 4)
	dir := repo(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, _, stderr := start(t, bin, "server", sub, nil, "sh", "-c", "echo x >> ../ran")
	awaitArmed(t, armed)
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	if code := exitOf(cmd.Wait()); code != 125 || !strings.Contains(stderr.String(), "the job did not run") {
		t.Errorf("exit %d, stderr %q: want 125 and the message", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the command ran")
	}
}

func TestWrapperKilledMidJobLeavesNoProcessOnTheServer(t *testing.T) {
	cmd := startRunning(t, 7317)
	cmd.Process.Kill()
	cmd.Wait()
	waitOnServer(t, "[s]leep 7317", false, 5*time.Second)
}

func TestRemoteUnreachableAtStartRunsLocallyAndLogsIt(t *testing.T) {
	bin := binWithConfig(t, "remote ghost 192.0.2.1")
	state := repo(t) // not t.TempDir(): an XDG_STATE_HOME under /tmp keeps every job local
	cmd, stdout, _ := start(t, bin, "ghost", repo(t), []string{"XDG_STATE_HOME=" + state},
		"sh", "-c", "echo ran-here; exit 4")
	err := cmd.Wait()
	if exitOf(err) != 4 || stdout.String() != "ran-here\n" {
		t.Errorf("code %d, stdout %q: want the command run locally", exitOf(err), stdout)
	}
	recs := readLog(t, state)
	if len(recs) != 1 || recs[0]["placement"] != "local" || recs[0]["rule"] != "remote-unreachable" ||
		recs[0]["exit_code"] != 4.0 || !strings.Contains(recs[0]["reason"].(string), "ghost") {
		t.Errorf("decisions.log does not record the fallback: %v", recs)
	}
}

// dropDev blackholes dev's packets on the server, as a dead link or a powered-off dev
// would, until pattern's process has left the server (at most 40 s) and at least 16 s
// have passed, then lifts the rules. The script runs on the server so that lifting does not need the link. It
// writes the seconds the process took to vanish to a file and returns that file's path.
func dropDev(t *testing.T, pattern string) (resultFile string) {
	t.Helper()
	resultFile = fmt.Sprintf("/tmp/offload-it-drop-%d", time.Now().UnixNano())
	lift := "sudo iptables -D INPUT -s " + devAddr + " -j DROP; sudo iptables -D OUTPUT -d " + devAddr + " -j DROP"
	t.Cleanup(func() { // whatever happened: leave no rule behind
		for i := 0; i < 20; i++ {
			if out, _ := sh(t, true, lift+" 2>&1"); strings.Contains(out, "Bad rule") || strings.Contains(out, "does a matching rule exist") {
				break
			}
			time.Sleep(time.Second)
		}
		sh(t, true, "rm -f "+resultFile)
	})
	script := `
trap '` + lift + ` 2>/dev/null' EXIT
sleep 1 # let the ssh that started this script return before its own packets are dropped
sudo iptables -I INPUT -s ` + devAddr + ` -j DROP
sudo iptables -I OUTPUT -d ` + devAddr + ` -j DROP
t0=$(date +%s.%N)
until ! pgrep -f '` + pattern + `' >/dev/null || (( $(date +%s) - ${t0%.*} > 40 )); do sleep 0.25; done
echo "$(date +%s.%N) - $t0" | bc >` + resultFile + `.tmp && mv ` + resultFile + `.tmp ` + resultFile + `
# Hold the drop past the wrapper's own deadline: lifting it once the job is gone would
# deliver the launcher's buffered status and hide the wrapper's lost-contact path.
until (( $(date +%s) - ${t0%.*} >= 16 )); do sleep 0.25; done`
	if err := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr,
		"nohup setsid bash -c "+shq(script)+" >/dev/null 2>&1 </dev/null &").Run(); err != nil {
		t.Fatal(err)
	}
	return resultFile
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestLinkDroppedMidJobFailsTheWrapperWithin15sAndStopsTheJob(t *testing.T) {
	cmd, stderr := startRunningWithStderr(t, 7313)

	// The job process itself: the launcher and sshd carry the command in their argv, and a
	// launcher whose cleanup waits on the dead NFS export outlives the job. The clock starts
	// once the drop is installed: the ssh that installs it can take seconds on a loaded dev.
	result := dropDev(t, "^sleep 7313$")
	t0 := time.Now()
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	// Runs before start's cleanup, whose own Wait must not run beside this one.
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("offload still running 15 s after the link was dropped")
	}
	t.Logf("wrapper failed %v after the drop, exit %d", time.Since(t0), exitOf(waitErr))
	if exitOf(waitErr) == 0 {
		t.Errorf("exit 0 after the link was lost")
	}
	if !strings.Contains(stderr.String(), "may still land") {
		t.Errorf("stderr lacks the writes warning: %q", stderr)
	}

	// The job vanished from the server within 15 s of the drop.
	var took string
	waitFor(t, func() bool {
		out, err := sh(t, true, "cat "+result)
		took = strings.TrimSpace(out)
		return err == nil
	}, 60*time.Second, "the server's drop script to finish")
	secs, _ := strconv.ParseFloat(took, 64)
	if secs <= 0 || secs > 15 {
		t.Errorf("job took %q s to leave the server, want within 15", took)
	}
}

// A launcher that gives up waiting for `go` ran nothing, even if a late `go` reaches it:
// the wrapper falls back to a local run, once, and both sides say why.
func TestALauncherThatGaveUpOnGoRunsTheJobLocallyOnce(t *testing.T) {
	bin := patchedLauncher(t, "nogo", "go_by=$((SECONDS + 30))", "go_by=$SECONDS")
	dir := repo(t)
	code, stdout, stderr := runOffload(t, bin, dir, nil, "--where", "server", "--", "sh", "-c", "hostname; echo x >> ran")
	host, _ := os.Hostname()
	if code != 0 || stdout != host+"\n" {
		t.Errorf("exit %d, stdout %q: want 0 and a local run on %s; stderr %q", code, stdout, host, stderr)
	}
	for _, want := range []string{"no go from the wrapper", "running locally"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); string(b) != "x\n" {
		t.Errorf("ran %q, want the command run exactly once", b)
	}
}

// A wrapper stopped past the silence limit (SIGSTOP, which it cannot forward) loses its job
// to the remote's watchdog; the run fails with a message, never the kill's 137 as the job's.
func TestAWrapperStoppedPastTheSilenceLimitFailsLoudly(t *testing.T) {
	dir := repo(t)
	cmd, _, stderr := start(t, offloadBin, "server", dir, nil,
		"sh", "-c", "hostname > ran; sleep 7318; echo done >> ran")
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ran")); return err == nil }, 30*time.Second, "the job to start")
	time.Sleep(time.Second)
	// A job that fell back to a local run would sleep for two hours past the stop.
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); string(b) != serverHostname(t)+"\n" {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("the job ran on %q, not the server; stderr %q", b, stderr)
	}
	syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP)
	time.Sleep(15 * time.Second)
	syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
	code := exitOf(cmd.Wait())
	if code != 255 || !strings.Contains(stderr.String(), "hearing nothing from this wrapper") && !strings.Contains(stderr.String(), "lost contact") {
		t.Errorf("exit %d, stderr %q: want 255 and a message", code, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); string(b) != serverHostname(t)+"\n" {
		t.Errorf("ran %q", b)
	}
	waitOnServer(t, "[s]leep 7318", false, 5*time.Second)
}

// A host-side launcher killed outright runs no trap: its monitor stops the job, cleans up
// and tells the wrapper, which ends at once with a message instead of waiting forever.
func TestALauncherKilledOutrightEndsTheRunLoudlyAndLeavesNothing(t *testing.T) {
	cmd, stderr := startRunningWithStderr(t, 7319)
	launcher := strings.TrimSpace(mustSh(t, true,
		`c=$(cut -d: -f3 "/proc/$(pgrep -nf '^[s]leep 7319')/cgroup"); c=${c##*offload-job-}; echo "${c%.scope}"`))
	mustSh(t, true, "sudo kill -9 "+launcher)
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	// Runs before start's cleanup, whose own Wait must not run beside this one.
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	select {
	case <-done:
		if code := exitOf(waitErr); code != 255 || !strings.Contains(stderr.String(), "launcher died") {
			t.Errorf("exit %d, stderr %q: want 255 and the message", code, stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the wrapper still waits 20 s after its launcher was killed; stderr %q", stderr)
	}
	waitOnServer(t, "[s]leep 7319", false, 5*time.Second)
	if out := mustSh(t, true, "systemctl list-units --no-legend offload-job-"+launcher+".scope; ls -d /run/offload-job.* 2>/dev/null || true"); strings.TrimSpace(out) != "" {
		t.Errorf("left on the server: %s", out)
	}
}

// The launcher's monitor killed outright leaves no heartbeat and no watchdog: the launcher
// stops the job at once and ends the run with a message, so the job cannot go on writing
// to dev after the wrapper gives up on it.
func TestAMonitorKilledOutrightStopsTheJobAndEndsTheRunLoudly(t *testing.T) {
	cmd, stderr := startRunningWithStderr(t, 7320)
	launcher := strings.TrimSpace(mustSh(t, true,
		`c=$(cut -d: -f3 "/proc/$(pgrep -nf '^[s]leep 7320')/cgroup"); c=${c##*offload-job-}; echo "${c%.scope}"`))
	// The monitor is the launcher's one child with no children of its own.
	mon := strings.Fields(mustSh(t, true, `for c in $(pgrep -P `+launcher+`); do [ -z "$(pgrep -P $c)" ] && echo $c; done; true`))
	if len(mon) != 1 {
		t.Fatalf("monitor candidates %q", mon)
	}
	mustSh(t, true, "sudo kill -9 "+mon[0])
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	select {
	case <-done:
		if code := exitOf(waitErr); code != 255 || !strings.Contains(stderr.String(), "launcher died") || strings.Contains(stderr.String(), "Killed") {
			t.Errorf("exit %d, stderr %q: want 255 and the message", code, stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the wrapper still waits 20 s after the monitor was killed; stderr %q", stderr)
	}
	waitOnServer(t, "[s]leep 7320", false, 5*time.Second)
}

// A launcher killed outright after `go` but before the job passed its wait ran nothing: its
// monitor reports the job unrun, and the wrapper runs it locally, once.
func TestALauncherKilledBeforeTheJobIsReleasedRunsItLocallyOnce(t *testing.T) {
	bin, armed := heldLauncher(t, "unreleased", `touch "$ctl/released"`, 27)
	dir := repo(t)
	cmd, stdout, stderr := start(t, bin, "server", dir, nil, "sh", "-c", "hostname; echo x >> ran")
	awaitArmed(t, armed)
	launcher := strings.TrimSpace(mustSh(t, true,
		`c=$(cut -d: -f3 "/proc/$(pgrep -nf '^[s]leep 27$')/cgroup"); c=${c##*offload-job-}; echo "${c%.scope}"`))
	mustSh(t, true, "sudo kill -9 "+launcher)
	code := exitOf(cmd.Wait())
	host, _ := os.Hostname()
	if code != 0 || stdout.String() != host+"\n" {
		t.Errorf("exit %d, stdout %q: want 0 and a local run on %s; stderr %q", code, stdout, host, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); string(b) != "x\n" {
		t.Errorf("ran %q, want the command run exactly once", b)
	}
}
