package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func feedAll(lines ...string) controlState {
	var s controlState
	for _, l := range lines {
		s.feed(l)
	}
	return s
}

func TestNoStartedRecordFallsBackToALocalRun(t *testing.T) {
	for name, s := range map[string]controlState{
		"nothing at all":       feedAll(),
		"only heartbeats":      feedAll("hb", "hb"),
		"garbage":              feedAll("who knows"),
		"status without start": feedAll("status exit 3"),
	} {
		if o := decide(s, 0); !o.fallback {
			t.Errorf("%s: want fallback, got %+v", name, o)
		}
	}
}

// heardAndSent is feedAll after the wrapper has answered every `ready` with `go`.
func heardAndSent(lines ...string) controlState {
	s := feedAll(lines...)
	if s.wantGo(0) {
		s.goSent = true
	}
	return s
}

func TestWrapperSendsGoOnceOnReady(t *testing.T) {
	var s controlState
	if s.feed("hb"); s.wantGo(0) {
		t.Error("go before ready")
	}
	s.feed("ready")
	if !s.wantGo(0) {
		t.Fatal("no go after ready")
	}
	s.goSent = true
	if s.feed("ready"); s.wantGo(0) {
		t.Error("go sent twice")
	}
}

func TestContactLostAfterGoNeverFallsBackToALocalRun(t *testing.T) {
	// The launcher may have written started and exec'd before the wrapper heard it.
	for name, s := range map[string]controlState{
		"nothing more after go": heardAndSent("ready"),
		"only heartbeats":       heardAndSent("ready", "hb"),
	} {
		o := decide(s, 0)
		if o.fallback {
			t.Errorf("%s: fell back after go was sent: %+v", name, o)
		}
		if o.code != 255 || !strings.Contains(o.message, "may still land") {
			t.Errorf("%s: want the lost-contact outcome, got %+v", name, o)
		}
		// The job may still run, holding its files open.
		if o.sweep {
			t.Errorf("%s: sweeps while the job may still run: %+v", name, o)
		}
	}
}

func TestContactLostBeforeGoFallsBackToALocalRun(t *testing.T) {
	if o := decide(feedAll("hb"), 0); !o.fallback {
		t.Errorf("launcher never said ready: %+v", o)
	}
	if o := decide(feedAll("ready"), 0); !o.fallback {
		t.Errorf("go was never sent, so nothing ran: %+v", o)
	}
}

func TestNoSlotStillFallsBackAndNamesTheReason(t *testing.T) {
	if o := decide(feedAll("noslot"), 0); !o.fallback || !o.noSlot {
		t.Errorf("noslot: %+v", o)
	}
}

func TestStartedJobIsNeverRerunLocally(t *testing.T) {
	for _, s := range []controlState{
		feedAll("started"),
		feedAll("started", "hb", "status exit 0"),
		feedAll("started", "status oom-kill"),
	} {
		if o := decide(s, 0); o.fallback {
			t.Errorf("%+v fell back after the job started", s)
		}
	}
}

func TestExitCodeComesFromTheWaitStatusRecord(t *testing.T) {
	cases := []struct {
		lines []string
		want  int
	}{
		{[]string{"started", "status exit 0"}, 0},
		{[]string{"started", "status exit 3"}, 3},
		{[]string{"started", "status exit 137"}, 137}, // kill -9 of the job: 128+9
		{[]string{"started", "status oom-kill"}, 137},
	}
	for _, c := range cases {
		if o := decide(feedAll(c.lines...), 0); o.code != c.want {
			t.Errorf("%v: code %d, want %d", c.lines, o.code, c.want)
		}
	}
}

func TestACleanJobResultAddsNoMessage(t *testing.T) {
	if o := decide(feedAll("started", "status exit 3"), 0); o.message != "" {
		t.Errorf("message %q on a job that simply exited 3", o.message)
	}
}

func TestOOMKillIsReportedByName(t *testing.T) {
	o := decide(feedAll("started", "status oom-kill"), 0)
	if !strings.Contains(o.message, "oom-kill") {
		t.Errorf("message %q does not name the OOM kill", o.message)
	}
	if !strings.Contains(o.message, "no more writes") {
		t.Errorf("message %q does not say whether writes may still land", o.message)
	}
}

func TestLostPeerAfterStartFailsAndWarnsWritesMayLand(t *testing.T) {
	o := decide(feedAll("started", "hb"), 0)
	if o.fallback || o.code == 0 {
		t.Fatalf("lost peer: %+v", o)
	}
	if !strings.Contains(o.message, "may still land") {
		t.Errorf("message %q does not warn that writes may still land", o.message)
	}
}

func TestSignalDeathOfTheWrapperMapsTo128PlusN(t *testing.T) {
	sig := int(syscall.SIGINT)
	if o := decide(feedAll("started", "status exit 130"), sig); o.code != 130 {
		t.Errorf("job status wins when it arrives: %+v", o)
	}
	if o := decide(feedAll("started"), sig); o.code != 128+sig {
		t.Errorf("signalled and peer gone: code %d, want %d", o.code, 128+sig)
	}
	if o := decide(feedAll(), sig); o.fallback || o.code != 128+sig {
		t.Errorf("signalled before the start must not run the job locally: %+v", o)
	}
	if o := decide(heardAndSent("ready"), sig); o.fallback || o.code != 128+sig {
		t.Errorf("signalled after go, no started heard: %+v", o)
	}
}

