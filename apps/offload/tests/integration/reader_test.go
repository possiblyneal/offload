package integration

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readerGone runs sh -c script (offloaded or not) in a fresh repository with stdout on a
// pipe whose reader reads one line and closes, as `cmd | head -1` does. It returns the
// exit code, the signal the run died by, and the side-effect file `ran`.
func readerGone(t *testing.T, offloaded bool, script string) (code, sig int, ran, stderr string) {
	t.Helper()
	dir := repo(t)
	argv := []string{"sh", "-c", script}
	if offloaded {
		argv = append([]string{offloadBin, "--where", "server", "--"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	bufio.NewReader(r).ReadString('\n')
	r.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(40 * time.Second):
		syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
		t.Errorf("offloaded=%v still running 40 s after its reader closed", offloaded)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ran"))
	return exitOf(err), signalOf(cmd.ProcessState), string(b), errBuf.String()
}

// A caller's reader gone ends an offloaded run by SIGPIPE, as it ends a local one, and the
// job does not run on to its end; writes in the round trip before the signal lands may.
func TestAReaderGoneEndsAnOffloadedRunBySIGPIPE(t *testing.T) {
	script := `echo a; sleep 2; echo b; echo after-b >> ran; sleep 2; echo end >> ran`
	lc, ls, lran, _ := readerGone(t, false, script)
	rc, rs, rran, rerr := readerGone(t, true, script)
	if lc != 141 || ls != int(syscall.SIGPIPE) || lran != "" {
		t.Fatalf("the local run is not the reference expected: code %d signal %d ran %q", lc, ls, lran)
	}
	if rc != lc || rs != ls || strings.Contains(rran, "end") || rerr != "" {
		t.Errorf("offloaded: code %d signal %d ran %q stderr %q; want code %d signal %d, no end, no message",
			rc, rs, rran, rerr, lc, ls)
	}
}

// An endless producer whose reader is gone ends as a local one does.
func TestAReaderGoneEndsAnEndlessProducer(t *testing.T) {
	lc, ls, _, _ := readerGone(t, false, "yes")
	rc, rs, _, rerr := readerGone(t, true, "yes")
	if rc != lc || rs != ls {
		t.Errorf("offloaded: code %d signal %d stderr %q; local: code %d signal %d", rc, rs, rerr, lc, ls)
	}
}

// SIGTERM to the remote's launcher (an admin's kill, sshd stopping) once the job runs: the
// launcher stops the job and says so, rather than leaving the wrapper to time out on a
// silence that claims writes may still land.
func TestALauncherStoppedBySIGTERMSaysSo(t *testing.T) {
	cmd, stderr := startRunningWithStderr(t, 7391)
	launcher := strings.TrimSpace(mustSh(t, true,
		`c=$(cut -d: -f3 "/proc/$(pgrep -nf '^[s]leep 7391')/cgroup"); c=${c##*offload-job-}; echo "${c%.scope}"`))
	began := time.Now()
	mustSh(t, true, "sudo kill -TERM "+launcher)
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the wrapper still waits 30 s after its launcher got SIGTERM; stderr %q", stderr)
	}
	took := time.Since(began)
	if code := exitOf(waitErr); code != 255 || !strings.Contains(stderr.String(), "launcher died") ||
		strings.Contains(stderr.String(), "lost contact") || took > 4*time.Second {
		t.Errorf("exit %d after %v, stderr %q; want 255 at once, saying the launcher died", code, took, stderr)
	}
	waitOnServer(t, "[s]leep 7391", false, 5*time.Second)
}

// Output to a destination that refuses it (/dev/full) ends as a local run does: the job's
// own code, its later work still done, and the error said.
func TestAFullDestinationKeepsTheJobsOwnCode(t *testing.T) {
	run := func(offloaded bool) (int, string, string) {
		dir := repo(t)
		argv := []string{"sh", "-c", "head -c 3000000 /dev/zero; echo done >ran"}
		if offloaded {
			argv = append([]string{offloadBin, "--where", "server", "--"}, argv...)
		}
		full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer full.Close()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Stdout = full
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err = cmd.Run()
		b, _ := os.ReadFile(filepath.Join(dir, "ran"))
		return exitOf(err), string(b), errBuf.String()
	}
	lc, lran, _ := run(false)
	rc, rran, rerr := run(true)
	if rc != lc || rran != lran || !strings.Contains(rerr, "no space left on device") {
		t.Errorf("offloaded: code %d ran %q stderr %q; local: code %d ran %q", rc, rran, rerr, lc, lran)
	}
}

// A reader gone while the launcher still sets the job up (`offload -- cmd | true`) does not
// end the setup: the job runs and dies by SIGPIPE at its first write, as a local run does.
// The close lands at several points of the setup, since its length varies.
func TestAReaderGoneDuringSetupLetsTheJobRun(t *testing.T) {
	run := func(offloaded bool, after time.Duration) (int, int, string, string) {
		dir := repo(t)
		argv := []string{"sh", "-c", "echo started >> ran; sleep 2; echo out; echo after >> ran"}
		if offloaded {
			argv = append([]string{offloadBin, "--where", "server", "--"}, argv...)
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Stdout = w
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w.Close()
		time.Sleep(after)
		r.Close()
		err = cmd.Wait()
		b, _ := os.ReadFile(filepath.Join(dir, "ran"))
		return exitOf(err), signalOf(cmd.ProcessState), string(b), errBuf.String()
	}
	lc, ls, lran, _ := run(false, 0)
	if lc != 141 || ls != int(syscall.SIGPIPE) || lran != "started\n" {
		t.Fatalf("the local run is not the reference expected: code %d signal %d ran %q", lc, ls, lran)
	}
	for after := 200 * time.Millisecond; after <= 1600*time.Millisecond; after += 200 * time.Millisecond {
		rc, rs, rran, rerr := run(true, after)
		if rc != lc || rs != ls || rran != lran || rerr != "" {
			t.Errorf("reader gone after %v: code %d signal %d ran %q stderr %q; want code %d signal %d ran %q, no message",
				after, rc, rs, rran, rerr, lc, ls, lran)
		}
	}
}
