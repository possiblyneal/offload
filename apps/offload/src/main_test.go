package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testUnexportedMounts models an exported workspace and /tmp, with /dev/shm private.
func testUnexportedMounts() []string { return []string{"/dev/shm"} }

func useTestMounts(t *testing.T) {
	t.Helper()
	original := devMounts
	t.Cleanup(func() { devMounts = original })
	devMounts = testUnexportedMounts
}

// TestMain runs `offload` itself when OFFLOAD_TEST_RUN holds its arguments, joined by
// \x1f, so a test can watch a run that ends in exec.
func TestMain(m *testing.M) {
	if v := os.Getenv(contWatchEnv); v != "" {
		os.Exit(watchContinue(v))
	}
	if len(os.Args) > 1 && os.Args[1] == openFilesCmd { // listAsRoot runs the test binary under sudo
		os.Exit(openFilesMain(os.Args[2:], os.Stdout))
	}
	if report := os.Getenv("OFFLOAD_TEST_STDIO"); report != "" { // stdio_test.go
		plan, problem := readStdio()
		os.WriteFile(report, []byte(fmt.Sprintf("%s|%s", plan.env(), problem)), 0o600)
		os.Exit(0)
	}
	if mode := os.Getenv("OFFLOAD_TEST_QUIT"); mode != "" {
		quitQuietly()
		if mode == "sweep" { // TestASweepOutlivesTheSignalsItDrops
			lock, err := holdJobsLock(os.Getenv("OFFLOAD_TEST_LOCK"))
			if err != nil {
				os.Exit(3)
			}
			fmt.Println("ready")
			sweepSillyRenames(lock, os.Getenv("OFFLOAD_TEST_DIR"))
			os.Exit(7)
		}
		if mode == "relay" {
			c := make(chan os.Signal, 1)
			defer notifyUnignored(c, syscall.SIGQUIT)()
			fmt.Println("ready")
			<-c
			time.Sleep(300 * time.Millisecond) // time for a wrong quitQuietly to end it first
			os.Exit(9)
		}
		fmt.Println("ready")
		select {}
	}
	if path := os.Getenv("OFFLOAD_TEST_SWEEP"); path != "" { // sweep_test.go: find cannot start
		lock, err := holdJobsLock(path)
		if err != nil {
			os.Exit(3)
		}
		os.Setenv("PATH", "")
		sweepSillyRenames(lock, os.Getenv("OFFLOAD_TEST_DIR"))
		os.Exit(7)
	}
	if args := os.Getenv("OFFLOAD_TEST_RUN"); args != "" {
		configDir = cmp.Or(os.Getenv("OFFLOAD_TEST_CONFIG"), configDir)
		if os.Getenv("OFFLOAD_TEST_MOUNTS") == "1" {
			devMounts = testUnexportedMounts
		}
		os.Exit(run(strings.Split(args, "\x1f")))
	}
	os.Exit(m.Run())
}