func TestParseConfigReadsLifetimeSettings(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
remote server 192.0.2.10
grace 7
cpu-quota server 700%
memory-max server 24G
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.grace != 7 {
		t.Errorf("grace = %d", cfg.grace)
	}
	if cfg.cpuQuota["server"] != "700%" || cfg.memoryMax["server"] != "24G" {
		t.Errorf("caps = %v %v", cfg.cpuQuota, cfg.memoryMax)
	}
	def, _ := parseConfig(strings.NewReader("remote server 192.0.2.10"))
	if def.grace != 10 {
		t.Errorf("default grace = %d, want 10", def.grace)
	}
}

func TestLauncherCommandCarriesTheControlIDAndLimits(t *testing.T) {
	j := job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "abc123", grace: 10, cpuQuota: "700%", memoryMax: "24G"}
	got := launcherCommand(j)
	for _, want := range []string{"'OFFLOAD_CTL=abc123'", "'OFFLOAD_GRACE=10'", "'OFFLOAD_CPU_QUOTA=700%'", "'OFFLOAD_MEMORY_MAX=24G'"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	j.cpuQuota, j.memoryMax = "", ""
	got = launcherCommand(j)
	if strings.Contains(got, "OFFLOAD_CPU_QUOTA") || strings.Contains(got, "OFFLOAD_MEMORY_MAX") {
		t.Errorf("unset caps must be left to the remote's defaults: %q", got)
	}
}

func TestLauncherCommandPassesTheStdioPlan(t *testing.T) {
	j := job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "abc123"}
	if got := launcherCommand(j); strings.Contains(got, "OFFLOAD_STDIO") {
		t.Errorf("a plan of plain relays needs no OFFLOAD_STDIO: %q", got)
	}
	j.stdio = stdioPlan{merged: true}
	if got := launcherCommand(j); !strings.Contains(got, "'OFFLOAD_STDIO=err merged'") {
		t.Errorf("merged stderr missing from %q", got)
	}
}

func TestForwardedSignalsAreNamedTheWayTheLauncherReadsThem(t *testing.T) {
	for sig, want := range map[syscall.Signal]string{syscall.SIGINT: "INT", syscall.SIGTERM: "TERM", syscall.SIGHUP: "HUP",
		syscall.SIGQUIT: "QUIT", syscall.SIGUSR1: "USR1", syscall.SIGUSR2: "USR2"} {
		if got := signalName(sig); got != want {
			t.Errorf("signalName(%v) = %q, want %q", sig, got, want)
		}
	}
}

func TestControlCommandNamesTheChannel(t *testing.T) {
	if got := controlCommand("abc123"); !strings.Contains(got, "--control 'abc123'") {
		t.Errorf("controlCommand = %q", got)
	}
}

func TestNoGoAfterASignal(t *testing.T) {
	s := feedAll("ready")
	if s.wantGo(int(syscall.SIGINT)) {
		t.Error("go sent after the wrapper forwarded a signal")
	}
}

func TestStatusAfterGoBeforeStartedIsTheExit(t *testing.T) {
	// A signal after go, or a setup failure, ends the job before `started`.
	s := heardAndSent("ready")
	s.feed("status exit 130")
	o := decide(s, int(syscall.SIGINT))
	if o.fallback || o.code != 130 || o.message != "" {
		t.Errorf("want exit 130 without lost-contact warning, got %+v", o)
	}
}

// fakeLauncher answers the control protocol the way the launcher does: `ready` first,
// then `started` and the given tail of records once it hears `go`.
func fakeLauncher(tail string) *exec.Cmd {
	return exec.Command("sh", "-c", `echo ready; while read l; do
		if [ "$l" = go ]; then echo started; `+tail+` fi; done`)
}

// superviseWith runs supervise on a fake job and a fake control session, with the job's
// stdout read slowly by the test: nothing is read for stall, then everything.
func superviseWith(t *testing.T, job, ctl *exec.Cmd, stall time.Duration) (outcome, int) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	job.Stdout = pw
	read := make(chan int)
	go func() {
		time.Sleep(stall)
		n, _ := io.Copy(io.Discard, pr)
		read <- int(n)
	}()
	out, err := supervise(job, ctl, func() {})
	pw.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out, <-read
}

func TestSlowReaderGetsEveryByteOfTheJobsOutput(t *testing.T) {
	const size = 3000000
	job := exec.Command("sh", "-c", fmt.Sprintf("exec head -c %d /dev/zero", size))
	out, got := superviseWith(t, job, fakeLauncher("echo 'status exit 0';"), 6*time.Second)
	if got != size || out.code != 0 || out.message != "" {
		t.Errorf("read %d of %d bytes, outcome %+v", got, size, out)
	}
}

// heldJob is a fake job's ssh session that runs script only once the fake launcher runs
// release (a fakeLauncher tail), as a real job's output starts only after `go`: a fake job
// ending before then is a session that ended before the start, which falls back.
func heldJob(t *testing.T, script string) (job *exec.Cmd, release string) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command("sh", "-c", `read _ <"$1"; `+script, "sh", gate), "echo >'" + gate + "';"
}

func TestJobStreamCutBeforeItsEndFailsWithAMessage(t *testing.T) {
	// ssh exits 255 when its connection drops; the launcher said the job exited 0.
	job, release := heldJob(t, "printf partial; exit 255")
	out, _ := superviseWith(t, job, fakeLauncher(release+"echo 'status exit 0';"), 0)
	if out.code == 0 || !strings.Contains(out.message, "output") {
		t.Errorf("a cut stream must fail and say why: %+v", out)
	}
}

