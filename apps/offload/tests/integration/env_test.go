package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// launcherUnderTest builds an offload binary that runs this checkout's launcher, copied to
// a path of its own on the server, and returns the binary.
func launcherUnderTest(t *testing.T, name string) string {
	t.Helper()
	return patchedLauncher(t, name, "", "")
}

// patchedLauncher is launcherUnderTest for a copy of the launcher whose first occurrence of
// old is replaced by new, so a test can hold or break the launcher at a chosen step.
func patchedLauncher(t *testing.T, name, old, new string) string {
	t.Helper()
	copyPath := "/usr/local/libexec/offload-launch-it-" + name
	src, err := os.ReadFile("../../src/offload-launch")
	if err != nil {
		t.Fatal(err)
	}
	patched := string(src)
	if old != "" {
		if patched = strings.Replace(patched, old, new, 1); patched == string(src) {
			t.Fatalf("no %q in the launcher to patch", old)
		}
	}
	put := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr,
		"sudo tee "+copyPath+" >/dev/null && sudo chmod 755 "+copyPath)
	put.Stdin = strings.NewReader(patched)
	if out, err := put.CombinedOutput(); err != nil {
		t.Fatalf("install launcher copy: %v %s", err, out)
	}
	t.Cleanup(func() { sh(t, true, "sudo rm -f "+copyPath) })
	conf, _ := filepath.Abs("../../config")
	bin := filepath.Join(t.TempDir(), "offload")
	build := exec.Command("go", "build", "-ldflags", ldflags(conf)+" -X main.launcherPath="+copyPath, "-o", bin, "devex/offload")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// heldLauncher is a launcher copy that stops for secs seconds just before the line before,
// and the file on the server that it creates once it is held there.
func heldLauncher(t *testing.T, name, before string, secs int) (bin, armed string) {
	t.Helper()
	armed = "/run/offload-it-armed-" + name
	sh(t, true, "sudo rm -f "+armed)
	t.Cleanup(func() { sh(t, true, "sudo rm -f "+armed) })
	bin = patchedLauncher(t, name, "\n"+before+"\n", fmt.Sprintf("\ntouch %s; sleep %d\n%s\n", armed, secs, before))
	return bin, armed
}

// awaitArmed waits for a heldLauncher's job to be held.
func awaitArmed(t *testing.T, armed string) {
	t.Helper()
	waitFor(t, func() bool { _, err := sh(t, true, "test -e "+armed); return err == nil }, 30*time.Second, "the job to be held")
}

// An open-file limit too low for the remote's own helpers still runs the job, as a local
// run would: the limit is set on the job alone.
func TestALowOpenFileLimitStillRunsTheJob(t *testing.T) {
	const caller = `ulimit -Sn 12; exec "$@"`
	local := execIn(t, repo(t), false, "sh", "-c", caller, "sh", "sh", "-c", "ulimit -Sn")
	remote := execIn(t, repo(t), false, "sh", "-c", caller, "sh", offloadBin, "--where", "server", "--", "sh", "-c", "ulimit -Sn")
	if local.stdout != "12\n" {
		t.Fatalf("the local run printed %q: the probe is wrong", local.stdout)
	}
	same(t, local, remote)
}

// The job runs with the caller's umask and soft open-file limit, as it would locally, not
// with what sudo on the remote leaves (0022, 1024).
func TestJobHasTheCallersUmaskAndOpenFileLimit(t *testing.T) {
	const inner = `echo x > f; mkdir d; umask; ulimit -Sn; stat -c '%A %n' f d`
	const caller = `umask 077; ulimit -Sn 777; exec "$@"`
	local := execIn(t, repo(t), false, "sh", "-c", caller, "sh", "sh", "-c", inner)
	remote := execIn(t, repo(t), false, "sh", "-c", caller, "sh", offloadBin, "--where", "server", "--", "sh", "-c", inner)
	if want := "0077\n777\n-rw------- f\ndrwx------ d\n"; local.stdout != want {
		t.Fatalf("the local run printed %q, want %q: the probe is wrong", local.stdout, want)
	}
	same(t, local, remote)
}

// A signal the caller ignored (trap with an empty action) stays ignored for the job, as it does
// for a local run.
func TestJobKeepsTheSignalsTheCallerIgnored(t *testing.T) {
	const inner = `grep SigIgn /proc/self/status`
	const caller = `trap '' USR1 TERM; exec "$@"`
	local := execIn(t, repo(t), false, "sh", "-c", caller, "sh", offloadBin, "--where", "local", "--", "sh", "-c", inner)
	remote := execIn(t, repo(t), false, "sh", "-c", caller, "sh", offloadBin, "--where", "server", "--", "sh", "-c", inner)
	if !strings.Contains(local.stdout, "4200") {
		t.Fatalf("the local run printed %q, want USR1 and TERM ignored: the probe is wrong", local.stdout)
	}
	same(t, local, remote)
}

