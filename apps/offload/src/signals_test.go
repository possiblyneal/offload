package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCtrlZSuspendsTheRemoteJobInsteadOfLettingItDie(t *testing.T) {
	defer func(f func() bool) { groupOrphaned = f }(groupOrphaned)
	groupOrphaned = func() bool { return false }
	log := filepath.Join(t.TempDir(), "ctl.log")
	defer func(f func()) { suspendSelf = f }(suspendSelf)
	var suspended bool
	suspendSelf = func() { suspended = true; syscall.Kill(os.Getpid(), syscall.SIGCONT) }
	// The launcher side: records what the wrapper sends and ends the job on `sig CONT`.
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do
		echo "$l" >> `+log+`
		if [ "$l" = go ]; then echo started; fi
		if [ "$l" = "sig CONT" ]; then echo 'status exit 0'; fi
	done`)
	job := exec.Command("sleep", "2")
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGTSTP)
	}()
	out, _ := superviseWith(t, job, ctl, 0)
	if !suspended {
		t.Error("the wrapper did not suspend itself")
	}
	if out.code != 0 || out.message != "" {
		t.Errorf("a suspended and resumed job must end normally: %+v", out)
	}
	b, _ := os.ReadFile(log)
	var sigs []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "sig ") {
			sigs = append(sigs, l)
		}
	}
	if want := []string{"sig TSTP", "sig CONT"}; !reflect.DeepEqual(sigs, want) {
		t.Errorf("sent %q, want %q", sigs, want)
	}
}

func TestLocalRunForwardsASignalToTheJobsHandler(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "handled")
	script := `trap 'echo handled > ` + marker + `; exit 0' INT; : > ` + marker + `.up; while :; do sleep 0.1; done`
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(marker + ".up"); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	code, err := runLocal([]string{"sh", "-c", script}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "handled\n" {
		t.Errorf("the job's SIGINT handler did not run: %q", b)
	}
}

func TestLocalRunWithoutATerminalLeadsItsOwnGroup(t *testing.T) {
	// A group kill reaching the wrapper must not also reach the job, which the wrapper
	// signals itself: the job is outside the wrapper's group.
	if stdinIsTerminal() || hasControllingTerminal() {
		t.Skip("a terminal is present")
	}
	out := filepath.Join(t.TempDir(), "pgid")
	code, err := runLocal([]string{"sh", "-c", "ps -o pgid= $$ > " + out}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	b, _ := os.ReadFile(out)
	if strings.TrimSpace(string(b)) == strconv.Itoa(syscall.Getpgrp()) {
		t.Errorf("the job shares the wrapper's process group %s", b)
	}
}

// On a terminal the command gets the terminal's SIGINT itself, so the wrapper drops its own
// copy and stays to wait for the command. Run under `script`, which gives it a terminal.
func TestLocalRunOnATerminalOutlivesSIGINT(t *testing.T) {
	if os.Getenv("OFFLOAD_PTY_CHILD") == "" {
		if _, err := exec.LookPath("script"); err != nil {
			t.Skip("no script(1)")
		}
		run := os.Args[0] + " -test.run=^TestLocalRunOnATerminalOutlivesSIGINT$"
		cmd := exec.Command("script", "-qec", run, "/dev/null")
		cmd.Env = append(os.Environ(), "OFFLOAD_PTY_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "finished") {
			t.Errorf("under a terminal: %v\n%s", err, out)
		}
		return
	}
	if !stdinIsTerminal() {
		t.Fatal("script gave no terminal")
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	code, err := runLocal([]string{"sh", "-c", "sleep 0.6; echo finished"}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
}

// With a controlling terminal but stdin elsewhere, a command that reads /dev/tty (a
// passphrase prompt) must not be stopped by SIGTTIN as a background group: a local run
// would read the terminal. Run under `script`, with stdin from /dev/null.
func TestLocalRunWithATerminalButNoTerminalStdinCanReadTheTerminal(t *testing.T) {
	if os.Getenv("OFFLOAD_PTY_CHILD") == "" {
		if _, err := exec.LookPath("script"); err != nil {
			t.Skip("no script(1)")
		}
		run := os.Args[0] + " -test.run=^TestLocalRunWithATerminalButNoTerminalStdinCanReadTheTerminal$ </dev/null"
		cmd := exec.Command("timeout", "-s", "KILL", "20", "script", "-qec", run, "/dev/null")
		cmd.Env = append(os.Environ(), "OFFLOAD_PTY_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "read done") {
			t.Errorf("under a terminal, stdin from /dev/null: %v\n%s", err, out)
		}
		return
	}
	if stdinIsTerminal() || !hasControllingTerminal() {
		t.Fatal("want a controlling terminal and stdin elsewhere")
	}
	// The terminal reads for its foreground group only; any other group is stopped by
	// SIGTTIN. So the command's group must be the terminal's foreground group (tpgid).
	script := `set -- $(ps -o pgid=,tpgid= $$); [ "$1" = "$2" ] && echo read done || echo "pgid $1, terminal's $2"`
	code, err := runLocal([]string{"sh", "-c", script}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
}

// withStdinPipe puts the read end of a pipe holding data on fd 0 for the test, as a caller's
// `while read` loop would, and returns a func that reads back what the job left unread.
func withStdinPipe(t *testing.T, data string) (unread func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	saved, err := syscall.Dup(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(int(r.Fd()), 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Dup3(saved, 0, 0)
		syscall.Close(saved)
		r.Close()
	})
	return func() string {
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

func TestLocalRunWithoutStdinNeverReadsTheCallersStdin(t *testing.T) {
	unread := withStdinPipe(t, "line 1\nline 2\n")
	dir := t.TempDir()
	script := "readlink /proc/self/fd/0 > " + dir + "/fd0; cat > " + dir + "/read"
	if code, err := runLocal([]string{"sh", "-c", script}, false); err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	if b, _ := os.ReadFile(dir + "/fd0"); strings.TrimSpace(string(b)) != "/dev/null" {
		t.Errorf("the job's stdin is %q, want /dev/null", b)
	}
	if b, _ := os.ReadFile(dir + "/read"); len(b) != 0 {
		t.Errorf("the job read the caller's stdin: %q", b)
	}
	if got := unread(); got != "line 1\nline 2\n" {
		t.Errorf("the caller's stdin lost input: %q left", got)
	}
}

func TestLocalRunWithStdinReadsTheCallersStdin(t *testing.T) {
	unread := withStdinPipe(t, "line 1\nline 2\n")
	read := filepath.Join(t.TempDir(), "read")
	if code, err := runLocal([]string{"sh", "-c", "cat > " + read}, true); err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	if b, _ := os.ReadFile(read); string(b) != "line 1\nline 2\n" {
		t.Errorf("with --stdin the job read %q", b)
	}
	if got := unread(); got != "" {
		t.Errorf("%q left unread", got)
	}
}

// A wrapper killed outright (SIGKILL, as review.py's timeout does) takes its local command
// with it, though the command leads a process group of its own.
func TestLocalRunDiesWithTheWrapper(t *testing.T) {
	if pidfile := os.Getenv("OFFLOAD_WRAPPER_CHILD"); pidfile != "" {
		runLocal([]string{"sh", "-c", "echo $$ > " + pidfile + "; exec sleep 300"}, false)
		os.Exit(0)
	}
	pidfile := filepath.Join(t.TempDir(), "pid")
	wrapper := exec.Command(os.Args[0], "-test.run=^TestLocalRunDiesWithTheWrapper$")
	wrapper.Env = append(os.Environ(), "OFFLOAD_WRAPPER_CHILD="+pidfile)
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		b, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if pid == 0 {
		wrapper.Process.Kill()
		t.Fatal("the local command never started")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	wrapper.Process.Signal(syscall.SIGKILL)
	wrapper.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for running(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if running(pid) {
		t.Errorf("the local command %d outlived the killed wrapper", pid)
	}
}

// running reports whether pid exists and is not a zombie.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	return len(f) > 0 && f[0] != "Z"
}

// A status that arrives while the wrapper is stopped (Ctrl-Z), after the job's ssh has
// ended, still counts on resume: the stop does not use up the silence deadline.
func TestStatusArrivingDuringCtrlZAfterTheJobEndedStillCounts(t *testing.T) {
	defer func(f func() bool) { groupOrphaned = f }(groupOrphaned)
	groupOrphaned = func() bool { return false }
	defer func(f func()) { suspendSelf = f }(suspendSelf)
	defer func(s time.Duration) { silenceLimit = s }(silenceLimit)
	silenceLimit = 100 * time.Millisecond
	suspendSelf = func() { time.Sleep(300 * time.Millisecond); syscall.Kill(os.Getpid(), syscall.SIGCONT) }
	// select picks among ready cases at random. Slow timing only makes a run pass vacuously.
	for i := 0; i < 10; i++ {
		job, release := heldJob(t, "exit 0")
		go func() {
			time.Sleep(50 * time.Millisecond)
			syscall.Kill(os.Getpid(), syscall.SIGTSTP)
		}()
		out, _ := superviseWith(t, job, fakeLauncher(release+"sleep 0.15; echo 'status exit 0';"), 0)
		if out.code != 0 || out.message != "" {
			t.Fatalf("run %d: want the status's clean exit, got %+v", i, out)
		}
	}
}

// A caller's ignored SIGHUP (nohup) stays ignored for a local command: the wrapper does not
// catch it, so a hangup neither reaches the command through the wrapper nor is lost by exec.
func TestLocalRunKeepsTheCallersIgnoredHangup(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	dir := t.TempDir()
	script := "kill -HUP " + strconv.Itoa(os.Getpid()) + "; sleep 0.3; grep SigIgn /proc/self/status > " + dir + "/ign; echo survived > " + dir + "/out"
	code, err := runLocal([]string{"sh", "-c", script}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	if b, _ := os.ReadFile(dir + "/out"); string(b) != "survived\n" {
		t.Errorf("the command did not survive the hangup: %q", b)
	}
	b, _ := os.ReadFile(dir + "/ign")
	f := strings.Fields(string(b))
	if len(f) != 2 {
		t.Fatalf("SigIgn: %q", b)
	}
	if mask, _ := strconv.ParseUint(f[1], 16, 64); mask&1 == 0 {
		t.Errorf("the command lost the ignored SIGHUP: SigIgn %s", f[1])
	}
}

// The same for a remote run: an ignored SIGHUP is never forwarded to the job.
func TestRemoteRunNeverForwardsAnIgnoredHangup(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	defer func(s time.Duration) { silenceLimit = s }(silenceLimit)
	silenceLimit = 100 * time.Millisecond
	log := filepath.Join(t.TempDir(), "ctl.log")
	ctl := exec.Command("sh", "-c", `echo ready; while read l; do
		echo "$l" >> `+log+`
		if [ "$l" = go ]; then echo started; fi
	done`)
	job := exec.Command("sh", "-c", "sleep 0.6")
	go func() {
		time.Sleep(200 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGHUP)
	}()
	superviseWith(t, job, ctl, 0)
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "sig HUP") {
		t.Errorf("forwarded an ignored hangup:\n%s", b)
	}
}

// A signal the caller ignored before offload started (bash ignores SIGQUIT for a script's
// background job; `trap ” TERM`) stays ignored for the local command, though the Go
// runtime replaces every such ignore but SIGHUP's and SIGINT's before main: those the
// wrapper keeps ignored itself, and the rest (XFSZ, WINCH, VTALRM) through viaEnv.
func TestLocalRunKeepsEveryIgnoreTheWrapperInherited(t *testing.T) {
	if out := os.Getenv("OFFLOAD_IGNORE_CHILD"); out != "" {
		keepInheritedIgnores()
		code, _ := runLocal(append(viaEnv(), "sh", "-c", "grep SigIgn /proc/self/status > "+out), false)
		os.Exit(code)
	}
	out := filepath.Join(t.TempDir(), "ign")
	run := "trap '' TERM QUIT TSTP USR1 XFSZ WINCH VTALRM; exec " + os.Args[0] + " -test.run=^TestLocalRunKeepsEveryIgnoreTheWrapperInherited$"
	cmd := exec.Command("bash", "-c", run)
	cmd.Env = append(os.Environ(), "OFFLOAD_IGNORE_CHILD="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	b, _ := os.ReadFile(out)
	f := strings.Fields(string(b))
	if len(f) != 2 {
		t.Fatalf("SigIgn: %q", b)
	}
	mask, _ := strconv.ParseUint(f[1], 16, 64)
	for _, s := range []syscall.Signal{syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGUSR1,
		syscall.SIGXFSZ, syscall.SIGWINCH, syscall.SIGVTALRM} {
		if mask&(1<<(uint(s)-1)) == 0 {
			t.Errorf("the command lost the ignored %v: SigIgn %s", s, f[1])
		}
	}
}

// Without a terminal, SIGQUIT, SIGUSR1 and SIGUSR2 aimed at the wrapper reach the command,
// which leads a group of its own, as `timeout -s USR1` expects.
func TestLocalRunForwardsQuitAndUserSignals(t *testing.T) {
	if stdinIsTerminal() || hasControllingTerminal() {
		t.Skip("a terminal is present")
	}
	for _, s := range []syscall.Signal{syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2} {
		marker := filepath.Join(t.TempDir(), "handled")
		name := sigName(s)
		script := `trap 'echo handled > ` + marker + `; exit 0' ` + name + `; : > ` + marker + `.up; for i in $(seq 30); do sleep 0.1; done; exit 3`
		go func() {
			for i := 0; i < 100; i++ {
				if _, err := os.Stat(marker + ".up"); err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			syscall.Kill(os.Getpid(), s)
		}()
		code, err := runLocal([]string{"sh", "-c", script}, false)
		if b, _ := os.ReadFile(marker); err != nil || code != 0 || string(b) != "handled\n" {
			t.Errorf("%v: runLocal = %d, %v; handler wrote %q", s, code, err, b)
		}
	}
}

// On a terminal the command shares the wrapper's group, so SIGUSR1 and SIGUSR2 aimed at
// the wrapper alone (`timeout -s USR1`) must be passed on, not end the wrapper. Run under
// `script`.
func TestLocalRunOnATerminalForwardsUserSignals(t *testing.T) {
	if os.Getenv("OFFLOAD_PTY_USR_CHILD") == "" {
		if _, err := exec.LookPath("script"); err != nil {
			t.Skip("no script(1)")
		}
		run := os.Args[0] + " -test.run=^TestLocalRunOnATerminalForwardsUserSignals$ -test.v"
		cmd := exec.Command("script", "-qec", run, "/dev/null")
		cmd.Env = append(os.Environ(), "OFFLOAD_PTY_USR_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PASS") {
			t.Errorf("under a terminal: %v\n%s", err, out)
		}
		return
	}
	if !stdinIsTerminal() {
		t.Fatal("script gave no terminal")
	}
	for _, s := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
		marker := filepath.Join(t.TempDir(), "handled")
		script := `trap 'echo handled > ` + marker + `; exit 0' ` + sigName(s) + `; : > ` + marker + `.up; for i in $(seq 30); do sleep 0.1; done; exit 3`
		go func() {
			for i := 0; i < 100; i++ {
				if _, err := os.Stat(marker + ".up"); err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			syscall.Kill(os.Getpid(), s)
		}()
		code, err := runLocal([]string{"sh", "-c", script}, false)
		if b, _ := os.ReadFile(marker); err != nil || code != 0 || string(b) != "handled\n" {
			t.Errorf("%v: runLocal = %d, %v; handler wrote %q", s, code, err, b)
		}
	}
}

func sigName(s syscall.Signal) string {
	return map[syscall.Signal]string{syscall.SIGQUIT: "QUIT", syscall.SIGUSR1: "USR1", syscall.SIGUSR2: "USR2"}[s]
}

// On a terminal, Ctrl-Z stops the command (in the wrapper's group) and the wrapper stops
// itself, so the shell sees the job stopped; even after a remote's Notify for SIGTSTP left
// the Go handler installed. Run under `script`.
func TestLocalRunOnATerminalStopsTheWrapperOnCtrlZ(t *testing.T) {
	if os.Getenv("OFFLOAD_PTY_CHILD") == "" {
		if _, err := exec.LookPath("script"); err != nil {
			t.Skip("no script(1)")
		}
		// The job-control shell gives the test a group of its own whose parent is in the
		// session: the group script's session leader heads is orphaned, and the kernel
		// drops a stop sent to an orphaned group.
		run := "sh -mc '" + os.Args[0] + " -test.run=^TestLocalRunOnATerminalStopsTheWrapperOnCtrlZ$ </dev/null; :'"
		cmd := exec.Command("timeout", "-s", "KILL", "20", "script", "-qec", run, "/dev/null")
		cmd.Env = append(os.Environ(), "OFFLOAD_PTY_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "wrapper stopped") || !strings.Contains(string(out), "continued") ||
			!strings.Contains(string(out), "ran on") || strings.Contains(string(out), "though ignored") {
			t.Errorf("under a terminal: %v\n%s", err, out)
		}
		return
	}
	c := make(chan os.Signal, 1) // as a remote run's supervise leaves it
	signal.Notify(c, syscall.SIGTSTP)
	signal.Stop(c)
	// The shell's fg would continue the whole group; the stand-in does it at once.
	suspendSelf = func() { os.Stdout.WriteString("wrapper stopped\n"); syscall.Kill(0, syscall.SIGCONT) }
	// Ctrl-Z reaches the whole foreground group: the wrapper and the command.
	code, err := runLocal([]string{"sh", "-c", "kill -TSTP 0; echo continued"}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
	// A command that ignores Ctrl-Z runs on, and the wrapper with it.
	suspendSelf = func() { os.Stdout.WriteString("stopped though ignored\n"); syscall.Kill(0, syscall.SIGCONT) }
	code, err = runLocal([]string{"sh", "-c", "trap '' TSTP; kill -TSTP 0; echo ran on"}, false)
	if err != nil || code != 0 {
		t.Fatalf("runLocal = %d, %v", code, err)
	}
}

// A command killed by a signal kills the wrapper by the same signal, so a calling shell
// ends its loop on Ctrl-C as it would for a plain run; SIGQUIT leaves no Go stack trace.
// A command that exits 130 makes the wrapper exit 130.
func TestTheWrapperDiesByTheSignalItsCommandDiedBy(t *testing.T) {
	if script := os.Getenv("OFFLOAD_DIE_CHILD"); script != "" {
		code, err := runLocal([]string{"sh", "-c", script}, false)
		if err != nil {
			os.Exit(99)
		}
		exitAs(code)
	}
	for _, c := range []struct {
		script string
		sig    syscall.Signal
		code   int
	}{
		{"kill -INT $$", syscall.SIGINT, -1},
		{"kill -TERM $$", syscall.SIGTERM, -1},
		{"kill -QUIT $$", syscall.SIGQUIT, -1},
		{"exit 130", 0, 130},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTheWrapperDiesByTheSignalItsCommandDiedBy$")
		cmd.Env = append(os.Environ(), "OFFLOAD_DIE_CHILD="+c.script)
		cmd.Dir = t.TempDir()
		out, _ := cmd.CombinedOutput()
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		switch {
		case c.sig != 0 && (!ws.Signaled() || ws.Signal() != c.sig):
			t.Errorf("%s: wrapper ended %v, want killed by %v\n%s", c.script, ws, c.sig, out)
		case c.sig == 0 && (ws.Signaled() || ws.ExitStatus() != c.code):
			t.Errorf("%s: wrapper ended %v, want exit %d\n%s", c.script, ws, c.code, out)
		case strings.Contains(string(out), "goroutine"):
			t.Errorf("%s: a stack trace:\n%s", c.script, out)
		}
		if ws.CoreDump() {
			t.Errorf("%s: the wrapper dumped core", c.script)
		}
	}
}

// A caller without XDG_RUNTIME_DIR runs its command in the batch scope without it too, as
// a plain run would, though systemd-run is given one to reach the user manager.
func TestALocalRunAddsNoRuntimeDirToTheCommand(t *testing.T) {
	if os.Getenv("OFFLOAD_NOXDG_CHILD") != "" {
		code, _, err := runBatch([]string{"sh", "-c", "echo \"[${XDG_RUNTIME_DIR-unset}]\""}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	if _, err := os.Stat(fmt.Sprintf("/run/user/%d/systemd/private", os.Getuid())); err != nil {
		t.Skip("no user systemd manager")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestALocalRunAddsNoRuntimeDirToTheCommand$")
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "XDG_RUNTIME_DIR=")
	}), "OFFLOAD_NOXDG_CHILD=1")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || !strings.Contains(stdout.String(), "[unset]") {
		t.Errorf("%v: stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "outside the batch scope") {
		t.Errorf("the run skipped the scope: %q", stderr.String())
	}
}

// A runtime dir set but empty reaches the command set and empty, as in a plain run.
func TestALocalRunKeepsAnEmptyRuntimeDir(t *testing.T) {
	if os.Getenv("OFFLOAD_EMPTYXDG_CHILD") != "" {
		code, _, err := runBatch([]string{"sh", "-c", "echo \"[${XDG_RUNTIME_DIR-unset}]\""}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestALocalRunKeepsAnEmptyRuntimeDir$")
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "XDG_RUNTIME_DIR=")
	}), "OFFLOAD_EMPTYXDG_CHILD=1", "XDG_RUNTIME_DIR=")
	cmd.Dir = t.TempDir() // no `bus` here: an empty runtime dir must not be read as the cwd
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || !strings.Contains(stdout.String(), "[]") {
		t.Errorf("%v: stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "outside the batch scope") {
		t.Errorf("an empty XDG_RUNTIME_DIR skipped the scope: %q", stderr.String())
	}
}

// A manager socket left by a dead user manager is no manager, whatever the D-Bus `bus`
// does: the command runs, outside the scope and saying so, where systemd-run would exit 1
// before running it.
func TestALocalRunWithAStaleBusSocketStillRunsTheCommand(t *testing.T) {
	if os.Getenv("OFFLOAD_STALEBUS_CHILD") != "" {
		code, _, err := runBatch([]string{"sh", "-c", "echo ran"}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	// Not t.TempDir or the checkout's tmp/: a socket path must fit in 108 bytes wherever the
	// checkout sits, and t.TempDir's name carries this test's long one.
	dir, err := os.MkdirTemp("", "offload-bus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bus, err := net.Listen("unix", filepath.Join(dir, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close() // a live D-Bus socket, which systemd-run does not use
	if err := os.Mkdir(filepath.Join(dir, "systemd"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "systemd", "private"))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // the socket file stays, with nothing listening
	cmd := exec.Command(os.Args[0], "-test.run=^TestALocalRunWithAStaleBusSocketStillRunsTheCommand$")
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "XDG_RUNTIME_DIR=")
	}), "OFFLOAD_STALEBUS_CHILD=1", "XDG_RUNTIME_DIR="+dir)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || !strings.Contains(stdout.String(), "ran") {
		t.Errorf("%v: stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "outside the batch scope") {
		t.Errorf("stderr %q does not say the run left the scope", stderr.String())
	}
}

// A command run in the batch scope sees exactly the caller's environment, as in a plain
// run: nothing the scope adds (INVOCATION_ID) and nothing the wrapper sets to reach it
// (XDG_RUNTIME_DIR), whether the caller had each, had it empty, or had it relative.
func TestALocalRunGivesTheCommandTheCallersEnvironment(t *testing.T) {
	if os.Getenv("OFFLOAD_ENV_CHILD") != "" {
		code, _, err := runBatch([]string{"env", "-0"}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	if _, err := os.Stat(fmt.Sprintf("/run/user/%d/systemd/private", os.Getuid())); err != nil {
		t.Skip("no user systemd manager")
	}
	for _, extra := range [][]string{
		nil,
		{"INVOCATION_ID=0123456789abcdef0123456789abcdef"},
		{"INVOCATION_ID="},
		{"XDG_RUNTIME_DIR=run/user"},
		{"XDG_RUNTIME_DIR=", "INVOCATION_ID=caller"},
		{fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid())},
	} {
		env := slices.DeleteFunc(os.Environ(), func(e string) bool {
			return strings.HasPrefix(e, "XDG_RUNTIME_DIR=") || strings.HasPrefix(e, "INVOCATION_ID=")
		})
		env = slices.Concat(env, []string{"OFFLOAD_ENV_CHILD=1"}, extra)
		cmd := exec.Command(os.Args[0], "-test.run=^TestALocalRunGivesTheCommandTheCallersEnvironment$")
		cmd.Env = env
		cmd.Dir = t.TempDir()
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Errorf("%q: %v: stderr %q", extra, err, stderr.String())
			continue
		}
		if strings.Contains(stderr.String(), "outside the batch scope") {
			t.Errorf("%q: the run skipped the scope: %q", extra, stderr.String())
		}
		got := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
		slices.Sort(got)
		want := slices.Clone(env)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%q: the command's environment differs from the caller's\nonly the command's: %q\nonly the caller's: %q",
				extra, minus(got, want), minus(want, got))
		}
	}
}

// A caller that blocked signals (pthread_sigmask before exec) has its command start with
// them blocked, as a plain run would, though the Go runtime unblocks SIGTERM.
func TestALocalRunKeepsTheSignalsTheCallerBlocked(t *testing.T) {
	if os.Getenv("OFFLOAD_BLOCK_CHILD") != "" {
		code, _, err := runBatch([]string{"grep", "^SigBlk:", "/proc/self/status"}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	cmd := exec.Command("/usr/bin/env", "--block-signal=USR1,TERM", "--", os.Args[0],
		"-test.run=^TestALocalRunKeepsTheSignalsTheCallerBlocked$")
	cmd.Env = append(os.Environ(), "OFFLOAD_BLOCK_CHILD=1")
	cmd.Dir = t.TempDir()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	_, hex, ok := strings.Cut(stdout.String(), "SigBlk:")
	mask, err := strconv.ParseUint(strings.TrimSpace(strings.SplitN(hex, "\n", 2)[0]), 16, 64)
	if want := uint64(1<<(syscall.SIGUSR1-1) | 1<<(syscall.SIGTERM-1)); !ok || err != nil || mask&want != want {
		t.Errorf("the command's blocked set %q lacks USR1 and TERM", stdout.String())
	}
}

// startupCases is the start-up rule's table, run against commandWords and against the
// launcher's own copy of the rule, so the two cannot drift apart.
func startupCases() []struct{ in, want []string } {
	sh := []string{"/bin/sh", "-c", `exec "$0" "$@"`}
	return []struct{ in, want []string }{
		{[]string{"ls", "a=b"}, []string{"ls", "a=b"}},
		{[]string{"A=1", "_b2=x", "ls"}, []string{"A=1", "_b2=x", "ls"}},
		{[]string{"A=1"}, []string{"A=1"}},
		{nil, nil},
		{[]string{"./d=e/prog", "x"}, slices.Concat(sh, []string{"./d=e/prog", "x"})},
		{[]string{"A=1", "/o/b=1/t"}, slices.Concat([]string{"A=1"}, sh, []string{"/o/b=1/t"})},
		{[]string{"2A=1", "ls"}, slices.Concat(sh, []string{"2A=1", "ls"})},
		{[]string{"=x", "ls"}, slices.Concat(sh, []string{"=x", "ls"})},
		{[]string{"A=1", "a b=c", "x y"}, slices.Concat([]string{"A=1"}, sh, []string{"a b=c", "x y"})},
	}
}

// commandWords starts a command word holding `=` through sh's exec, past the leading
// NAME=value words, and leaves every other argv as it is.
func TestCommandWords(t *testing.T) {
	for _, c := range startupCases() {
		if got := commandWords(c.in); !slices.Equal(got, c.want) {
			t.Errorf("commandWords(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The launcher applies the same start-up rule as commandWords: its block, cut from
// offload-launch between `command_words=("$@")` and the `fi` closing it, gives the same
// words for every case.
func TestTheLauncherStartsCommandsByTheSameRule(t *testing.T) {
	src, err := os.ReadFile("offload-launch")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(src), "\ncommand_words=(\"$@\")\n")
	block, _, closed := strings.Cut(rest, "\nfi\n")
	if !ok || !closed {
		t.Fatal("offload-launch has no command_words block")
	}
	script := "set -euo pipefail\ncommand_words=(\"$@\")\n" + block + "\nfi\n" +
		`((${#command_words[@]} == 0)) || printf '%s\0' "${command_words[@]}"`
	for _, c := range startupCases() {
		out, err := exec.Command("bash", slices.Concat([]string{"-c", script, "_"}, c.in)...).Output()
		if err != nil {
			t.Fatalf("launcher block on %q: %v", c.in, err)
		}
		var got []string
		if len(out) > 0 {
			got = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("launcher block on %q = %q, want %q", c.in, got, c.want)
		}
	}
}

// A local run starts a command whose path holds `=`, with its arguments, rather than env
// reading the path as NAME=value.
func TestALocalRunStartsACommandWhosePathHoldsAnEqualsSign(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d=e")
	out := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho \"$0 [$*] $FOO\" > "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, err := runBatch([]string{"FOO=1", prog, "a b", "c=d"}, false)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v", code, err)
	}
	if got, _ := os.ReadFile(out); string(got) != prog+" [a b c=d] 1\n" {
		t.Errorf("the command wrote %q", got)
	}
}

// minus is what a holds that b does not.
func minus(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(e string) bool { return slices.Contains(b, e) })
}

// Without a user systemd manager (root under sudo) a local run still runs the command,
// outside the batch scope, and says so.
func TestALocalRunWithoutAUserManagerRunsOutsideTheScope(t *testing.T) {
	if os.Getenv("OFFLOAD_NOBUS_CHILD") != "" {
		code, _, err := runBatch([]string{"sh", "-c", "echo RAN"}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestALocalRunWithoutAUserManagerRunsOutsideTheScope$")
	cmd.Env = append(os.Environ(), "OFFLOAD_NOBUS_CHILD=1", "XDG_RUNTIME_DIR="+t.TempDir())
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || !strings.Contains(stdout.String(), "RAN") {
		t.Fatalf("%v: stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "outside the batch scope") {
		t.Errorf("stderr does not say the scope was skipped: %q", stderr.String())
	}
}

// A process group with no parent in another group of its session (setsid) is orphaned: no
// shell can continue it, so a caught SIGTSTP is dropped. One whose parent is in the same
// session is not.
func TestGroupOrphaned(t *testing.T) {
	if os.Getenv("OFFLOAD_ORPHAN_CHILD") != "" {
		if groupOrphaned() {
			os.Exit(10)
		}
		os.Exit(11)
	}
	run := func(attr *syscall.SysProcAttr) int {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGroupOrphaned$")
		cmd.Env = append(os.Environ(), "OFFLOAD_ORPHAN_CHILD=1")
		cmd.SysProcAttr = attr
		cmd.Run()
		return cmd.ProcessState.ExitCode()
	}
	if got := run(&syscall.SysProcAttr{Setsid: true}); got != 10 {
		t.Errorf("a session leader's group: exit %d, want 10 (orphaned)", got)
	}
	if got := run(&syscall.SysProcAttr{Setpgid: true}); got != 11 {
		t.Errorf("a group whose parent is in the session: exit %d, want 11 (not orphaned)", got)
	}
}

// Even with no terminal, a SIGTSTP to a wrapper in an orphaned group stops nothing: the
// command finishes, as the kernel would have discarded the stop.
func TestCtrlZInAnOrphanedGroupStopsNothing(t *testing.T) {
	if os.Getenv("OFFLOAD_ORPHAN_TSTP_CHILD") != "" {
		code, _ := runLocal([]string{"sh", "-c", "kill -TSTP $PPID; sleep 0.3; echo finished"}, false)
		os.Exit(code)
	}
	cmd := exec.Command("timeout", "-s", "KILL", "10", os.Args[0], "-test.run=^TestCtrlZInAnOrphanedGroupStopsNothing$")
	cmd.Env = append(os.Environ(), "OFFLOAD_ORPHAN_TSTP_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "finished") {
		t.Errorf("%v\n%s", err, out)
	}
}

// A terminal local run whose command is stopped and then continued other than by the
// shell's fg (`kill -CONT <pid>` aimed at the command, or a group stop undone at once)
// does not leave the wrapper stopped for good: it runs on and ends with the command.
func TestALocalRunContinuedFromOutsideDoesNotStayStopped(t *testing.T) {
	if script := os.Getenv("OFFLOAD_OUTSIDE_CONT_CHILD"); script != "" {
		code, err := runLocal([]string{"sh", "-c", script}, false)
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	for name, script := range map[string]string{
		// The command stops itself; a process outside the group continues it by pid.
		"by pid": `setsid sh -c "sleep 0.5; kill -CONT $$" & kill -STOP $$; echo resumed`,
		// The wrapper and the command are stopped and continued from outside before the
		// wrapper reads the stop (a group stop would stop `script` too).
		"both": `setsid sh -c "kill -STOP $$ $PPID; sleep 0.3; kill -CONT $$ $PPID" & wait; echo resumed`,
	} {
		t.Run(name, func(t *testing.T) {
			run := os.Args[0] + " -test.run=^TestALocalRunContinuedFromOutsideDoesNotStayStopped$ </dev/null"
			cmd := exec.Command("timeout", "-s", "KILL", "10", "script", "-qec", run, "/dev/null")
			cmd.Env = append(os.Environ(), "OFFLOAD_OUTSIDE_CONT_CHILD="+script)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "resumed") {
				t.Errorf("err %v (a stopped wrapper is killed by the timeout)\n%s", err, out)
			}
		})
	}
}

// The watcher continues a stopped wrapper once the command runs again, and ends without
// signalling one that runs again by itself or is gone.
func TestWatchContinue(t *testing.T) {
	start := func(script string) *exec.Cmd {
		c := exec.Command("sh", "-c", script)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		return c
	}
	stoppedUntil := func(c *exec.Cmd) {
		for procState(c.Process.Pid) != 'T' {
			time.Sleep(10 * time.Millisecond)
		}
	}
	cmd, wrapper := start("sleep 30"), start("sleep 30")
	syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP)
	syscall.Kill(wrapper.Process.Pid, syscall.SIGSTOP)
	stoppedUntil(cmd)
	stoppedUntil(wrapper)
	done := make(chan int)
	go func() { done <- watchContinue(fmt.Sprintf("%d %d", cmd.Process.Pid, wrapper.Process.Pid)) }()
	time.Sleep(300 * time.Millisecond)
	if procState(wrapper.Process.Pid) != 'T' {
		t.Fatal("the wrapper was continued while the command stayed stopped")
	}
	syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not end once the command ran again")
	}
	if s := procState(wrapper.Process.Pid); s == 'T' {
		t.Error("the wrapper stayed stopped after the command ran again")
	}
	// A wrapper that is gone ends the watcher.
	gone := start("true")
	gone.Wait()
	if code := watchContinue(fmt.Sprintf("%d %d", cmd.Process.Pid, gone.Process.Pid)); code != 0 {
		t.Errorf("watchContinue on a gone wrapper = %d", code)
	}
}

// A stop already undone when the wrapper reads it stops nothing: the shell would otherwise
// see the job stopped, take the terminal back and leave it in the background.
func TestStopWithCommandIgnoresAStopAlreadyUndone(t *testing.T) {
	defer func(f func()) { suspendSelf = f }(suspendSelf)
	suspended := false
	suspendSelf = func() { suspended = true }
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { c.Process.Kill(); c.Wait() }()
	stopWithCommand(c.Process.Pid)
	if suspended {
		t.Error("the wrapper stopped for a command that runs")
	}
	syscall.Kill(c.Process.Pid, syscall.SIGSTOP)
	for procState(c.Process.Pid) != 'T' {
		time.Sleep(10 * time.Millisecond)
	}
	stopWithCommand(c.Process.Pid)
	if !suspended {
		t.Error("the wrapper did not stop with a stopped command")
	}
}

// suspendSelf returns only once the wrapper has been stopped and continued: kill returns
// before the stop takes hold, and a caller acting at once would send `sig CONT` to a
// remote job, or end the watcher, while it is still running.
func TestSuspendSelfReturnsOnlyAfterTheStop(t *testing.T) {
	if os.Getenv("OFFLOAD_SUSPEND_CHILD") != "" {
		for i := 0; i < 20; i++ {
			suspendSelf()
			os.Stdout.WriteString("returned\n")
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSuspendSelfReturnsOnlyAfterTheStop$")
	cmd.Env = append(os.Environ(), "OFFLOAD_SUSPEND_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	lines := make(chan string)
	go func() {
		r := bufio.NewScanner(out)
		for r.Scan() {
			lines <- r.Text()
		}
		close(lines)
	}()
	pid := cmd.Process.Pid
	for i := 0; i < 20; i++ {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil); err != nil || !ws.Stopped() {
			t.Fatalf("round %d: the child did not stop: %v %v", i, ws, err)
		}
		select {
		case l := <-lines:
			t.Fatalf("round %d: suspendSelf returned (%q) before the child was continued", i, l)
		case <-time.After(20 * time.Millisecond):
		}
		syscall.Kill(pid, syscall.SIGCONT)
		select {
		case <-lines:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: suspendSelf did not return once continued", i)
		}
	}
}

// Time the wrapper spends stopped (Ctrl-Z) before `ready` is not the launcher's setup
// time: on resume, a `ready` that arrived meanwhile is taken, not a fallback past
// setupLimit.
func TestCtrlZPastTheSetupLimitBeforeReadyStillRunsRemotely(t *testing.T) {
	defer func(f func() bool) { groupOrphaned = f }(groupOrphaned)
	groupOrphaned = func() bool { return false }
	defer func(f func()) { suspendSelf = f }(suspendSelf)
	defer func(h, l time.Duration) { heartbeatEvery, setupLimit = h, l }(heartbeatEvery, setupLimit)
	heartbeatEvery, setupLimit = 20*time.Millisecond, 300*time.Millisecond
	suspendSelf = func() { time.Sleep(600 * time.Millisecond); syscall.Kill(os.Getpid(), syscall.SIGCONT) }
	// select picks among ready cases at random, so a few runs.
	for i := 0; i < 3; i++ {
		job, release := heldJob(t, "exit 0")
		go func() {
			time.Sleep(100 * time.Millisecond)
			syscall.Kill(os.Getpid(), syscall.SIGTSTP)
		}()
		// `ready` arrives while the wrapper is stopped, past setupLimit from the start.
		ctl := exec.Command("sh", "-c", `sleep 0.3; echo ready; while read l; do
			if [ "$l" = go ]; then echo started; `+release+` echo 'status exit 0'; fi; done`)
		out, _ := superviseWith(t, job, ctl, 0)
		if out.fallback || out.code != 0 {
			t.Fatalf("run %d: want the remote's clean exit, got %+v", i, out)
		}
	}
}