func TestJobExitingLikeItsStatusAddsNoMessage(t *testing.T) {
	job, release := heldJob(t, "echo hi; exit 255")
	out, _ := superviseWith(t, job, fakeLauncher(release+"echo 'status exit 255';"), 0)
	if out.code != 255 || out.message != "" {
		t.Errorf("a job that really exited 255: %+v", out)
	}
}

func TestSilentRemoteStillEndsAtTheSilenceLimit(t *testing.T) {
	defer func(h, s time.Duration) { heartbeatEvery, silenceLimit = h, s }(heartbeatEvery, silenceLimit)
	heartbeatEvery, silenceLimit = 50*time.Millisecond, 300*time.Millisecond
	job := exec.Command("sleep", "60")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do [ "$l" = go ] && echo started; done`)
	start := time.Now()
	out, _ := superviseWith(t, job, ctl, 0)
	if out.code == 0 || !strings.Contains(out.message, "lost contact") || time.Since(start) > 10*time.Second {
		t.Errorf("silent remote: %+v after %s", out, time.Since(start))
	}
}

func TestIdleStdinDoesNotHoldTheWrapperAfterTheJobEnds(t *testing.T) {
	// --stdin with a caller that never writes nor closes: the job ends regardless.
	idle, hold := io.Pipe()
	defer hold.Close()
	in, start, err := jobStdin([]byte("env\x00\x00"), idle)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	job := exec.Command("sh", "-c", "head -c 5 >/dev/null; exit 0")
	job.Stdin = in
	done := make(chan outcome, 1)
	go func() {
		out, err := supervise(job, fakeLauncher("echo 'status exit 0';"), start)
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if out.code != 0 || out.message != "" {
			t.Errorf("a clean exit with stdin still open: %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wrapper is still waiting on an idle stdin")
	}
}

// ctlLog is the lines the fake launcher logged, one per line the wrapper sent it.
func ctlLog(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func sent(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// A job's ssh that ends before `go` ran nothing, so the job falls back to a local run.
func TestJobSessionEndingBeforeGoFallsBack(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ctl.log")
	ctl := exec.Command("sh", "-c", `while read l; do echo "$l" >>"$1"; done`, "sh", log)
	out, _ := superviseWith(t, exec.Command("sh", "-c", "exit 255"), ctl, 0)
	if !out.fallback || sent(ctlLog(t, log), "go") {
		t.Errorf("want a fallback with no go sent: %+v, sent %q", out, ctlLog(t, log))
	}
}

// The control session ending after `go` with no status may have left the job running, so
// the wrapper reports lost contact and never reruns it.
func TestControlClosedAfterGoWithoutStatusIsLostContact(t *testing.T) {
	job, release := heldJob(t, "exit 0")
	ctl := exec.Command("sh", "-c", `echo ready; read l; `+release+` exit 0`)
	out, _ := superviseWith(t, job, ctl, 0)
	if out.fallback || out.code != 255 || !strings.Contains(out.message, "lost contact") {
		t.Errorf("want 255 and the lost-contact message, got %+v", out)
	}
}

// The job's ssh ending after `go` with no status within silenceLimit is lost contact too:
// the stream ending says nothing about the job, which may still be running.
func TestJobSessionEndingAfterGoWithoutStatusIsLostContact(t *testing.T) {
	job, release := heldJob(t, "exit 0")
	ctl := fakeLauncher(release)
	start := time.Now()
	out, _ := superviseWith(t, job, ctl, 0)
	if out.fallback || out.code != 255 || !strings.Contains(out.message, "lost contact") {
		t.Errorf("want 255 and the lost-contact message, got %+v", out)
	}
	if d := time.Since(start); d < silenceLimit || d > silenceLimit+5*time.Second {
		t.Errorf("ended after %s, want about silenceLimit (%s)", d, silenceLimit)
	}
}

// A status that trails the job's end by more than a moment, as on a loaded control
// session, still decides the exit code: the job's clean exit is not reported as lost.
func TestLateStatusAfterTheJobEndedStillCounts(t *testing.T) {
	job, release := heldJob(t, "exit 0")
	out, _ := superviseWith(t, job, fakeLauncher(release+"sleep 4; echo 'status exit 0';"), 0)
	if out.fallback || out.code != 0 || out.message != "" {
		t.Errorf("want the late status's clean exit, got %+v", out)
	}
}

// A `noslot` still in flight when the job's ssh has already ended is read, so the job
// moves to the next remote instead of running here.
func TestNoSlotArrivingAfterTheJobSessionEndedIsRead(t *testing.T) {
	ctl := exec.Command("sh", "-c", "sleep 0.5; echo noslot")
	out, _ := superviseWith(t, exec.Command("sh", "-c", "exit 0"), ctl, 0)
	if !out.fallback || !out.noSlot {
		t.Errorf("want a no-slot fallback, got %+v", out)
	}
}

// A launcher that keeps beating but never reaches `ready` (a hung mount) is given up on:
// nothing ran, so the job runs here.
func TestSetupThatNeverReachesReadyFallsBack(t *testing.T) {
	defer func(h, l time.Duration) { heartbeatEvery, setupLimit = h, l }(heartbeatEvery, setupLimit)
	heartbeatEvery, setupLimit = 20*time.Millisecond, 300*time.Millisecond
	log := filepath.Join(t.TempDir(), "ctl.log")
	ctl := exec.Command("sh", "-c", `while :; do echo hb; sleep 0.05; done & while read l; do echo "$l" >>"$1"; done; kill $!`, "sh", log)
	job := exec.Command("sleep", "30")
	start := time.Now()
	out, _ := superviseWith(t, job, ctl, 0)
	if !out.fallback || sent(ctlLog(t, log), "go") {
		t.Errorf("want a fallback with no go sent: %+v", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("gave up after %s, want about setupLimit", d)
	}
}

// keepSignals keeps this test process alive through the signals a test sends itself,
// whether or not supervise is listening for them at that moment.
func keepSignals(t *testing.T) {
	c := make(chan os.Signal, 16)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(c) })
}

// fifo makes a named pipe; opening it for reading waits for the fake launcher's write.
func fifo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A signal before `ready` is forwarded, and `go` is never sent once the launcher does say
// ready: nothing ran, so the wrapper exits 128+n and runs nothing locally.
func TestSignalBeforeReadyForwardsItAndNeverSendsGo(t *testing.T) {
	defer func(h time.Duration) { heartbeatEvery = h }(heartbeatEvery)
	heartbeatEvery = 20 * time.Millisecond
	keepSignals(t)
	log, armed := filepath.Join(t.TempDir(), "ctl.log"), fifo(t)
	job, release := heldJob(t, "exit 255")
	// The first ping proves supervise listens for signals: it pings only after it does.
	// After `sig INT` the launcher says ready and logs the next line, `go` or a ping.
	ctl := exec.Command("sh", "-c", `pinged=; while read l; do echo "$l" >>"$1"
		case $l in
		ping) [ -n "$pinged" ] || { pinged=1; echo >"$2"; } ;;
		"sig INT") echo ready; read l; echo "$l" >>"$1"; `+release+` exit 0 ;;
		esac; done`, "sh", log, armed)
	go func() {
		if f, err := os.Open(armed); err == nil {
			io.Copy(io.Discard, f) // closed before the write, the writer would die of SIGPIPE
			f.Close()
			syscall.Kill(os.Getpid(), syscall.SIGINT)
		}
	}()
	out, _ := superviseWith(t, job, ctl, 0)
	lines := ctlLog(t, log)
	if out.fallback || out.code != 130 || !sent(lines, "sig INT") || sent(lines, "go") {
		t.Errorf("want exit 130, sig INT sent and no go: %+v, sent %q", out, lines)
	}
}

// A signal while the wrapper waits for the last of the output after the status ends the
// wait. The job is over, so the signal reaches no one but the wrapper, which stands for a
// local command still writing its output: a run with no death of its own dies by it, and
// says why rather than passing for the job's own end. A death the status carries came
// first and stands, as the first ending signal does.
func TestSignalDuringTheFlushEndsTheRunByIt(t *testing.T) {
	for status, want := range map[string]struct {
		code int
		says string
	}{
		"status exit 0":   {143, "output stream was cut"},
		"status exit 3":   {143, "output stream was cut"},
		"status watchdog": {143, "hearing nothing from this wrapper"},
		"status signal 2": {130, ""},
		"status oom-kill": {137, "out-of-memory killer"},
		"status graced":   {143, "grace period"},
	} {
		t.Run(status, func(t *testing.T) {
			keepSignals(t)
			statused := fifo(t)
			tail := `echo '` + status + `'; echo >'` + statused + `';`
			if status == "status graced" {
				// The remote graces a job only after a forwarded signal, which the job
				// then dies by: the signals start first, and the status follows the
				// forwarded one, so the later ones land in the flush.
				tail = `echo >'` + statused + `'; while read l; do [ "$l" = "sig TERM" ] && break; done; echo '` +
					status + `';`
			}
			ctl := fakeLauncher(tail)
			done := make(chan struct{})
			go repeatSignalAfter(statused, syscall.SIGTERM, done)
			out, _ := superviseWith(t, exec.Command("sleep", "60"), ctl, 0)
			close(done)
			if out.code != want.code || out.signal != want.code-128 || !strings.Contains(out.message, want.says) {
				t.Errorf("want death by signal %d saying %q, got %+v", want.code-128, want.says, out)
			}
		})
	}
}

