package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A link in the cwd into an unexported mount keeps the job here though no argument names
// it: a build or a script follows it, and the remote would read what the mount hides.
func TestTmpRefusalKeepsAJobWhoseCwdLinksIntoAnUnexportedMount(t *testing.T) {
	cwd, mnt := outsideTmp(t), outsideTmp(t)
	if err := os.Symlink(mnt, filepath.Join(cwd, "piconf")); err != nil {
		t.Fatal(err)
	}
	if got := cwdLinkRefusal([]string{mnt}, cwdLinks(t, cwd, "")); !strings.Contains(got, "piconf") {
		t.Errorf("got %q, want the link refused", got)
	}
	if got := cwdLinkRefusal([]string{filepath.Join(mnt, "sub")}, cwdLinks(t, cwd, "")); !strings.Contains(got, "holds") {
		t.Errorf("a link holding a mount: %q", got)
	}
	if got := cwdLinkRefusal([]string{"/elsewhere"}, cwdLinks(t, cwd, "")); got != "" {
		t.Errorf("a link to an exported directory: %q", got)
	}
}

// jobLinks sees a dot-named link in the cwd and a link the repository tracks at any depth.
func TestJobLinksFindsDotLinksAndTrackedDeeperLinks(t *testing.T) {
	repo, mnt := outsideTmp(t), outsideTmp(t)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	deep := filepath.Join(repo, "conf/deep/live")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{filepath.Join(repo, ".venv"), deep} {
		if err := os.Symlink(mnt, l); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", repo, "add", "conf").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	links := cwdLinks(t, repo, repo)
	for _, want := range []string{filepath.Join(repo, ".venv"), deep} {
		if !slices.Contains(links, want) {
			t.Errorf("jobLinks %q lacks %s", links, want)
		}
	}
	if got := cwdLinkRefusal([]string{mnt}, links); got == "" {
		t.Error("a link onto an unexported mount was not refused")
	}
}

// A link onto a remote-local path keeps the job here though no argument names it: `make`
// would follow it to the remote's own copy.
func TestRemoteLocalRefusalKeepsAJobWhoseLinkReachesARemoteLocalPath(t *testing.T) {
	home, cwd := outsideTmp(t), outsideTmp(t)
	target := filepath.Join(home, ".cache/go-build")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cwd, "out")); err != nil {
		t.Fatal(err)
	}
	rl := []remoteLocalPath{{path: target, rel: ".cache/go-build"}}
	for _, argv := range [][]string{{"make"}, {"sh", "-c", "echo x > out/result"}} {
		if got := remoteLocalRefusal(rl, home, cwd, argv, cwdLinks(t, cwd, "")); !strings.Contains(got, "link") {
			t.Errorf("%q: got %q, want the link refused", argv, got)
		}
	}
	if got := remoteLocalRefusal(rl, home, cwd, []string{"make"}, nil); got != "" {
		t.Errorf("with no links: %q", got)
	}
}