// Inside a job, offload runs the command where it is: a nested call has no remote to reach
// and no user bus for the batch scope, so placing it would fail a job that would run.
func TestAJobsOwnOffloadCallRunsTheCommandInPlace(t *testing.T) {
	tests := []struct {
		argv   []string
		code   int
		stdout string
	}{
		{[]string{"--where", "server", "--", "sh", "-c", "echo ran; exit 7"}, 7, "ran\n"},
		{[]string{"--", "offload-no-such-command"}, 127, ""},
		{[]string{"--", "FOO=set", "sh", "-c", "echo $FOO"}, 0, "set\n"},
	}
	for _, tt := range tests {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "OFFLOAD_JOB=1", "OFFLOAD_TEST_RUN="+strings.Join(tt.argv, "\x1f"))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != tt.code || stdout.String() != tt.stdout || !strings.Contains(stderr.String(), "inside an offloaded job") {
			t.Errorf("%q: code %d, stdout %q, stderr %q; want %d, %q", tt.argv, code, stdout.String(), stderr.String(), tt.code, tt.stdout)
		}
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    options
		wantErr string // substring of the error, "" for none
	}{
		{"a command", []string{"--", "make"}, options{argv: []string{"make"}}, ""},
		{"a named remote", []string{"--where", "server", "--", "make"}, options{where: "server", argv: []string{"make"}}, ""},
		{"every flag", []string{"--where", "local", "--explain", "--stdin", "--", "a", "b"},
			options{where: local, explain: true, stdin: true, argv: []string{"a", "b"}}, ""},
		{"flags after -- are the command's", []string{"--", "tool", "--where", "x"}, options{argv: []string{"tool", "--where", "x"}}, ""},
		{"--where with nothing after it", []string{"--where"}, options{}, "--where needs a value"},
		{"--where followed by --", []string{"--where", "--", "make"}, options{}, "--where needs a value"},
		{"--where= form", []string{"--where=server", "--", "make"}, options{}, `unexpected "--where=server"`},
		{"a command without --", []string{"make"}, options{}, `unexpected "make"`},
		{"no command after --", []string{"--"}, options{}, "no command"},
		{"no arguments", nil, options{}, "no command"},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args)
		switch {
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: parseArgs(%q) error = %v, want %q", tt.name, tt.args, err, tt.wantErr)
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: parseArgs(%q) error = %v", tt.name, tt.args, err)
		case tt.wantErr == "" && !reflect.DeepEqual(got, tt.want):
			t.Errorf("%s: parseArgs(%q) = %+v, want %+v", tt.name, tt.args, got, tt.want)
		}
	}
}