// repeatSignalAfter sends sig to the test until done once ready is written. Until
// supervise has read the status a signal is only forwarded, so it repeats until one lands
// in the flush.
func repeatSignalAfter(ready string, sig syscall.Signal, done <-chan struct{}) {
	f, err := os.Open(ready)
	if err != nil {
		return
	}
	io.Copy(io.Discard, f) // closed before the write, the writer would die of SIGPIPE
	f.Close()
	for {
		syscall.Kill(os.Getpid(), sig)
		select {
		case <-done:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A signal forwarded while the job runs is the job's to handle: one that catches SIGINT
// and exits 0 ends the run with 0, never by the signal.
func TestAForwardedSignalTheJobHandledKeepsItsExit(t *testing.T) {
	out, lines := signalAfter(t, syscall.SIGINT, true, `echo 'status exit 0'; exit 0`)
	if out.fallback || out.code != 0 || out.signal != 0 || out.message != "" || !sent(lines, "sig INT") {
		t.Errorf("want the job's exit 0 and sig INT sent: %+v, sent %q", out, lines)
	}
}

func TestAJobThatNeverPassedItsWaitForGoFallsBackEvenAfterGo(t *testing.T) {
	// A late `go` reaches a launcher that already gave up: the job ran nowhere.
	for name, s := range map[string]controlState{
		"before go": feedAll("ready", "status unrun"),
		"after go":  heardAndSent("ready", "status unrun"),
	} {
		if o := decide(s, 0); !o.fallback {
			t.Errorf("%s: %+v, want a fallback", name, o)
		}
		if o := decide(s, 2); o.fallback || o.code != 130 {
			t.Errorf("%s with SIGINT forwarded: %+v, want exit 130 and no run", name, o)
		}
	}
}

func TestAnUnrunJobWhoseStdinWasSentFailsLoudlyRatherThanRunOnTheRest(t *testing.T) {
	// --stdin began flowing at `go`: a local run would read a truncated input.
	s := heardAndSent("ready", "status unrun")
	s.stdinSpent = true
	if o := decide(s, 0); o.fallback || o.code != 255 || !strings.Contains(o.message, "nothing ran") {
		t.Errorf("%+v, want exit 255 saying nothing ran", o)
	}
	if o := decide(s, 2); o.fallback || o.code != 130 {
		t.Errorf("with SIGINT forwarded: %+v, want exit 130", o)
	}
}

func TestAJobTheRemoteStoppedIsNeverReportedAsItsOwnExit(t *testing.T) {
	for status, want := range map[string]string{
		"watchdog":      "hearing nothing from this wrapper",
		"launcher-lost": "launcher died",
	} {
		o := decide(heardAndSent("ready", "started", "status "+status), 0)
		if o.fallback || o.code != 255 || !strings.Contains(o.message, want) {
			t.Errorf("%s: %+v, want exit 255 saying %q", status, o, want)
		}
	}
}

func TestAStatusBeforeGoOtherThanUnrunIsIgnored(t *testing.T) {
	if s := feedAll("ready", "status watchdog"); s.status != "" {
		t.Errorf("status %q accepted before go", s.status)
	}
}

func TestALateGoWithStdinFollowingNeverRunsTheRestLocally(t *testing.T) {
	// The launcher gave up before `go` arrived; --stdin began flowing at `go`. The job's
	// ssh outlives the control script: one that ended before `ready` would mean nothing ran.
	ctl := exec.Command("sh", "-c", `echo ready; read l; echo 'status unrun'; sleep 1`)
	out, err := supervise(exec.Command("sleep", "3"), ctl, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if out.fallback || out.code != 255 {
		t.Errorf("%+v, want exit 255 and no local run", out)
	}
	out, err = supervise(exec.Command("sleep", "3"), exec.Command("sh", "-c", `echo ready; read l; echo 'status unrun'; sleep 1`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.fallback {
		t.Errorf("without --stdin: %+v, want a local run", out)
	}
}

// A job that died by a signal makes the wrapper die by it too; one that exited 130 does
// not, nor does a status before the job may have run. A signal the wrapper forwarded
// before the job ran ends it as that signal would have.
func TestASignalDeathIsReportedApartFromItsCode(t *testing.T) {
	if o := decide(feedAll("ready", "started", "status signal 2"), 0); o.code != 130 || o.signal != 2 || o.fallback {
		t.Errorf("status signal 2: %+v, want code 130, signal 2", o)
	}
	if o := decide(feedAll("ready", "started", "status exit 130"), 0); o.code != 130 || o.signal != 0 {
		t.Errorf("status exit 130: %+v, want code 130 and no signal", o)
	}
	if s := feedAll("ready", "status signal 2"); s.status != "" {
		t.Errorf("status signal accepted before go: %+v", s)
	}
	for _, bad := range []string{"status signal 0", "status signal 99", "status signal x"} {
		if s := feedAll("ready", "started", bad); s.status != "" {
			t.Errorf("%q accepted: %+v", bad, s)
		}
	}
	if o := decide(feedAll("ready"), 2); o.code != 130 || o.signal != 2 {
		t.Errorf("Ctrl-C before go: %+v, want code 130, signal 2", o)
	}
}

// signalAfter runs supervise on a fake launcher that logs every line it reads, answers
// `go` with `started` when started is set, then has the test send sig, and answers the
// line the wrapper forwards with reply (a shell fragment that ends the session).
func signalAfter(t *testing.T, sig syscall.Signal, started bool, reply string) (outcome, []string) {
	t.Helper()
	defer func(h time.Duration) { heartbeatEvery = h }(heartbeatEvery)
	heartbeatEvery = 20 * time.Millisecond
	keepSignals(t)
	log, armed := filepath.Join(t.TempDir(), "ctl.log"), fifo(t)
	job, release := heldJob(t, "exit 0")
	arm := `echo >"$2"`
	onGo, onPing := `echo started; `+arm, `:`
	if !started {
		onGo, onPing = `:`, `[ -n "$armed" ] || { armed=1; `+arm+`; }`
	}
	ctl := exec.Command("sh", "-c", `armed=; [ -n "$3" ] || echo ready; while read l; do echo "$l" >>"$1"
		case $l in
		go) `+onGo+` ;;
		ping) `+onPing+` ;;
		"sig "*) `+release+` `+reply+` ;;
		esac; done`, "sh", log, armed, map[bool]string{true: "", false: "held"}[started])
	go func() {
		if f, err := os.Open(armed); err == nil {
			io.Copy(io.Discard, f) // closed before the write, the writer would die of SIGPIPE
			f.Close()
			syscall.Kill(os.Getpid(), sig)
		}
	}()
	out, _ := superviseWith(t, job, ctl, 0)
	return out, ctlLog(t, log)
}

// USR1 or USR2 on a running job reaches it and the job runs on, as dd's progress report
// does on a local run: the run ends with the job's own status, not 128+n.
func TestUSR1ReachesARunningJobWithoutEndingTheRun(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
		out, lines := signalAfter(t, sig, true, `echo 'status exit 0'; exit 0`)
		if out.fallback || out.code != 0 || out.signal != 0 || !sent(lines, "sig "+signalName(sig)) {
			t.Errorf("%v: want the job's exit 0 and the signal forwarded: %+v, sent %q", sig, out, lines)
		}
	}
}

// SIGQUIT on a running job is forwarded by name and ends the run, as on a local run.
func TestQuitIsForwardedToARunningJob(t *testing.T) {
	out, lines := signalAfter(t, syscall.SIGQUIT, true, `echo 'status signal 3'; exit 0`)
	if out.fallback || out.code != 131 || !sent(lines, "sig QUIT") {
		t.Errorf("want 131 and sig QUIT sent: %+v, sent %q", out, lines)
	}
}

// USR1 before the job started ends the run with nothing run anywhere: the wrapper would
// die of it before a local command existed, so it aborts the launcher and never sends go.
func TestUSR1BeforeTheJobStartedEndsTheRunUnrun(t *testing.T) {
	out, lines := signalAfter(t, syscall.SIGUSR1, false, `echo ready; read l; echo "$l" >>"$1"; echo 'status unrun'; exit 0`)
	if out.fallback || out.code != 128+int(syscall.SIGUSR1) || !sent(lines, "sig TERM") || sent(lines, "go") {
		t.Errorf("want 138, sig TERM sent and no go: %+v, sent %q", out, lines)
	}
}

// Two ending signals before the job started: the run dies by the first, as a local
// command killed by it would, though each is forwarded.
func TestTheFirstEndingSignalBeforeGoDecidesTheExit(t *testing.T) {
	defer func(h time.Duration) { heartbeatEvery = h }(heartbeatEvery)
	heartbeatEvery = 20 * time.Millisecond
	keepSignals(t)
	log, armed := filepath.Join(t.TempDir(), "ctl.log"), fifo(t)
	job, release := heldJob(t, "exit 0")
	ctl := exec.Command("sh", "-c", `armed=; n=0; while read l; do echo "$l" >>"$1"
		case $l in
		ping) [ -n "$armed" ] || { armed=1; echo >"$2"; } ;;
		"sig "*) n=$((n+1)); [ $n = 2 ] && { `+release+` echo 'status unrun'; exit 0; } ;;
		esac; done`, "sh", log, armed)
	go func() {
		if f, err := os.Open(armed); err == nil {
			io.Copy(io.Discard, f)
			f.Close()
			syscall.Kill(os.Getpid(), syscall.SIGINT)
			time.Sleep(50 * time.Millisecond)
			syscall.Kill(os.Getpid(), syscall.SIGUSR1)
		}
	}()
	out, _ := superviseWith(t, job, ctl, 0)
	lines := ctlLog(t, log)
	if out.fallback || out.code != 130 || out.signal != 2 || !sent(lines, "sig INT") || !sent(lines, "sig TERM") {
		t.Errorf("want 130 by SIGINT with both forwarded: %+v, sent %q", out, lines)
	}
}

// laterSignalsAfter sends this process SIGINT once the fake launcher (which runs start,
// then arms on `go` or as onPing says) arms, then SIGQUIT
// every 10 ms until supervise returns, so a SIGQUIT lands in whichever wait follows the
// launcher's answer to `sig INT` (reply). The run must still die by the first, SIGINT.
func laterSignalsAfter(t *testing.T, job *exec.Cmd, start, onPing, reply string) outcome {
	t.Helper()
	defer func(h time.Duration) { heartbeatEvery = h }(heartbeatEvery)
	heartbeatEvery = 20 * time.Millisecond
	keepSignals(t)
	armed := fifo(t)
	ctl := exec.Command("sh", "-c", `armed=; `+start+` while read l; do
		case $l in
		go) echo started; echo >"$1" ;;
		ping) `+onPing+` ;;
		"sig INT") `+reply+` ;;
		esac; done`, "sh", armed)
	done := make(chan struct{})
	go func() {
		f, err := os.Open(armed)
		if err != nil {
			return
		}
		io.Copy(io.Discard, f)
		f.Close()
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
				syscall.Kill(os.Getpid(), syscall.SIGQUIT)
			}
		}
	}()
	out, _ := superviseWith(t, job, ctl, 0)
	close(done)
	return out
}

// After `noslot` or `status unrun`, while the job's ssh still holds the stream, USR1 or
// USR2 ends the run by that signal: no job ran to inform, so it must not fall back to a
// local run that never hears it.
func TestAnInformationalSignalAfterTheJobNeverRanEndsTheRun(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"noslot", `echo noslot; echo >"$1"; sleep 5`},
		{"unrun", `echo ready; while read l; do case $l in go) echo 'status unrun'; echo >"$1"; sleep 5 ;; esac; done`},
	} {
		for _, sig := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
			keepSignals(t)
			armed := fifo(t)
			ctl := exec.Command("sh", "-c", c.script, "sh", armed)
			go func() {
				f, err := os.Open(armed)
				if err != nil {
					return
				}
				io.Copy(io.Discard, f)
				f.Close()
				time.Sleep(100 * time.Millisecond) // into the flush, past the record
				syscall.Kill(os.Getpid(), sig)
			}()
			out, _ := superviseWith(t, exec.Command("sleep", "3"), ctl, 0)
			if out.fallback || out.signal != int(sig) || out.code != 128+int(sig) {
				t.Errorf("%s then %v: want death by it, got %+v", c.name, sig, out)
			}
		}
	}
}

