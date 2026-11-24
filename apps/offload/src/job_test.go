package main

import (
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestParseConfigReadsDevAndRemotes(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
# the address remotes mount
dev 192.0.2.50

remote server 192.0.2.10
remote ai 192.0.2.20
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dev != "192.0.2.50" {
		t.Errorf("dev = %q", cfg.dev)
	}
	if cfg.remotes["server"] != "192.0.2.10" || cfg.remotes["ai"] != "192.0.2.20" || len(cfg.remotes) != 2 {
		t.Errorf("remotes = %v", cfg.remotes)
	}
}

func TestParseConfigRejectsWhatItDoesNotKnow(t *testing.T) {
	for _, bad := range []string{"bogus x", "remote server", "dev"} {
		if _, err := parseConfig(strings.NewReader(bad)); err == nil {
			t.Errorf("parseConfig(%q) succeeded", bad)
		}
	}
}

func TestShellQuoteSurvivesTheRemoteShell(t *testing.T) {
	cases := map[string]string{
		"plain":       `'plain'`,
		"with space":  `'with space'`,
		"it's":        `'it'\''s'`,
		"$HOME `x` ;": `'$HOME ` + "`x`" + ` ;'`,
		"":            `''`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestLauncherCommandNamesTheJob(t *testing.T) {
	got := launcherCommand(job{
		devAddr:  "192.0.2.50",
		uid:      1000,
		gid:      1000,
		groups:   []int{24, 25, 100},
		repo:     "/home/testuser/my repo",
		cwd:      "/home/testuser/my repo/sub",
		writable: []string{"/home/testuser/code", "/home/testuser/my repo"},
		readOnly: []string{"/home/testuser/code/a/TASKS.md"},
		forwards: []forward{{port: 8765, dest: "127.0.0.1"}, {port: 5000, dest: "::1"}},
		id:       "ab12",
		ctlID:    "c1",
		argv:     []string{"go", "test", "./..."},
	})
	want := `sudo -n --preserve-env=SSH_AUTH_SOCK env 'OFFLOAD_CTL=c1' 'OFFLOAD_GRACE=0' /usr/local/libexec/offload-launch '192.0.2.50' 1000 1000 '24,25,100' '/home/testuser/my repo/sub' '/home/testuser/code
/home/testuser/my repo' '/home/testuser/code/a/TASKS.md' '/tmp/offload-ab12-8765.sock /tmp/offload-ab12-5000.sock' -- 'go' 'test' './...'`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A caller with no supplementary groups still passes the groups argument, empty: an
// unquoted empty word would vanish and shift every later argument.
func TestLauncherCommandKeepsAnEmptyGroupListAsAnArgument(t *testing.T) {
	got := launcherCommand(job{devAddr: "10.0.0.1", uid: 1000, gid: 1000, repo: "/r", cwd: "/r", argv: []string{"true"}, ctlID: "c1"})
	if !strings.Contains(got, " 1000 1000 '' ") {
		t.Errorf("groups not kept as an empty argument: %s", got)
	}
}

func TestLauncherCommandCarriesTheCallersUmaskAndLimits(t *testing.T) {
	got := launcherCommand(job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "c1", umask: "0077", nofile: "1024", rlimits: "fsize=4096 core=unlimited"})
	for _, want := range []string{"'OFFLOAD_UMASK=0077'", "'OFFLOAD_NOFILE=1024'", "'OFFLOAD_RLIMITS=fsize=4096 core=unlimited'"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if got := launcherCommand(job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "c1"}); strings.Contains(got, "OFFLOAD_RLIMITS") {
		t.Errorf("no limits still sent some: %q", got)
	}
}

// Each soft limit goes as prlimit's name for it, in its own units, and none as "unlimited".
func TestCallerRlimits(t *testing.T) {
	if testWithFileSizeLimitInChild(t) {
		return
	}
	var fsize, core syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &fsize); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &core); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &fsize); syscall.Setrlimit(syscall.RLIMIT_CORE, &core) })
	low := fsize
	low.Cur = 1536
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &low); err != nil {
		t.Fatal(err)
	}
	none := core
	none.Cur = core.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &none); err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(callerRlimits())
	if len(words) != len(rlimits) || !slices.Contains(words, "fsize=1536") {
		t.Errorf("callerRlimits() = %q, want every limit and fsize=1536", words)
	}
	if core.Max == math.MaxUint64 && !slices.Contains(words, "core=unlimited") {
		t.Errorf("callerRlimits() = %q, want core=unlimited", words)
	}
}