// An untracked link at any depth under the cwd is followed unnamed as one directly in it
// is (`make` reaching build/rootfs), so jobLinks walks the cwd for them.
func TestJobLinksFindsUntrackedLinksDeeperThanTheCwd(t *testing.T) {
	home, cwd, mnt := outsideTmp(t), outsideTmp(t), outsideTmp(t)
	cache := filepath.Join(home, ".cache/go-build")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "build/out/deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs, gocache := filepath.Join(cwd, "build/out/deep/rootfs"), filepath.Join(cwd, "build/gocache")
	for l, target := range map[string]string{rootfs: mnt, gocache: cache} {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	links, problem := jobLinks(cwd, "")
	if problem != "" || !slices.Contains(links, rootfs) || !slices.Contains(links, gocache) {
		t.Fatalf("jobLinks %q, problem %q; want %s and %s", links, problem, rootfs, gocache)
	}
	if got := cwdLinkRefusal([]string{mnt}, links); !strings.Contains(got, rootfs) {
		t.Errorf("a deep link onto an unexported mount: %q", got)
	}
	rl := []remoteLocalPath{{path: cache, rel: ".cache/go-build"}}
	if got := remoteLocalRefusal(rl, home, cwd, []string{"make"}, links); !strings.Contains(got, gocache) {
		t.Errorf("a deep link onto a remote-local path: %q", got)
	}
}

// The walk is bounded and loud: past linkWalkLimit entries, or at a directory the job can
// enter but offload cannot list, it says so rather than pass over links it never saw. A
// directory the job cannot enter holds no link it could follow, so it is skipped.
func TestJobLinksSaysWhenItCannotSeeEveryLink(t *testing.T) {
	cwd := outsideTmp(t)
	for _, d := range []string{"a/b", "sealed/x", "blind/y"} {
		if err := os.MkdirAll(filepath.Join(cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if links, problem := jobLinks(cwd, ""); problem != "" || links != nil {
		t.Fatalf("links %q, problem %q; want neither", links, problem)
	}
	defer func(n int) { linkWalkLimit = n }(linkWalkLimit)
	linkWalkLimit = 3
	if _, problem := jobLinks(cwd, ""); !strings.Contains(problem, "more than 3") {
		t.Errorf("past the limit: problem %q", problem)
	}
	linkWalkLimit = 1000
	if os.Geteuid() == 0 {
		t.Skip("root lists every directory")
	}
	sealed, blind := filepath.Join(cwd, "sealed"), filepath.Join(cwd, "blind")
	chmod := func(d string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(d, 0o755) })
	}
	chmod(sealed, 0)
	if _, problem := jobLinks(cwd, ""); problem != "" {
		t.Errorf("a directory the job cannot enter: problem %q, want it skipped", problem)
	}
	chmod(blind, 0o311)
	if _, problem := jobLinks(cwd, ""); !strings.Contains(problem, blind) {
		t.Errorf("a directory the job can enter but not list: problem %q, want it named", problem)
	}
}

// The walk stays on the cwd's filesystem: a mount below it is one the export does not
// carry, which readOnlyMounts binds read-only and tmpRefusal judges.
func TestReadLinkDirSkipsADirectoryOnAnotherDevice(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/x", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	if links, _, _, err := readLinkDir(dir, st.Dev); err != nil || len(links) != 1 {
		t.Errorf("its own device: links %q, %v", links, err)
	}
	if links, subdirs, n, err := readLinkDir(dir, st.Dev+1); err != nil || links != nil || subdirs != nil || n != 0 {
		t.Errorf("another device: links %q, subdirs %q, %d entries, %v; want none", links, subdirs, n, err)
	}
}

// gatherFacts refuses a job whose cwd offload cannot walk for links.
func TestGatherFactsRefusesACwdItCannotWalkForLinks(t *testing.T) {
	useTestMounts(t)
	cwd := outsideTmp(t)
	if err := os.MkdirAll(filepath.Join(cwd, "a/b/c"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { linkWalkLimit = n }(linkWalkLimit)
	linkWalkLimit = 2
	if f, _ := gatherJobFacts(t.TempDir(), cwd, "", []string{"make"}, []string{cwd}, nil, nil, exportView{}); !strings.Contains(f.Tmpfs, "more than 2") {
		t.Errorf("Tmpfs %q, want the walk's limit named", f.Tmpfs)
	}
}

// A link onto dev's own machine (a device, /sys, a dev process by its pid) or into a
// private directory is followed unnamed as a link onto a mount is, so it keeps the job here
// too; a link to one of devFiles or to the opener's own process does not, nor one into a
// private directory whose target's directory dev lacks, which leads nowhere either side.
func TestCwdLinkRefusalKeepsAJobWhoseLinkNamesDevsOwnMachine(t *testing.T) {
	cwd := outsideTmp(t)
	other := exec.Command("sleep", "60")
	other.Dir = cwd
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	for _, tt := range []struct {
		target string
		refuse bool
	}{
		{"/dev/mqueue/q", true},
		{"/run/user", true},
		{"/sys/class/net", true},
		{"/proc/1/root/etc", true},
		{"/dev/nvme0n1", true},
		{"/run/offload-absent/state", false}, // dev has no /run/offload-absent either
		{"/dev/null", false},
		{"/proc/self/fd/1", false},
		{"/etc/hostname", false},
		// Its real path is an ordinary directory: only the target as written is dev's own.
		{fmt.Sprintf("/proc/%d/cwd", other.Process.Pid), true},
	} {
		l := filepath.Join(cwd, strings.ReplaceAll(tt.target, "/", "_"))
		if err := os.Symlink(tt.target, l); err != nil {
			t.Fatal(err)
		}
		if got := cwdLinkRefusal(nil, []string{l}); (got != "") != tt.refuse || tt.refuse && !strings.Contains(got, l) {
			t.Errorf("a link to %s: %q, want refused %v", tt.target, got, tt.refuse)
		}
	}
}

// A repository whose tracked links git cannot list (here, through a GIT_DIR naming no
// repository) is a problem, as a walk that cannot see every link is: the tracked ones may
// lead anywhere.
func TestJobLinksSaysWhenGitCannotListTheTrackedLinks(t *testing.T) {
	cwd := outsideTmp(t)
	t.Setenv("GIT_DIR", filepath.Join(cwd, "no-repository"))
	if _, problem := jobLinks(cwd, cwd); !strings.Contains(problem, "could not list the links the repository tracks") {
		t.Errorf("problem %q, want one naming the tracked links", problem)
	}
}

// git refuses a repository another user owns (dubious ownership): its tracked links are
// unknown, not absent. Only "not a git repository" means there are none.
func TestJobLinksSaysWhenGitRefusesTheCwdsRepository(t *testing.T) {
	cwd := outsideTmp(t)
	bin := t.TempDir()
	git := "#!/bin/sh\necho \"fatal: detected dubious ownership in repository at '$PWD'\" >&2\necho advice >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	_, problem := jobLinks(cwd, "")
	if !strings.Contains(problem, "could not tell whether the cwd is in a repository") || !strings.Contains(problem, "dubious ownership") || strings.Contains(problem, "advice") {
		t.Errorf("problem %q, want git's first line, naming the refused repository", problem)
	}
}

func TestJobLinksOutsideARepositoryIsNoProblem(t *testing.T) {
	cwd := outsideTmp(t)
	t.Setenv("GIT_DIR", filepath.Join(cwd, "no-repository"))
	if _, problem := jobLinks(cwd, ""); problem != "" {
		t.Errorf("problem %q, want none outside a repository", problem)
	}
}

// cwdLinks is jobLinks, failing the test on a walk problem.
func cwdLinks(t *testing.T, cwd, repo string) []string {
	t.Helper()
	links, problem := jobLinks(cwd, repo)
	if problem != "" {
		t.Fatal(problem)
	}
	return links
}