// Before go, the job's session ends and the wrapper drains the records still in flight;
// a signal then does not replace the one already forwarded.
func TestASignalWhileDrainingKeepsTheFirstEndingSignal(t *testing.T) {
	job, release := heldJob(t, "exit 255")
	out := laterSignalsAfter(t, job, ``, `[ -n "$armed" ] || { armed=1; echo >"$1"; }`, release)
	if out.fallback || out.code != 130 || out.signal != 2 {
		t.Errorf("want 130 by SIGINT: %+v", out)
	}
}

// After the job started, the control session ends without a status and the wrapper
// waits for the job's last output; a signal then does not replace the first.
func TestASignalWhileFlushingKeepsTheFirstEndingSignal(t *testing.T) {
	out := laterSignalsAfter(t, exec.Command("sleep", "60"), `echo ready;`, `:`, `exit 0`)
	if out.fallback || out.code != 130 || out.signal != 2 {
		t.Errorf("want 130 by SIGINT: %+v", out)
	}
}

// Until start runs, the job's ssh gets the environment block and nothing of the caller's
// stdin: a job the remote gives up on before `go` falls back here on stdin unread.
func TestJobStdinHoldsTheCallersStdinUntilStart(t *testing.T) {
	follow := strings.NewReader("caller data")
	in, start, err := jobStdin([]byte("env\x00"), follow)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	buf := make([]byte, 64)
	n, _ := in.Read(buf)
	if string(buf[:n]) != "env\x00" {
		t.Fatalf("first read %q, want the environment block", buf[:n])
	}
	in.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := in.Read(buf); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("before start: read %q, %v; want nothing", buf[:n], err)
	}
	if follow.Len() != len("caller data") {
		t.Fatalf("before start, %d bytes of the caller's stdin were consumed", len("caller data")-follow.Len())
	}
	start()
	in.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(in); string(rest) != "caller data" || err != nil {
		t.Fatalf("after start: %q, %v", rest, err)
	}
}