// Go raises its own soft RLIMIT_NOFILE to the hard limit at startup, so the caller's
// limit is the one a child sees, not the one this process reports.
func TestCallerLimitsAreTheUmaskAndTheSoftLimitTheCallerGaveThisProcess(t *testing.T) {
	old := syscall.Umask(0o027)
	t.Cleanup(func() { syscall.Umask(old) })
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) })
	low := lim
	low.Cur = 1000
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	umask, nofile, err := callerLimits()
	if err != nil {
		t.Fatal(err)
	}
	if umask != "0027" || nofile != "1000" {
		t.Errorf("callerLimits() = %q %q, want 0027 1000", umask, nofile)
	}
}

func TestEnvBlockIsNulTerminatedEntriesEndedByAnEmptyOne(t *testing.T) {
	got := string(envBlock([]string{"A=1", "B=two words"}))
	if got != "A=1\x00B=two words\x00\x00" {
		t.Errorf("got %q", got)
	}
}

func TestEnvBlockDropsTheDisplayVariablesAndBareWords(t *testing.T) {
	got := string(envBlock([]string{"A=1", "rm", "DISPLAY=:99", "WAYLAND_DISPLAY=wayland-1", "DISPLAY_NAME=keep", "B=2"}))
	if got != "A=1\x00DISPLAY_NAME=keep\x00B=2\x00\x00" {
		t.Errorf("got %q", got)
	}
}

// A command line ssh cannot pass to the remote's shell is a refusal, not an exec failure.
func TestCommandTooLongPassesOnlyWhatSSHCarries(t *testing.T) {
	small := job{argv: []string{"sh", "-c", "true"}}
	if n := commandTooLong(small); n != 0 {
		t.Errorf("a short command: %d, want 0", n)
	}
	big := job{argv: []string{"echo", strings.Repeat("x", maxCommandLine)}}
	if n := commandTooLong(big); n <= maxCommandLine || n != len(launcherCommand(big)) {
		t.Errorf("a long command: %d, want its length %d", n, len(launcherCommand(big)))
	}
}

// The signals the caller ignored reach the launcher, which ignores them for the job as a
// local run does through viaEnv.
func TestLauncherCommandCarriesTheCallersIgnoredSignals(t *testing.T) {
	got := launcherCommand(job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "c1", ignore: "10,15", block: "2,12"})
	if !strings.Contains(got, "'OFFLOAD_IGNORE=10,15'") || !strings.Contains(got, "'OFFLOAD_BLOCK=2,12'") {
		t.Errorf("ignore or block list missing from %q", got)
	}
	got = launcherCommand(job{devAddr: "10.0.0.1", argv: []string{"true"}, ctlID: "c1"})
	if strings.Contains(got, "OFFLOAD_IGNORE") || strings.Contains(got, "OFFLOAD_BLOCK") {
		t.Errorf("an empty signal list was passed: %q", got)
	}
}

// Go's test-cache logger writes from the test process. Lower the process-wide
// file size limit only in a child that has no cache log to corrupt.
func testWithFileSizeLimitInChild(t *testing.T) bool {
	t.Helper()
	const child = "OFFLOAD_TEST_FILE_LIMIT_CHILD"
	if os.Getenv(child) == "1" {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), child+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("file size limit child: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
	return true
}
