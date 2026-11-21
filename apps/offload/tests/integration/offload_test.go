package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExitCodesMatchLocal(t *testing.T) {
	for _, code := range []string{"0", "1", "3"} {
		t.Run("exit "+code, func(t *testing.T) {
			local, remote := both(t, "sh", "-c", "exit "+code)
			same(t, local, remote)
		})
	}
}

// A job killed by a signal kills the wrapper by it, as a plain run's shell sees; a job
// that exits 130 is not mistaken for one killed by SIGINT.
func TestASignalDeathMatchesLocal(t *testing.T) {
	for _, script := range []string{"kill -TERM $$", "kill -INT $$", "exit 130"} {
		t.Run(script, func(t *testing.T) {
			local, remote := both(t, "sh", "-c", script)
			same(t, local, remote)
			if (remote.signal != 0) == (script == "exit 130") {
				t.Errorf("offloaded: signal %d, code %d", remote.signal, remote.code)
			}
		})
	}
}

// A command offload cannot start, or one led by a NAME=value word, reads the same wherever
// the job lands: every path starts the command through env.
func TestCommandStartMatchesBetweenLocalAndOffloadedRuns(t *testing.T) {
	for name, argv := range map[string][]string{
		"missing":          {"offload-it-no-such-command"},
		"not executable":   {"/etc/hostname"},
		"leading NAME=val": {"FOO=set", "sh", "-c", `echo "[$FOO]"`},
	} {
		t.Run(name, func(t *testing.T) {
			var codes [2]int
			var outs, errs [2]string
			for i, where := range []string{"local", "server"} {
				codes[i], outs[i], errs[i] = runOffload(t, offloadBin, repo(t), nil, append([]string{"--where", where, "--"}, argv...)...)
				errs[i] = strings.ReplaceAll(errs[i], "offload: running locally: --where local\n", "")
			}
			if codes[0] != codes[1] || outs[0] != outs[1] || errs[0] != errs[1] {
				t.Errorf("local: %d %q %q; offloaded: %d %q %q", codes[0], outs[0], errs[0], codes[1], outs[1], errs[1])
			}
		})
	}
}

func TestRunsOnTheRemote(t *testing.T) {
	r := execIn(t, repo(t), true, "hostname")
	if r.code != 0 || r.stdout != "server\n" {
		t.Errorf("hostname offloaded: code %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestToolchainOutputMatchesLocal(t *testing.T) {
	local, remote := both(t, "go", "version")
	if local.code != 0 {
		t.Fatalf("local go version failed: %v", local)
	}
	same(t, local, remote)
}

func TestFileWrittenInRepoLandsOnDevOwnedByTheCaller(t *testing.T) {
	script := `echo hello > out.txt && mkdir sub && echo nested > sub/in.txt && git status --porcelain`
	local, remote := both(t, "sh", "-c", script)
	same(t, local, remote)

	f, ok := remote.files["out.txt"]
	if !ok || f.content != "hello\n" {
		t.Fatalf("out.txt not written on dev: %v", remote.files)
	}
	if int(f.uid) != os.Getuid() || int(f.gid) != os.Getgid() {
		t.Errorf("out.txt owned by %d:%d, want %d:%d", f.uid, f.gid, os.Getuid(), os.Getgid())
	}
	if !strings.Contains(remote.stdout, "?? out.txt") {
		t.Errorf("git status did not work on the written file: %q", remote.stdout)
	}
}

func TestReadOutsideTheRepoReturnsTheSameBytes(t *testing.T) {
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(home, ".claude", "settings.json")}
	if projects, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl")); len(projects) > 0 {
		paths = append(paths, projects[0])
	}
	for _, p := range paths {
		local, remote := both(t, "sha256sum", p)
		if local.code != 0 {
			t.Fatalf("local read of %s failed: %v", p, local)
		}
		same(t, local, remote)
	}
}

func TestWriteOutsideTheRepoFailsReadOnly(t *testing.T) {
	home, _ := os.UserHomeDir()
	target := filepath.Join(home, "offload-it-must-not-exist")
	t.Cleanup(func() { os.Remove(target) })

	r := execIn(t, repo(t), true, "sh", "-c", "echo x > "+target)
	if r.code == 0 {
		t.Errorf("write outside the repository succeeded")
	}
	if !strings.Contains(r.stderr, "Read-only file system") || !strings.Contains(r.stderr, target) {
		t.Errorf("stderr should name the path and the read-only error: %q", r.stderr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("%s exists on dev", target)
	}
}

func TestUserNamespacesWork(t *testing.T) {
	if r := execIn(t, repo(t), true, "unshare", "-r", "true"); r.code != 0 {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// A container tool named outright stays on dev even under --where server.
func TestAContainerToolForcedOntoTheRemoteRunsHere(t *testing.T) {
	_, _, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--", "podman", "--version")
	if !strings.Contains(stderr, "offload: running locally: ") || !strings.Contains(stderr, "on the remote's disk") {
		t.Errorf("stderr %q: want a local run that names the remote's store", stderr)
	}
}

// One a script hides from placement meets the remote's container store bound read-only,
// so the run fails loudly instead of changing a store dev never sees.
func TestAHiddenContainerRunOnTheRemoteFailsLoudly(t *testing.T) {
	r := execIn(t, repo(t), true, "sh", "-c", `p=pod; "${p}man" run --rm alpine true`)
	if r.code == 0 || !strings.Contains(r.stderr, "read-only file system") {
		t.Errorf("exit %d, stderr %q: want a failure naming the read-only store", r.code, r.stderr)
	}
}