func TestAJobKilledAfterTheGraceDiesByTheForwardedSignalAndSaysSo(t *testing.T) {
	o := decide(heardAndSent("ready", "started", "status graced"), int(syscall.SIGINT))
	if o.fallback || o.code != 130 || o.signal != int(syscall.SIGINT) {
		t.Errorf("%+v, want death by SIGINT, never an exit of 137 as the job's own", o)
	}
	if !strings.Contains(o.message, "grace period") || !strings.Contains(o.message, "part of its output") {
		t.Errorf("message %q does not say the remote killed it after the grace", o.message)
	}
	if o := decide(heardAndSent("ready", "started", "status exit 137"), int(syscall.SIGINT)); o.code != 137 || o.message != "" {
		t.Errorf("the job's own exit 137 must stay its own: %+v", o)
	}
	// With no forwarded signal on record the grace kill's own SIGKILL ends the run.
	if o := decide(heardAndSent("ready", "started", "status graced"), 0); o.code != 137 || o.signal != int(syscall.SIGKILL) {
		t.Errorf("no forwarded signal: %+v, want death by SIGKILL", o)
	}
	// The CPU the job used before the kill is still logged.
	if o := decide(heardAndSent("ready", "started", "cpu 1500", "status graced"), int(syscall.SIGINT)); !o.cpuKnown || o.cpuMs != 1500 {
		t.Errorf("cpu before graced: %+v, want 1500 ms known", o)
	}
}

