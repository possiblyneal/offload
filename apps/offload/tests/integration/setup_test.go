package integration

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sh runs a shell script on dev, or on the server when remote is set, and returns
// its combined output.
func sh(t *testing.T, remote bool, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	if remote {
		cmd = exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr, script)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustSh(t *testing.T, remote bool, script string) string {
	t.Helper()
	out, err := sh(t, remote, script)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
}

// devMounts lists the processes in any namespace of the host that hold a mount of dev.
func devMounts() string {
	return `sudo sh -c 'grep -l "nfs4 ` + devAddr + `:/" /proc/[0-9]*/mountinfo 2>/dev/null' || true`
}

func TestNothingOfDevIsMountedOnTheServerHostOrItsContainers(t *testing.T) {
	cmd := exec.Command(offloadBin, "--where", "server", "--", "sleep", "8")
	cmd.Dir = repo(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGKILL); cmd.Wait() })
	time.Sleep(3 * time.Second)

	if got := strings.TrimSpace(mustSh(t, true, devMounts())); got == "" {
		t.Fatal("while a job runs, no process on the server holds a mount of dev: the probe sees nothing")
	}
	host := mustSh(t, true, "findmnt -rn -t nfs,nfs4 -o SOURCE,TARGET || true")
	if strings.Contains(host, devAddr) {
		t.Errorf("host namespace shows a mount of dev:\n%s", host)
	}
	inContainer := mustSh(t, true,
		`sudo nsenter -t "$(sudo podman inspect -f '{{.State.Pid}}' node-exporter)" -m findmnt -rn -t nfs,nfs4 -o SOURCE,TARGET || true; echo probe-ran`)
	if !strings.Contains(inContainer, "probe-ran") || strings.Contains(inContainer, devAddr) {
		t.Errorf("node-exporter shows a mount of dev (or the probe failed):\n%s", inContainer)
	}

	cmd.Wait()
	time.Sleep(time.Second)
	if got := strings.TrimSpace(mustSh(t, true, devMounts())); got != "" {
		t.Errorf("a mount of dev outlived its job: %s", got)
	}
}

func TestExportAdmitsOnlyTheListedRemotes(t *testing.T) {
	exports := mustSh(t, false, "sudo exportfs -s")
	if !strings.Contains(exports, serverAddr+"(") {
		t.Errorf("server is not exported to:\n%s", exports)
	}
	for _, line := range strings.Split(strings.TrimSpace(exports), "\n") {
		for _, field := range strings.Fields(line)[1:] {
			if !strings.HasPrefix(field, serverAddr+"(") {
				t.Errorf("export admits %q, want only %s", field, serverAddr)
			}
		}
	}

	// Dev itself is not listed, so its own mount attempt is refused. The attempt
	// runs in a private mount namespace, so a success would still leave nothing.
	out, err := sh(t, false, `sudo unshare --mount --propagation private bash -c '
		d=$(mktemp -d /run/offload-it.XXXXXX); trap "rmdir $d" EXIT
		mount -t nfs4 -o vers=4.2 `+devAddr+`:/ $d'`)
	if err == nil {
		t.Errorf("an address outside the list mounted dev's filesystem")
	} else if !strings.Contains(out, "denied") {
		t.Errorf("mount failed, but not as a denial: %s", out)
	}
}

// activeJobs lists the job scopes running on the server.
func activeJobs(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(mustSh(t, true, `systemctl list-units --state=active 'offload-job-*.scope' --no-legend --plain`))
}

func TestUninstallRefusesWhileAJobRunsAndChangesNothing(t *testing.T) {
	if jobs := activeJobs(t); jobs != "" {
		t.Fatalf("another session's job is running on the server; not uninstalling under it:\n%s", jobs)
	}
	t.Cleanup(func() {
		// The killed wrapper's job winds down on the server within its grace; install
		// refuses until it has.
		waitOnServer(t, "/usr/local/libexec/[o]ffload-launch", false, 30*time.Second)
		mustSh(t, false, "../../install.sh")
	})
	mustSh(t, false, "../../install.sh")
	startRunning(t, 60)

	out, err := sh(t, false, "../../uninstall.sh")
	if err == nil {
		t.Fatalf("uninstall succeeded under a running job:\n%s", out)
	}
	if !strings.Contains(out, serverAddr) {
		t.Errorf("refusal does not name the remote:\n%s", out)
	}
	if exports := mustSh(t, false, "sudo exportfs -s"); !strings.Contains(exports, serverAddr+"(") {
		t.Errorf("export removed despite the refusal:\n%s", exports)
	}
	if got := mustSh(t, false, "ls ~/.local/bin/offload"); !strings.Contains(got, "offload") {
		t.Errorf("command removed despite the refusal")
	}
	if got := mustSh(t, true, "ls /usr/local/libexec"); !strings.Contains(got, "offload-launch") {
		t.Errorf("launcher removed despite the refusal:\n%s", got)
	}
}

func TestUninstallLeavesNfsServerEnabledWhenAnotherExportRemains(t *testing.T) {
	if jobs := activeJobs(t); jobs != "" {
		t.Fatalf("another session's job is running on the server; not uninstalling under it:\n%s", jobs)
	}
	const other = "/etc/exports.d/offload-it-other.exports"
	t.Cleanup(func() {
		mustSh(t, false, "sudo rm -f "+other+"; sudo exportfs -ra; ../../install.sh")
	})
	mustSh(t, false, "../../install.sh")
	mustSh(t, false, "echo '/run/offload-it-other 127.0.0.1(ro)' | sudo tee "+other+" >/dev/null; sudo mkdir -p /run/offload-it-other")

	mustSh(t, false, "../../uninstall.sh")
	if got := strings.TrimSpace(mustSh(t, false, "systemctl is-enabled nfs-server")); got != "enabled" {
		t.Errorf("nfs-server is %q after uninstall, want enabled: another export remains", got)
	}
}

func TestInstallAndUninstallAreIdempotentAndLeaveNothing(t *testing.T) {
	if jobs := activeJobs(t); jobs != "" {
		t.Fatalf("another session's job is running on the server; not uninstalling under it:\n%s", jobs)
	}
	install := func() { mustSh(t, false, "../../install.sh") }
	uninstall := func() { mustSh(t, false, "../../uninstall.sh") }
	t.Cleanup(install) // leave the machine set up, whatever happened

	install()
	install()

	uninstall()
	uninstall()
	if out := mustSh(t, false, "sudo exportfs -s; ls /etc/exports.d"); strings.Contains(out, "offload") || strings.Contains(out, devAddr) || strings.Contains(out, serverAddr) {
		t.Errorf("export left behind:\n%s", out)
	}
	if out := mustSh(t, false, "ls ~/.local/bin/offload 2>&1 || true"); !strings.Contains(out, "No such file") {
		t.Errorf("command left behind: %s", out)
	}
	if out := mustSh(t, true, "ls /usr/local/libexec /etc/offload /var/lib/offload 2>&1 || true"); strings.Contains(out, "offload-") || !strings.Contains(out, "No such file") {
		t.Errorf("launcher left behind on the server:\n%s", out)
	}
	for _, remote := range []bool{false, true} {
		if got := strings.TrimSpace(mustSh(t, remote, devMounts())); got != "" {
			t.Errorf("mount of dev left behind (remote=%v): %s", remote, got)
		}
	}
}