func TestRunRemoteRefusesAnUnknownRemote(t *testing.T) {
	dir := t.TempDir()
	conf := "dev 10.0.0.1\nremote server 10.0.0.2\n"
	if err := os.WriteFile(filepath.Join(dir, "offload.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(d string) { configDir = d }(configDir)
	configDir = dir
	_, err := runRemote(options{where: "nosuch", argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), `unknown remote "nosuch"`) {
		t.Errorf("runRemote --where nosuch: %v", err)
	}
}

func TestRunPlaced(t *testing.T) {
	ran := outcome{code: 3}
	noSlot := outcome{fallback: true, noSlot: true}
	unreachable := outcome{fallback: true}
	tests := []struct {
		name      string
		where     string
		outcomes  map[string]outcome
		wantCalls []string
		wantWhere string
		wantRule  string
		wantErr   string // stderr substring, "" for no output at all
	}{
		{"the first remote runs it", "", map[string]outcome{"a": ran},
			[]string{"a"}, "a", "free-slot", ""},
		{"a full remote hands it to the next", "", map[string]outcome{"a": noSlot, "b": ran},
			[]string{"a", "b"}, "b", "free-slot", ""},
		{"every remote full runs it here", "", map[string]outcome{"a": noSlot, "b": noSlot},
			[]string{"a", "b"}, local, "no-free-slot", "offload: running locally: no remote has a free slot\n"},
		{"an unreachable remote runs it here without trying the next", "", map[string]outcome{"a": unreachable, "b": ran},
			[]string{"a"}, local, "remote-unreachable", "offload: running locally: a never let the job go, so nothing ran there\n"},
		{"a full named remote runs it here", "b", map[string]outcome{"b": noSlot, "a": ran},
			[]string{"b"}, local, "no-free-slot", "offload: running locally: b has no free slot\n"},
		{"a named remote runs it", "b", map[string]outcome{"b": ran},
			[]string{"b"}, "b", "where-remote", ""},
	}
	for _, tt := range tests {
		var calls []string
		defer func(f func(config, string, job, bool) (outcome, error)) { runOn = f }(runOn)
		runOn = func(_ config, where string, _ job, _ bool) (outcome, error) {
			calls = append(calls, where)
			return tt.outcomes[where], nil
		}
		req := placeRequest{Where: tt.where, Remotes: []string{"a", "b"},
			Slots: map[string]slotState{"a": {Total: 1, Free: 1}, "b": {Total: 1, Free: 1}}}
		var stderr bytes.Buffer
		d, out, err := runPlaced(&stderr, config{}, req, place(req), job{}, false)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !reflect.DeepEqual(calls, tt.wantCalls) {
			t.Errorf("%s: tried %q, want %q", tt.name, calls, tt.wantCalls)
		}
		if d.Placement != tt.wantWhere || d.Rule != tt.wantRule {
			t.Errorf("%s: placed %s by %s, want %s by %s", tt.name, d.Placement, d.Rule, tt.wantWhere, tt.wantRule)
		}
		if d.Placement != local && out != ran {
			t.Errorf("%s: outcome %+v, want the remote's %+v", tt.name, out, ran)
		}
		if stderr.String() != tt.wantErr {
			t.Errorf("%s: stderr %q, want %q", tt.name, stderr.String(), tt.wantErr)
		}
	}
}

func TestRunPlacedPassesOnAnError(t *testing.T) {
	defer func(f func(config, string, job, bool) (outcome, error)) { runOn = f }(runOn)
	runOn = func(config, string, job, bool) (outcome, error) { return outcome{}, errors.New("no pipe") }
	req := placeRequest{Remotes: []string{"a"}, Slots: map[string]slotState{"a": {Total: 1, Free: 1}}}
	if _, _, err := runPlaced(&bytes.Buffer{}, config{}, req, place(req), job{}, false); err == nil {
		t.Error("runPlaced dropped runOn's error")
	}
}

func TestNewlinePath(t *testing.T) {
	j := job{cwd: "/home/testuser/code/r", repo: "/home/testuser/code/r", writable: []string{"/home/testuser/code"}, readOnly: []string{"/home/testuser/code/r/a\nb"}}
	if got := newlinePath(j); got != "/home/testuser/code/r/a\nb" {
		t.Errorf("got %q, want the read-only entry", got)
	}
	j.readOnly = nil
	if got := newlinePath(j); got != "" {
		t.Errorf("got %q, want none", got)
	}
	j.stdio = stdioPlan{files: []outputFile{{stream: "out", fd: 1, path: "/home/testuser/code/r/o\nut"}}}
	if got := newlinePath(j); got != "/home/testuser/code/r/o\nut" {
		t.Errorf("got %q, want the output file, whose plan line would split", got)
	}
	for name, j := range map[string]job{
		"cwd":      {cwd: "/home/testuser/code/r/a\nb", repo: "/home/testuser/code/r"},
		"writable": {cwd: "/home/testuser/code/r", repo: "/home/testuser/code/r", writable: []string{"/home/testuser/a\nb"}},
	} {
		if got := newlinePath(j); !strings.Contains(got, "\n") {
			t.Errorf("%s: got %q, want the %s entry", name, got, name)
		}
	}
}

// The cwd is read-only to the job when no writable path holds it, as written or through a
// symlink, or a read-only path does.
func TestReadOnlyCwd(t *testing.T) {
	dir := t.TempDir()
	code, link := filepath.Join(dir, "code"), filepath.Join(dir, "link")
	for _, d := range []string{code + "/r/secret", dir + "/elsewhere"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("code", link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(code+"/r/x", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/elsewhere/x", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/elsewhere", code+"/out"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("r", code+"/l2"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cwd                string
		writable, readOnly []string
		want               bool
	}{
		{code + "/r", []string{code}, nil, false},
		{dir + "/elsewhere", []string{code}, nil, true},
		{code + "/r", []string{link}, nil, false},
		{link + "/r", []string{code}, nil, false},
		{code + "/r/secret", []string{code}, []string{code + "/r/secret"}, true},
		// The job's bind follows symlinks, so the real path decides, both ways.
		{code + "/out/x", []string{code}, nil, true},
		{code + "/l2/x", []string{code}, []string{code + "/r"}, true},
		{code + "/r/secret", []string{code}, []string{link + "/r/secret"}, true},
	} {
		got := readOnlyCwd(c.cwd, c.writable, c.readOnly) != ""
		if got != c.want {
			t.Errorf("cwd %s, writable %v, read-only %v: got %v, want %v", c.cwd, c.writable, c.readOnly, got, c.want)
		}
	}
}

// A path's trailing whitespace is part of it.
// A linked worktree commits into its main checkout's git dir, so that dir is the job's to
// write, whatever the Edit rules say; in a plain checkout it is inside the toplevel.
func TestALinkedWorktreesGitDirIsWritable(t *testing.T) {
	dir := outsideTmp(t) // /tmp is writable to every job
	main, wt := dir+"/main", dir+"/wt"
	for _, args := range [][]string{{"init", "-q", main}, {"-C", main, "commit", "-q", "--allow-empty", "-m", "x"}, {"-C", main, "worktree", "add", "-q", wt}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	paths, err := resolvePaths([]byte("{}"), dir+"/home", jobRepos(wt), os.DirFS("/"))
	if err != nil {
		t.Fatal(err)
	}
	if !under(main+"/.git/worktrees/wt/index", paths.writable) || !under(main+"/.git/objects", paths.writable) || under(main+"/README", paths.writable) {
		t.Errorf("writable %v, want the worktree and %s/.git but not the main checkout", paths.writable, main)
	}
	if got := gitCommonDir(main); got != main+"/.git" {
		t.Errorf("gitCommonDir(main) = %q, want %q", got, main+"/.git")
	}
}

func TestGitToplevelKeepsTrailingSpace(t *testing.T) {
	repo := t.TempDir() + "/repo "
	if err := os.MkdirAll(repo+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if got := gitToplevel(repo + "/sub"); got != repo {
		t.Errorf("gitToplevel = %q, want %q", got, repo)
	}
}

// A SIGQUIT before the wrapper relays signals (placement, before supervise) ends it by
// SIGQUIT with nothing on stderr, as a plain command dies, not by the Go runtime's handler,
// which dumps every goroutine and exits 2. While a run relays them it is the run's to pass on.
func TestSIGQUITEndsTheWrapperQuietlyUnlessARunRelaysIt(t *testing.T) {
	for _, mode := range []string{"bare", "relay"} {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "OFFLOAD_TEST_QUIT="+mode)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		line := make([]byte, 6)
		if _, err := io.ReadFull(out, line); err != nil {
			t.Fatal(mode, err)
		}
		cmd.Process.Signal(syscall.SIGQUIT)
		err = cmd.Wait()
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		switch {
		case mode == "bare" && !(ws.Signaled() && ws.Signal() == syscall.SIGQUIT && !ws.CoreDump()):
			t.Errorf("bare: %v, want death by SIGQUIT", err)
		case mode == "relay" && ws.ExitStatus() != 9:
			t.Errorf("relay: %v, want exit 9 (relayed)", err)
		}
		if stderr.Len() != 0 {
			t.Errorf("%s: stderr %q, want none", mode, stderr.String())
		}
	}
}

// Each refusal fact read from the job reaches place: a job that trips each one is refused
// for each, and a plain one for none.
func TestWithJobFactsReadsEveryJobFact(t *testing.T) {
	dir := t.TempDir()
	plain := job{uid: 1000, gid: 1000, groups: []int{1000}, cwd: dir, repo: dir, argv: []string{"true"}, writable: []string{dir}}
	if got := withJobFacts(refusalFacts{}, plain); got != (refusalFacts{}) {
		t.Errorf("plain job: got %+v, want no facts", got)
	}
	many := make([]int, maxNFSGroups+1)
	for i := range many {
		many[i] = 2000 + i
	}
	tripped := job{uid: 0, gid: 0, groups: many, cwd: dir, repo: "/x\ny", argv: []string{"echo", strings.Repeat("a", maxCommandLine)}}
	got := withJobFacts(refusalFacts{}, tripped)
	if got.TooLong == 0 || !got.Root || got.Newline != "/x\ny" || got.Groups != maxNFSGroups+1 || got.ReadOnlyCwd != dir {
		t.Errorf("tripped job: got %+v, want TooLong, Root, Newline, Groups and ReadOnlyCwd", got)
	}
	denied := plain
	denied.readOnly = []string{dir}
	if got := withJobFacts(refusalFacts{}, denied); got.ReadOnlyCwd != dir {
		t.Errorf("cwd under a read-only path: got %+v, want ReadOnlyCwd %s", got, dir)
	}
}

// root_squash maps gid 0 too, as the primary group or a supplementary one.
func TestRootSquashedCoversGroupZero(t *testing.T) {
	for _, c := range []struct {
		name     string
		uid, gid int
		groups   []int
		want     bool
	}{
		{"an ordinary user", 1000, 1000, []int{1000, 27}, false},
		{"root", 0, 0, []int{0}, true},
		{"primary group 0", 1000, 0, []int{1000}, true},
		{"supplementary group 0", 1000, 1000, []int{1000, 0}, true},
	} {
		if got := rootSquashed(c.uid, c.gid, c.groups); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A message written to a reader of stderr that has gone (`running locally`, the closing
// one) leaves the wrapper to run the job and end as it did, not by SIGPIPE.
func TestTheWrapperOutlivesItsStderrReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	config := t.TempDir()
	for name, contents := range map[string]string{
		"offload.conf": "dev 192.0.2.50\nremote server 192.0.2.10\n",
		"local-only":   "xvfb-run\n",
	} {
		if err := os.WriteFile(filepath.Join(config, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Named for an entry of the fixture's local-only list, so placement keeps it here and says so.
	dir := t.TempDir()
	ran, job := filepath.Join(dir, "ran"), filepath.Join(dir, "xvfb-run")
	if err := os.WriteFile(job, []byte("#!/bin/sh\ntouch \"$1\"; exit 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_CONFIG="+config, "OFFLOAD_TEST_RUN="+strings.Join([]string{"--", job, ran}, "\x1f"))
	cmd.Stderr = w
	err = cmd.Run()
	w.Close()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 5 {
		t.Errorf("want the job's exit 5, got %v", err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Errorf("the job never ran: %v", err)
	}
}

// An output file outside the job's writable paths goes through their checks too: one a dev
// process holds a flock on keeps the job here, where a lock the job takes on it conflicts
// as it would locally.
func TestAnOutputFileAnotherProcessLocksKeepsTheJobHere(t *testing.T) {
	// The run must see every holder on the disk, other accounts' too, or it refuses for that.
	if exec.Command("sudo", "-n", "true").Run() != nil {
		t.Skip("needs passwordless sudo to read other accounts' open files")
	}
	out, err := os.CreateTemp(t.TempDir(), "offload-test-out-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(out.Name()) })
	holder := exec.Command("flock", out.Name(), "sleep", "60")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // sleep holds the lock too
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-holder.Process.Pid, syscall.SIGKILL); holder.Wait() })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if locks, _ := os.ReadFile("/proc/locks"); bytes.Contains(locks, []byte(fmt.Sprintf(" %d ", holder.Process.Pid))) {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the holder never took its flock")
		}
	}
	// An unreachable remote: a job wrongly sent there falls back, for another reason.
	config := t.TempDir()
	for _, name := range []string{"local-only", "remote-local-paths", "remote-store-commands"} {
		b, err := os.ReadFile(filepath.Join("../config", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(config, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	conf := "dev 192.0.2.50\nremote server 192.0.2.1\nslots server 1\n"
	if err := os.WriteFile(filepath.Join(config, "offload.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	// The run opens the file itself: one this test shares would be relayed, not the job's.
	out.Close()
	var stderr bytes.Buffer
	cmd := exec.Command("sh", "-c", `exec "$0" >"$1"`, os.Args[0], out.Name())
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "OFFLOAD_TEST_CONFIG=" + config,
		"OFFLOAD_TEST_RUN=--\x1ftrue"}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if want := "running locally: the job's output file " + out.Name(); !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}