// Only a job whose scope is known stopped after an end it did not choose may have left
// silly-rename files, and only then is a sweep due.
func TestOnlyAJobStoppedWithFilesOpenIsSwept(t *testing.T) {
	for line, want := range map[string]bool{
		"status exit 0":        false,
		"status exit 1":        false,
		"status exit 130":      true,
		"status signal 9":      true,
		"status oom-kill":      true,
		"status graced":        true,
		"status watchdog":      true,
		"status launcher-lost": true,
		"status unrun":         false,
		"hb":                   false, // no status: contact lost while the job may run
	} {
		if o := decide(heardAndSent("ready", "started", line), 0); o.sweep != want {
			t.Errorf("%s: sweep %v, want %v (%+v)", line, o.sweep, want, o)
		}
	}
}

// The caller's reader gone (`| head -1`) never reaches a remote job by itself: ssh swallows
// its EPIPE and sshd buffers the job's writes. The wrapper's write that fails stands for
// the job's, so the launcher is told which stream lost its reader (`eow out`, `eow err`),
// never sent a signal: the job's own status decides the run, as a local one's does, and the
// output left unread is not reported lost.
func TestAClosedReaderSendsEndOfWriteForItsStream(t *testing.T) {
	for _, stream := range []string{"out", "err"} {
		log := filepath.Join(t.TempDir(), "ctl.log")
		redirect := map[string]string{"out": "", "err": " >&2"}[stream]
		job, release := heldJob(t, "echo a"+redirect+"; sleep 0.3; echo b"+redirect+"; sleep 1; echo c"+redirect)
		ctl := exec.Command("sh", "-c", `echo ready; while read l; do echo "$l" >>"$1"
		case $l in
		go) echo started; `+release+` ;;
		"eow "*) echo 'status exit 141'; exit 0 ;;
		esac; done`, "sh", log)
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if stream == "out" {
			job.Stdout = pw
		} else {
			job.Stderr = pw
		}
		go func() {
			b := make([]byte, 2)
			io.ReadFull(pr, b) // "a\n", then the reader is gone
			pr.Close()
		}()
		out, err := supervise(job, ctl, nil)
		pw.Close()
		if err != nil {
			t.Fatal(err)
		}
		sentLog := ctlLog(t, log)
		if out.code != 141 || out.signal != 0 || out.message != "" || !sent(sentLog, "eow "+stream) ||
			sent(sentLog, "sig PIPE") {
			t.Errorf("%s: want the job's exit 141, no signal, no message, eow %s sent: %+v, sent %q",
				stream, stream, out, sentLog)
		}
	}
}

