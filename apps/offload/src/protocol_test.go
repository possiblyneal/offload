package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The control protocol is spoken by three programs: the wrapper (control.go), the launcher
// (offload-launch) and the job's init (cmd/offload-wait). These tests read the words each
// one uses from its source, so a record or signal added on one side and not the others
// fails here rather than on a remote.

// waitSource is the job's init.
const waitSource = "cmd/offload-wait/main.go"

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// matches returns the first group of every match of re in s, sorted and deduplicated.
func matches(s, re string) []string {
	var out []string
	for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Every status the launcher can send is one the wrapper's feed records, and every status
// feed records is one the launcher sends.
func TestTheWrapperReadsEveryStatusTheLauncherSends(t *testing.T) {
	sent := matches(readSource(t, "offload-launch"), `echo "status ([a-z-]+)`)
	if len(sent) == 0 {
		t.Fatal("offload-launch sends no status")
	}
	for _, w := range sent {
		s := controlState{goSent: true}
		s.feed("status " + w + " 7")
		if s.status == "" {
			s.feed("status " + w)
		}
		if s.status == "" {
			t.Errorf("the launcher sends `status %s`, which the wrapper ignores", w)
		}
	}
	var statusCases strings.Builder
	for _, l := range strings.Split(readSource(t, "control.go"), "\n") {
		if strings.Contains(l, `f[0] == "status"`) {
			statusCases.WriteString(l + "\n")
		}
	}
	for _, w := range matches(statusCases.String(), `f\[1\] == "([a-z-]+)"`) {
		if !slices.Contains(sent, w) {
			t.Errorf("the wrapper reads `status %s`, which the launcher never sends", w)
		}
	}
}

// The single-word records the launcher sends are each one the wrapper's feed records, or
// the heartbeat, which only resets its silence clock.
func TestTheWrapperReadsEveryRecordTheLauncherSends(t *testing.T) {
	for _, w := range matches(readSource(t, "offload-launch"), `echo ([a-z]+) >&4`) {
		if w == "hb" {
			continue
		}
		var s controlState
		before := s
		s.feed(w)
		if s == before {
			t.Errorf("the launcher sends `%s`, which the wrapper ignores", w)
		}
	}
	s := controlState{goSent: true}
	s.feed("cpu 12")
	if !s.cpuSeen || s.cpuMs != 12 {
		t.Error("the wrapper ignores the launcher's `cpu <ms>`")
	}
	launch := readSource(t, "offload-launch")
	if !strings.Contains(launch, `echo "offset $stream $n" >&4`) || !strings.Contains(launch, `^(out|err)$`) {
		t.Error("the launcher no longer sends `offset out|err <n>` as the wrapper reads it")
	}
	for i, stream := range []string{"out", "err"} {
		s.feed("offset " + stream + " 34")
		if !s.offsetSeen[i] || s.offsets[i] != 34 {
			t.Errorf("the wrapper ignores the launcher's `offset %s <n>`", stream)
		}
	}
}

// The stdio plan the wrapper writes is the one the launcher and offload-wait read: its
// variable, its `err merged` line and its file lines, and the descriptor offload-wait
// reports offsets on is the one the launcher opens.
func TestTheStdioPlanIsReadAsTheWrapperWritesIt(t *testing.T) {
	launch, wait := readSource(t, "offload-launch"), readSource(t, waitSource)
	plan := stdioPlan{merged: true, files: []outputFile{{stream: "out", fd: 1, path: "/a b", offset: 7}, {stream: "err", fd: 2, path: "/c", appendMode: true, readWrite: true}}}
	got := strings.Split(plan.env(), "\n")
	if want := []string{"err merged", "out file w 7 /a b", "err file ra 0 /c"}; !slices.Equal(got, want) {
		t.Fatalf("env() = %q, want %q", got, want)
	}
	if !strings.Contains(launch, "stdio=${OFFLOAD_STDIO:-}") || !strings.Contains(launch, `*$'\nerr merged\n'*`) {
		t.Error("the launcher no longer reads OFFLOAD_STDIO's `err merged`")
	}
	if !strings.Contains(wait, `line == "err merged"`) {
		t.Error("offload-wait no longer reads `err merged`")
	}
	re := regexp.MustCompile(matches(wait, "planLine = regexp.MustCompile\\(`([^`]+)`")[0])
	for _, l := range got[1:] {
		if !re.MatchString(l) {
			t.Errorf("offload-wait cannot read the plan line %q", l)
		}
	}
	fd := matches(wait, `offsetFD += ([0-9]+)`)
	if len(fd) != 1 || !strings.Contains(launch, "exec "+fd[0]+`>"$ctl/offsets"`) {
		t.Errorf("offload-wait reports offsets on fd %v, which the launcher does not open on $ctl/offsets", fd)
	}
}

// Each signal the wrapper relays has a name the launcher acts on, and the launcher's ending
// and informational sets match the wrapper's and offload-wait's.
func TestEverySignalNameIsKnownToTheLauncherAndTheJobsInit(t *testing.T) {
	launch := readSource(t, "offload-launch")
	acted := matches(launch, `\$sig == ([A-Z0-9]+)`)
	relayed := []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2}
	var ending, informationalNames []string
	for _, s := range relayed {
		n := signalName(s)
		if !slices.Contains(acted, n) {
			t.Errorf("the wrapper sends `sig %s`, which the launcher ignores", n)
		}
		if informational(s) {
			informationalNames = append(informationalNames, n)
		} else {
			ending = append(ending, n)
		}
	}
	for _, n := range []string{"TSTP", "CONT"} { // Ctrl-Z and fg, sent literally
		if !slices.Contains(acted, n) {
			t.Errorf("the wrapper sends `sig %s`, which the launcher ignores", n)
		}
	}
	slices.Sort(ending)
	slices.Sort(informationalNames)

	endingLine := regexp.MustCompile(`\[\[ \$sig == INT[^\]]*\]\]`).FindString(launch)
	if got := matches(endingLine, `== ([A-Z0-9]+)`); !slices.Equal(got, ending) {
		t.Errorf("the launcher ends the job on %q, the wrapper on %q", got, ending)
	}
	usrLine := regexp.MustCompile(`\[\[ \$sig == USR1[^\]]*\]\]`).FindString(launch)
	if got := matches(usrLine, `== ([A-Z0-9]+)`); !slices.Equal(got, informationalNames) {
		t.Errorf("the launcher passes on %q and runs on, the wrapper %q", got, informationalNames)
	}

	wait := readSource(t, waitSource)
	mapKeys := func(name string) []string {
		return matches(regexp.MustCompile(`var `+name+` = map\[string\]syscall.Signal\{[^}]*\}`).FindString(wait), `"([A-Z0-9]+)"`)
	}
	if info := mapKeys("informational"); !slices.Equal(info, informationalNames) {
		t.Errorf("offload-wait runs on after %q, the wrapper after %q", info, informationalNames)
	}
	if got := mapKeys("ending"); !slices.Equal(got, ending) {
		t.Errorf("offload-wait reads %q as ending signals on abort-sig, the launcher ends on %q", got, ending)
	}
	caught := matches(regexp.MustCompile(`var handled = \[\]os.Signal\{[^}]*\}`).FindString(wait), `syscall\.SIG([A-Z0-9]+)`)
	caught = slices.DeleteFunc(caught, func(n string) bool { return n == "PIPE" || slices.Contains(informationalNames, n) })
	if !slices.Equal(caught, ending) {
		t.Errorf("offload-wait catches %q (less PIPE and the informational), the launcher ends on %q", caught, ending)
	}
}
