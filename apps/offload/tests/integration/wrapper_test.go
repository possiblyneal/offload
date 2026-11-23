package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A terminal's Ctrl-C reaches the whole foreground process group. The wrapper must be
// the only one in it that hears the signal, so the job gets it over the control channel
// and its handler runs, instead of the ssh clients dying under it.
func TestInterruptToTheWrappersGroupReachesTheJobsHandler(t *testing.T) {
	dir := repo(t)
	cmd := exec.Command(offloadBin, "--where", "server", "--", "sh", "-c",
		`trap 'echo handled > h; exit 0' INT; touch up; while :; do sleep 0.1; done`)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")

	syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitOf(err); code != 0 {
			t.Errorf("exit %d, want the handler's 0; stderr %q", code, stderr.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("offload still running 8 s after the group SIGINT; stderr %q", stderr.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "h")); string(b) != "handled\n" {
		t.Errorf("the job's INT handler did not run; stderr %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "lost contact") {
		t.Errorf("an ordinary interrupt reported lost contact: %q", stderr.String())
	}
}

// With no free slot the job falls back to a local run, which must get the caller's
// stdin whole: none of it may have gone to the remote first.
func TestStdinFlagGivesTheWholeInputToALocalFallback(t *testing.T) {
	dir := repo(t)
	for i := 0; i < 2; i++ {
		start(t, offloadBin, "server", dir, nil, "sleep", "20")
	}
	waitFor(t, func() bool { return freeSlots(t, 2) == "0" }, 20*time.Second, "both slots taken")
	in := strings.Repeat("line of input\n", 20000)
	out, code := shell(t, dir, `"$OFFLOAD" --where server --stdin -- wc -l`, strings.NewReader(in))
	if code != 0 || strings.TrimSpace(out) != "20000" {
		t.Errorf("code %d, wc -l %q: want all 20000 lines", code, out)
	}
	waitFor(t, func() bool { return freeSlots(t, 2) == "2" }, 30*time.Second, "both slots free again")
}
