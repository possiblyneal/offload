// Package integration runs one command locally and through `offload --where server`
// and compares what each did: exit code, stdout, stderr, and the files written with
// their contents and owners. It needs the server and the export installed by
// apps/offload/install.sh, and is run by scripts/integration.
package integration

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

var (
	offloadBin  string
	scratchRoot string
	devAddr     string
	serverAddr  string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	devAddr = os.Getenv("OFFLOAD_TEST_DEV_ADDR")
	serverAddr = os.Getenv("OFFLOAD_TEST_SERVER_ADDR")
	dev, devErr := netip.ParseAddr(devAddr)
	server, serverErr := netip.ParseAddr(serverAddr)
	if devErr != nil || serverErr != nil || !dev.Is4() || !server.Is4() || dev == server {
		fmt.Fprintln(os.Stderr, "set OFFLOAD_TEST_DEV_ADDR and OFFLOAD_TEST_SERVER_ADDR to distinct IPv4 addresses for the test deployment")
		return 1
	}
	top, err := gitTop(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "not in a git checkout:", err)
		return 1
	}
	scratchRoot = filepath.Join(top, "tmp")
	if err := os.MkdirAll(scratchRoot, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	bin, err := os.MkdirTemp(scratchRoot, "offload-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(bin)
	// Every run logs to a decisions log of its own (on dev's disk: an XDG_STATE_HOME on
	// a tmpfs keeps every job local), so the faults the tests cause never reach the
	// real one, which `offload health` reports to the homelab panel.
	state := filepath.Join(bin, "state")
	if err := os.Setenv("XDG_STATE_HOME", state); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	offloadBin = filepath.Join(bin, "offload")
	config, err := filepath.Abs("../../config")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	build := exec.Command("go", "build", "-ldflags", ldflags(config), "-o", offloadBin, "devex/offload")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build offload: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// ldflags bakes the config folder into a test build of offload, and the launcher named by
// OFFLOAD_TEST_LAUNCHER when set, so a branch can be tested without replacing the
// launcher other branches use on the server.
func ldflags(configDir string) string {
	// No process has a WAL file open as far as the build can tell, so what the tests
	// check does not depend on what else runs on dev.
	flags := "-X main.configDir=" + configDir + " -X main.fdGlob=/nonexistent/*"
	if l := os.Getenv("OFFLOAD_TEST_LAUNCHER"); l != "" {
		flags += " -X main.launcherPath=" + l
	}
	return flags
}

func gitTop(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// repo makes a scratch git repository on dev's own disk (not /tmp, which is a
// tmpfs and is not exported) and returns its path.
func repo(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(scratchRoot, "offload-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

type fileState struct {
	content string
	uid     uint32
	gid     uint32
	mode    os.FileMode
}

type outcome struct {
	code   int
	signal int // the signal the process died by, or 0; code is then 128 plus it
	stdout string
	stderr string
	files  map[string]fileState
}

// execIn runs argv in dir, directly or through offload, and reports what happened.
// The directory's own path is replaced by <dir> in the output so two scratch
// repositories compare equal.
func execIn(t *testing.T, dir string, offloaded bool, argv ...string) outcome {
	t.Helper()
	if offloaded {
		argv = append([]string{offloadBin, "--where", "server", "--"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil && cmd.ProcessState == nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return outcome{
		code:   exitOf(err),
		signal: signalOf(cmd.ProcessState),
		stdout: strings.ReplaceAll(stdout.String(), dir, "<dir>"),
		stderr: strings.ReplaceAll(stderr.String(), dir, "<dir>"),
		files:  snapshot(t, dir),
	}
}

// snapshot records every file under dir except .git, with content, owner and mode.
func snapshot(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	files := map[string]fileState{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		files[rel] = fileState{string(content), st.Uid, st.Gid, info.Mode()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// both runs argv in two identical scratch repositories, once locally and once
// offloaded, and returns the two outcomes.
func both(t *testing.T, argv ...string) (local, remote outcome) {
	t.Helper()
	return execIn(t, repo(t), false, argv...), execIn(t, repo(t), true, argv...)
}

// same fails unless the offloaded run matches the local one on every observable.
func same(t *testing.T, local, remote outcome) {
	t.Helper()
	if local.code != remote.code || local.signal != remote.signal {
		t.Errorf("exit code: local %d (signal %d), offloaded %d (signal %d)", local.code, local.signal, remote.code, remote.signal)
	}
	if local.stdout != remote.stdout {
		t.Errorf("stdout: local %q, offloaded %q", local.stdout, remote.stdout)
	}
	if local.stderr != remote.stderr {
		t.Errorf("stderr: local %q, offloaded %q", local.stderr, remote.stderr)
	}
	if fmt.Sprint(local.files) != fmt.Sprint(remote.files) {
		t.Errorf("files written: local %v, offloaded %v", local.files, remote.files)
	}
}
