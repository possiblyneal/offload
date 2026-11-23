package integration

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEmptyArgumentsReachTheRemoteJobIntact(t *testing.T) {
	r := execIn(t, repo(t), true, "printf", "[%s]", "a", "", "b")
	if r.stdout != "[a][][b]" || r.code != 0 {
		t.Errorf("stdout %q, exit %d; want [a][][b], 0", r.stdout, r.code)
	}
}

func TestInterruptAimedAtTheWrapperAloneReachesALocalRunsHandler(t *testing.T) {
	dir := repo(t)
	script := `import signal, sys, time
def bye(*a):
    open("handled", "w").write("handled")
    sys.exit(0)
signal.signal(signal.SIGINT, bye)
open("up", "w").write("up")
time.sleep(7316)`
	cmd, _, stderr := start(t, offloadBin, "local", dir, nil, "python3", "-c", script)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 20*time.Second, "up")
	cmd.Process.Signal(syscall.SIGINT) // the wrapper only, not its group
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("offload still running 8 s after SIGINT; stderr %q", stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "handled")); string(b) != "handled" {
		t.Errorf("the job's SIGINT handler did not run")
	}
}

func TestCtrlZSuspendsTheRemoteJobPastTheSilenceLimitAndFgResumesIt(t *testing.T) {
	dir := repo(t)
	// The job's own clock runs on while it is stopped, so it ends soon after it is resumed.
	// Its sleep outlasts the stop by far, so the job seen on the server is the stopped one,
	// never one that ran on and has not finished yet.
	cmd, stdout, stderr := start(t, offloadBin, "server", dir, nil, "sh", "-c", "hostname > ran; sleep 25; echo finished 7317")
	// jobState is the state of the job's sleep on the server (T when stopped), or "".
	jobState := func() string {
		out, _ := sh(t, true, `for p in $(pgrep -fx 'sleep 25'); do ps -o stat= -p "$p"; done`)
		return strings.TrimSpace(out)
	}
	// The job's own mark, not a process match: the launcher's command line names the job too.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ran")); return err == nil }, 30*time.Second, "the job to start")
	// A stop landing between the two leaves sh stopped before it starts the sleep, with no
	// sleep to find.
	waitFor(t, func() bool { return jobState() != "" }, 10*time.Second, "the job's sleep")
	cmd.Process.Signal(syscall.SIGTSTP)
	time.Sleep(time.Second)
	if st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)); !strings.Contains(string(st), ") T ") {
		t.Fatalf("the wrapper did not stop on SIGTSTP: %s", st)
	}
	if st := jobState(); !strings.HasPrefix(st, "T") {
		t.Fatalf("the remote job is %q after Ctrl-Z, want stopped (T)", st)
	}
	// The wrapper stops itself, so it sends no heartbeat: longer than the launcher's
	// 10 s silence limit the job must still be there, stopped.
	time.Sleep(13 * time.Second)
	if st := jobState(); !strings.HasPrefix(st, "T") {
		t.Fatalf("the remote job is %q past the silence limit, want stopped (T); stderr %q", st, stderr)
	}
	cmd.Process.Signal(syscall.SIGCONT) // what the shell's fg does
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ran, _ := os.ReadFile(filepath.Join(dir, "ran"))
		if exitOf(err) != 0 || stdout.String() != "finished 7317\n" || stderr.Len() != 0 || string(ran) != serverHostname(t)+"\n" {
			t.Errorf("exit %d, stdout %q, stderr %q, ran on %q: want one run on the server, nothing said", exitOf(err), stdout, stderr, ran)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("job did not finish after fg; stdout %q stderr %q", stdout, stderr)
	}
}

