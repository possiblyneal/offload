package main

import (
	"strings"
	"testing"
)

// --explain prints a refusal line for every refusal that applies, not only the one that
// decides.
func TestExplainPrintsEveryRefusalThatApplies(t *testing.T) {
	f := refusalFacts{
		Tty: true, Sudo: true, WatchServe: "w", Tmpfs: "t", RemoteLocal: "r", WAL: "/a-wal", WALBare: "/b.db",
		Flock: "/l", Special: "s", Unseen: "u", Foreign: "/f-wal", TooLong: 1, Fd: "fd 3", ClosedStdio: "stdout", OutputFile: "o", Root: true, Newline: "/n",
		Groups: 17, ReadOnlyCwd: "/ro",
	}
	var b strings.Builder
	req := placeRequest{Facts: f}
	explainPlacement(&b, req, place(req))
	for _, rule := range []string{"tmpfs", "remote-local", "special-file", "wal-open", "wal-bare", "flock-held", "unseen-holder", "foreign-holder",
		"too-long", "inherited-fd", "closed-stdio", "output-file", "root", "newline-path", "groups", "interactive-tty", "sudo",
		"watch-serve", "read-only-cwd"} {
		if !strings.Contains(b.String(), "offload: refusal "+rule+": ") {
			t.Errorf("no %s refusal in\n%s", rule, b.String())
		}
	}
	if !strings.Contains(b.String(), "offload: placement local by tmpfs: ") {
		t.Errorf("the deciding line is not tmpfs's:\n%s", b.String())
	}
}

// With no refusal --explain prints none.
func TestExplainPrintsNoRefusalForACleanJob(t *testing.T) {
	var b strings.Builder
	req := placeRequest{Remotes: []string{"s"}, Slots: map[string]slotState{"s": {Total: 1, Free: 1}}}
	explainPlacement(&b, req, place(req))
	if strings.Contains(b.String(), "refusal") {
		t.Errorf("printed a refusal:\n%s", b.String())
	}
}

// A remote --explain's probe cannot reach is reported as unknown and assumed free, as a run
// assumes, so the decision is the run's: it then finds the remote unreachable.
func TestExplainSaysWhenASlotProbeFails(t *testing.T) {
	var b strings.Builder
	req := placeRequest{Remotes: []string{"s"}, Slots: map[string]slotState{"s": {Total: 2, Free: 2, Unknown: "exit status 255"}}}
	d := place(req)
	explainPlacement(&b, req, d)
	if !strings.Contains(b.String(), "offload: slots s unknown (exit status 255), assumed free\n") || d.Placement != "s" {
		t.Errorf("placement %s, printed:\n%s", d.Placement, b.String())
	}
}

// refusals weighs the rules in the order apps/offload/CLAUDE.md documents, so the first
// that applies is the one place reports.
func TestRefusalsAreInTheDocumentedOrder(t *testing.T) {
	f := refusalFacts{
		Tty: true, Sudo: true, WatchServe: "w", Tmpfs: "t", RemoteLocal: "r", WAL: "/a-wal", WALBare: "/b.db",
		Flock: "/l", Special: "s", Unseen: "u", Foreign: "/f-wal", TooLong: 1, Fd: "fd 3", ClosedStdio: "stdout", OutputFile: "o", Root: true, Newline: "/n",
		Groups: 17, ReadOnlyCwd: "/ro",
	}
	rules := func(ds []decision) string {
		var names []string
		for _, d := range ds {
			names = append(names, d.Rule)
		}
		return strings.Join(names, " ")
	}
	hard, soft := refusals(f)
	if got, want := rules(hard), "tmpfs remote-local wal-open wal-bare flock-held foreign-holder unseen-holder special-file too-long inherited-fd closed-stdio output-file root newline-path groups"; got != want {
		t.Errorf("hard refusals:\n got %s\nwant %s", got, want)
	}
	if got, want := rules(soft), "interactive-tty sudo watch-serve read-only-cwd"; got != want {
		t.Errorf("soft refusals:\n got %s\nwant %s", got, want)
	}
}