// A job starts with the signals its caller blocked still blocked, as a plain run does,
// though the Go runtime unblocks SIGTERM in the wrapper.
func TestJobKeepsTheSignalsTheCallerBlocked(t *testing.T) {
	inner := []string{"grep", "SigBlk", "/proc/self/status"} // no sh: dash clears the mask
	caller := []string{"/usr/bin/env", "--block-signal=USR1,TERM", "--", offloadBin}
	local := execIn(t, repo(t), false, slices.Concat(caller, []string{"--where", "local", "--"}, inner)...)
	remote := execIn(t, repo(t), false, slices.Concat(caller, []string{"--where", "server", "--"}, inner)...)
	if !strings.Contains(local.stdout, "4200") {
		t.Fatalf("the local run printed %q, want USR1 and TERM blocked", local.stdout)
	}
	same(t, local, remote)
}

// A command whose path holds `=` runs as in a plain run, with the caller's blocked
// signals and leading NAME=value words: env alone would read the path as an assignment and
// run its first argument instead, or print the environment and exit 0.
func TestACommandWhosePathHoldsAnEqualsSignRuns(t *testing.T) {
	dir := repo(t)
	if err := os.Mkdir(filepath.Join(dir, "d=e"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The read loop runs in the script's own shell, so it shows the mask exec left it.
	prog := "#!/bin/sh\necho \"RAN $0 [$*] FOO=$FOO\"\nwhile read -r l; do case $l in SigBlk*) echo \"$l\" ;; esac; done </proc/self/status\n"
	if err := os.WriteFile(filepath.Join(dir, "d=e/prog"), []byte(prog), 0o755); err != nil {
		t.Fatal(err)
	}
	// The plain run is a shell's: env itself would read the path as an assignment.
	block := []string{"/usr/bin/env", "--block-signal=USR1,TERM", "--"}
	for _, c := range []struct {
		line string
		argv []string
	}{
		{`exec ./d=e/prog 'a b' c`, []string{"./d=e/prog", "a b", "c"}},
		{`exec ./d=e/prog`, []string{"./d=e/prog"}},
		{`export FOO=1; exec ./d=e/prog x=y`, []string{"FOO=1", "./d=e/prog", "x=y"}},
	} {
		argv := c.argv
		plain := execIn(t, dir, false, slices.Concat(block, []string{"sh", "-c", c.line})...)
		if !strings.Contains(plain.stdout, "RAN ./d=e/prog") || !strings.Contains(plain.stdout, "4200") {
			t.Fatalf("the plain run printed %q: the probe is wrong", plain.stdout)
		}
		for _, where := range []string{"local", "server"} {
			got := execIn(t, dir, false, slices.Concat(block, []string{offloadBin, "--where", where, "--"}, argv)...)
			same(t, plain, got)
		}
	}
}

// A caller variable named like one of the launcher's own must change nothing about who the
// job runs as, where, or in which network namespace, and the job still sees it.
func TestCallerVariablesNamedLikeTheLaunchersOwnDoNotChangeTheJob(t *testing.T) {
	bin := launcherUnderTest(t, "env")
	dir := repo(t)
	_, stdout, stderr := start(t, bin, "server", dir,
		[]string{"uid=0", "gid=0", "groups=0", "netns=/offload-it-no-netns", "cwd=/offload-it-no-cwd", "merge=1"},
		"sh", "-c", `echo "$(id -u) $(id -g) $(pwd)"; echo "$uid $gid $groups $netns $cwd $merge"; sed 1,2d /proc/net/dev | cut -d: -f1 | tr -d ' ' | sort | tr '\n' ,`)
	waitFor(t, func() bool { return strings.Contains(stdout.String(), "eth0") }, 60*time.Second, "the job's output; stderr "+stderr.String())
	lines := strings.Split(stdout.String(), "\n")
	local, _ := exec.Command("sh", "-c", `echo "$(id -u) $(id -g)"`).Output()
	if want := strings.TrimSpace(string(local)) + " " + dir; lines[0] != want {
		t.Errorf("uid, gid and cwd %q, want %q", lines[0], want)
	}
	if want := "0 0 0 /offload-it-no-netns /offload-it-no-cwd 1"; lines[1] != want {
		t.Errorf("the job's own variables %q, want %q", lines[1], want)
	}
	if lines[2] != "eth0,lo," {
		t.Errorf("network interfaces %q, want only eth0,lo: the job left its own namespace", lines[2])
	}
}

// The probe reports on stderr, which the job shares with every launcher step after the
// pivot: the only steps that could find a library installed on dev's filesystem.
const preloadSrc = `#include <stdio.h>
#include <unistd.h>
__attribute__((constructor)) static void f(void) {
  fprintf(stderr, "offload-it-preload uid=%d\n", (int)geteuid());
}
`

// LD_PRELOAD in the caller's environment reaches the job, never a step of the launcher that
// runs as root.
func TestCallerLDPreloadIsNotLoadedByARootLauncherStep(t *testing.T) {
	bin := launcherUnderTest(t, "preload")
	work := t.TempDir()
	c := filepath.Join(work, "p.c")
	if err := os.WriteFile(c, []byte(preloadSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	const lib = "/usr/local/lib/offload-it-preload.so"
	so := filepath.Join(work, "p.so")
	if out, err := exec.Command("gcc", "-shared", "-fPIC", "-o", so, c).CombinedOutput(); err != nil {
		t.Fatalf("build preload: %v %s", err, out)
	}
	if out, err := exec.Command("sudo", "install", "-m", "755", so, lib).CombinedOutput(); err != nil {
		t.Fatalf("install preload: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("sudo", "rm", "-f", lib).Run() })

	cmd, _, stderr := start(t, bin, "server", repo(t), []string{"LD_PRELOAD=" + lib}, "true")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("job: %v %s", err, stderr)
	}
	got := stderr.String()
	if !strings.Contains(got, "offload-it-preload uid=1000") {
		t.Fatalf("the job never loaded the library (%q): the probe sees nothing", got)
	}
	if strings.Contains(got, "offload-it-preload uid=0") {
		t.Errorf("a root launcher step loaded the caller's LD_PRELOAD: %q", got)
	}
}

// With the offload firewall absent the launcher refuses before `ready`, so the job runs
// nowhere on the server and the wrapper falls back to a local run.
func TestLauncherRefusesWhenTheOffloadFirewallIsMissing(t *testing.T) {
	bin := launcherUnderTest(t, "nofw")
	mustSh(t, true, "sudo nft delete table inet offload_job")
	t.Cleanup(func() { mustSh(t, true, "sudo systemctl restart offload-net.service") })
	here, _ := os.Hostname()
	code, stdout, stderr := runOffload(t, bin, repo(t), autoEnv, "--where", "server", "--", "hostname")
	if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally") {
		t.Errorf("code %d, stdout %q, stderr %q: want a local run", code, stdout, stderr)
	}
}

// A descriptor the caller closed stays closed for the command: `yes >&-` ends with EBADF
// as it does plainly, rather than writing to /dev/null forever, and a closed stdio keeps
// the job here, where it can stay closed.
func TestAClosedStdioStaysClosedForTheCommand(t *testing.T) {
	dir := repo(t)
	run := func(line string) (string, string) {
		cmd := exec.Command("timeout", "20", "sh", "-c", line+`; echo "rc=$?"`)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Run()
		return stdout.String(), stderr.String()
	}
	for _, c := range []struct{ flags, cmd, redir string }{
		{"", "yes", ">&-"},
		{"", "sh -c 'echo out'", ">&-"},
	} {
		plain, plainErr := run(c.cmd + " " + c.redir)
		if plain == "rc=0\n" || plain == "" {
			t.Fatalf("%s %s: the plain run printed %q: the probe is wrong", c.cmd, c.redir, plain)
		}
		for _, where := range []string{"local", "server"} {
			got, gotErr := run(fmt.Sprintf("%s --where %s %s -- %s %s", offloadBin, where, c.flags, c.cmd, c.redir))
			if got != plain || !strings.HasSuffix(gotErr, plainErr) {
				t.Errorf("%s %s --where %s: stdout %q stderr %q, plain %q %q", c.cmd, c.redir, where, got, gotErr, plain, plainErr)
			}
			if where == "server" && !strings.Contains(gotErr, "closed") {
				t.Errorf("%s %s --where server: no closed-stdio refusal in %q", c.cmd, c.redir, gotErr)
			}
		}
	}
	// --stdin of a closed stdin has nothing to forward, and says so.
	for _, where := range []string{"local", "server"} {
		got, gotErr := run(offloadBin + " --where " + where + " --stdin -- cat <&-")
		if got != "rc=2\n" || !strings.Contains(gotErr, "stdin is closed") {
			t.Errorf("--stdin <&- --where %s: stdout %q stderr %q, want rc=2 naming the closed stdin", where, got, gotErr)
		}
	}
}

// The job gets neither end of the launcher's control channel, nor any other descriptor past
// stderr: one held by a job's leftover daemon would keep the control stream open.
func TestARemoteJobHoldsNoDescriptorPastStderr(t *testing.T) {
	code, out, stderr := runOffload(t, offloadBin, repo(t), nil, "--where", "server", "--", "sh", "-c", `ls /proc/$$/fd | tr '\n' ' '`)
	if code != 0 || out != "0 1 2 " {
		t.Errorf("exit %d, the job's descriptors %q (want only 0 1 2), stderr %q", code, out, stderr)
	}
}