// Ctrl-Z while the remote still sets the job up stops the run as it would a plain one:
// longer than the launcher's 30 s wait for `go`, the remote must not give up on it, and
// after fg the job runs there with nothing said. A watcher on the server times the stop and
// reports whether it stopped the job's group: one case needs it to land before the job has
// a group to stop, the other after. The stop races the setup, so a run where it landed on
// the wrong side is killed and tried again.
func TestCtrlZDuringSetupPastTheWaitForGoStillRunsTheJobRemotely(t *testing.T) {
	srv := serverHostname(t)
	for _, c := range []struct {
		name, at string
		stopped  bool
	}{{"before the job's group", "in", false}, {"after the job's group", "pid", true}} {
		// attempt reports false when the stop landed on the wrong side of the group.
		attempt := func() bool {
			watch := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr, `sudo bash -c '
old=$(ls /run/offload-ctl 2>/dev/null); echo armed
while :; do for d in /run/offload-ctl/*/; do
  if [[ -e ${d}`+c.at+` ]] && ! grep -qx "$(basename "$d")" <<<"$old"; then
    echo now; sleep 2
    ps -o stat= -p "$(cat ${d}pid 2>/dev/null || echo 0)" || echo none; exit
  fi
done; done'`)
			out, err := watch.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := watch.Start(); err != nil {
				t.Fatal(err)
			}
			defer watch.Wait()
			lines := bufio.NewScanner(out)
			if !lines.Scan() || lines.Text() != "armed" {
				t.Fatalf("the server-side watcher did not start: %q", lines.Text())
			}
			cmd, stdout, stderr := start(t, offloadBin, "server", repo(t), nil, "hostname")
			if !lines.Scan() || lines.Text() != "now" {
				t.Fatalf("%s: the watcher saw no new job: %q", c.name, lines.Text())
			}
			cmd.Process.Signal(syscall.SIGTSTP)
			lines.Scan()
			if strings.Contains(lines.Text(), "T") != c.stopped {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				cmd.Wait()
				return false
			}
			time.Sleep(35 * time.Second)
			cmd.Process.Signal(syscall.SIGCONT)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if exitOf(err) != 0 || stdout.String() != srv+"\n" || stderr.Len() != 0 {
					t.Errorf("stopped %s: exit %d, stdout %q, stderr %q: want the server's hostname and nothing else", c.name, exitOf(err), stdout, stderr)
				}
			case <-time.After(30 * time.Second):
				t.Errorf("stopped %s: no end 30 s after fg; stdout %q stderr %q", c.name, stdout, stderr)
			}
			return true
		}
		tries := 1
		for ; !attempt(); tries++ {
			if tries == 10 {
				t.Fatalf("no stop landed %s in 10 tries", c.name)
			}
		}
		t.Logf("stopped %s on try %d", c.name, tries)
	}
}

// A Ctrl-C while the remote's job half runs its helpers (offload-bind, offload-wait before
// its handlers) ends the run by SIGINT with nothing said, as a plain command would: before
// the launcher ignored SIGINT there, one landed in a python3 that died with a traceback
// and the wrapper exited 130 or 1. A watcher on the server marks when the job half has
// passed its wait for `go` (`released`); the Ctrl-C follows at a spread of delays across
// the helpers it then starts.
func TestCtrlCDuringTheJobsSetupEndsTheRunBySIGINTQuietly(t *testing.T) {
	for _, delay := range []time.Duration{0, 20, 40, 70, 100, 150, 200, 300, 450} {
		watch := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr, `sudo bash -c '
old=$(ls /run/offload-ctl 2>/dev/null); echo armed
while :; do for d in /run/offload-ctl/*/; do
  if [[ -e ${d}released ]] && ! grep -qx "$(basename "$d")" <<<"$old"; then echo now; exit; fi
done; done'`)
		out, err := watch.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := watch.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(out)
		if !lines.Scan() || lines.Text() != "armed" {
			t.Fatalf("the server-side watcher did not start: %q", lines.Text())
		}
		cmd, stdout, stderr := start(t, offloadBin, "server", repo(t), nil, "sleep", "20")
		if !lines.Scan() || lines.Text() != "now" {
			t.Fatalf("the watcher saw no new job: %q", lines.Text())
		}
		watch.Wait()
		time.Sleep(delay * time.Millisecond)
		cmd.Process.Signal(syscall.SIGINT)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			if signalOf(cmd.ProcessState) != int(syscall.SIGINT) || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("Ctrl-C %d ms after release: %v, stdout %q, stderr %q; want death by SIGINT, nothing said", delay, cmd.ProcessState, stdout, stderr)
			}
		case <-time.After(30 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatalf("Ctrl-C %d ms after release: no end in 30 s", delay)
		}
	}
}

// A SIGQUIT while the wrapper still places the job, before it relays signals, ends it by
// SIGQUIT with nothing on stderr, not by the Go runtime's goroutine dump and exit 2.
func TestSIGQUITDuringPlacementEndsTheWrapperQuietly(t *testing.T) {
	for _, delay := range []time.Duration{100, 300} {
		cmd, stdout, stderr := start(t, offloadBin, "server", repo(t), nil, "sleep", "20")
		time.Sleep(delay * time.Millisecond)
		cmd.Process.Signal(syscall.SIGQUIT)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			if signalOf(cmd.ProcessState) != int(syscall.SIGQUIT) || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("SIGQUIT at %d ms: %v, stdout %q, stderr %q; want death by SIGQUIT, nothing said", delay, cmd.ProcessState, stdout, stderr)
			}
		case <-time.After(30 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatalf("SIGQUIT at %d ms: no end in 30 s", delay)
		}
	}
}