// A write the caller's destination refuses (/dev/full) ends that stream for the job and
// is said, while the job's own status stands, as it would in a local run.
func TestARefusedWriteEndsTheStreamAndSaysSo(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ctl.log")
	job, release := heldJob(t, "echo a; sleep 1; echo b")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do echo "$l" >>"$1"
		case $l in
		go) echo started; `+release+` ;;
		"eow out") echo 'status exit 0'; exit 0 ;;
		esac; done`, "sh", log)
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	job.Stdout = full
	out, err := supervise(job, ctl, nil)
	if err != nil {
		t.Fatal(err)
	}
	sentLog := ctlLog(t, log)
	if out.code != 0 || out.signal != 0 || !strings.Contains(out.message, "no space left on device") ||
		!sent(sentLog, "eow out") {
		t.Errorf("want the job's exit 0, a message naming ENOSPC, eow out sent: %+v, sent %q", out, sentLog)
	}
}

// A pipe's reader gone is noticed without another write: the job's next write fails, not
// the one after the wrapper's next write of its output.
func TestAClosedReaderIsNoticedWithoutAWrite(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ctl.log")
	job, release := heldJob(t, "echo a; sleep 5")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do echo "$l" >>"$1"
		case $l in
		go) echo started; `+release+` ;;
		"eow out") date +%s.%N >"$1.t"; echo 'status exit 0'; exit 0 ;;
		esac; done`, "sh", log)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	job.Stdout = pw
	go func() {
		b := make([]byte, 2)
		io.ReadFull(pr, b)
		pr.Close()
	}()
	start := time.Now()
	out, err := supervise(job, ctl, nil)
	pw.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log + ".t")
	if err != nil {
		t.Fatalf("no eow out sent: %+v, sent %q", out, ctlLog(t, log))
	}
	at, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if took := time.Unix(0, int64(at*1e9)).Sub(start); took > 3*time.Second {
		t.Errorf("eow out sent %s in; want it at once", took)
	}
}

// A reader that read everything leaves a job's exit alone.
func TestAReaderThatReadsEverythingSendsNoEndOfWrite(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ctl.log")
	job, release := heldJob(t, "echo a; echo b")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do echo "$l" >>"$1"
		case $l in go) echo started; `+release+` sleep 0.3; echo 'status exit 0'; exit 0 ;; esac; done`, "sh", log)
	out, _ := superviseWith(t, job, ctl, 0)
	if out.code != 0 || sent(ctlLog(t, log), "eow out") || sent(ctlLog(t, log), "eow err") {
		t.Errorf("want exit 0 and no eow: %+v, sent %q", out, ctlLog(t, log))
	}
}

// A relayed write past the caller's file-size limit ends the run by SIGXFSZ, as the
// kernel would have killed a local job, however the job itself ended.
func TestAWritePastTheFileSizeLimitEndsTheRunBySIGXFSZ(t *testing.T) {
	if inheritedIgnore(syscall.SIGXFSZ) {
		t.Skip("the test runs with SIGXFSZ ignored")
	}
	job, release := heldJob(t, "head -c 4096 /dev/zero")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do
		case $l in
		go) echo started; `+release+` ;;
		"eow out") echo 'status exit 0'; exit 0 ;;
		esac; done`)
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var saved syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &saved); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1024, Max: saved.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &saved)
	job.Stdout = f
	out, err := supervise(job, ctl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.code != 128+int(syscall.SIGXFSZ) || out.signal != int(syscall.SIGXFSZ) || !strings.Contains(out.message, "file too large") {
		t.Errorf("want death by SIGXFSZ and a message: %+v", out)
	}
}

// The local job would have died of SIGXFSZ at its write, before a signal that later ends
// the wait for its output: the run dies by SIGXFSZ.
func TestASignalAfterAWritePastTheFileSizeLimitKeepsSIGXFSZ(t *testing.T) {
	if inheritedIgnore(syscall.SIGXFSZ) {
		t.Skip("the test runs with SIGXFSZ ignored")
	}
	keepSignals(t)
	statused := fifo(t)
	job, release := heldJob(t, "head -c 4096 /dev/zero; sleep 60")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do
		case $l in
		go) echo started; `+release+` ;;
		"eow out") echo 'status exit 0'; echo >"$1" ;;
		esac; done`, "sh", statused)
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var saved syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &saved); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1024, Max: saved.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &saved)
	job.Stdout = f
	done := make(chan struct{})
	go repeatSignalAfter(statused, syscall.SIGINT, done)
	out, err := supervise(job, ctl, nil)
	close(done)
	if err != nil {
		t.Fatal(err)
	}
	if out.code != 128+int(syscall.SIGXFSZ) || out.signal != int(syscall.SIGXFSZ) || !strings.Contains(out.message, "file too large") {
		t.Errorf("want death by SIGXFSZ and a message: %+v", out)
	}
}

func TestAStatusTheControlSessionEndedInsideIsLostContact(t *testing.T) {
	job := exec.Command("sh", "-c", "exit 0")
	out, _ := superviseWith(t, job, fakeLauncher("printf 'status exit 13'; exit;"), 0)
	if out.code != 255 || out.fallback {
		t.Errorf("a status cut before its newline: %+v, want lost contact (255)", out)
	}
}
