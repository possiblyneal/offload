package main

import "testing"

func slots(free int) map[string]slotState {
	return map[string]slotState{"server": {Total: 2, Free: free}}
}

func TestPlace(t *testing.T) {
	two := map[string]slotState{"a": {Total: 1, Free: 0}, "b": {Total: 1, Free: 1}}
	one := []string{"server"}
	tests := []struct {
		name      string
		req       placeRequest
		placement string
		rule      string
	}{
		{"free slot, no refusal", placeRequest{Remotes: one, Slots: slots(1)}, "server", "free-slot"},
		{"first remote with a free slot", placeRequest{Remotes: []string{"a", "b"}, Slots: two}, "b", "free-slot"},
		{"slot exhaustion", placeRequest{Remotes: one, Slots: slots(0)}, "local", "no-free-slot"},
		{"no remotes at all", placeRequest{}, "local", "no-free-slot"},

		{"interactive tty", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Tty: true}}, "local", "interactive-tty"},
		{"sudo", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Sudo: true}}, "local", "sudo"},
		{"watch or serve list", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{WatchServe: "npm run dev"}}, "local", "watch-serve"},
		{"tmpfs", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Tmpfs: "cwd /tmp/x is on tmpfs"}}, "local", "tmpfs"},
		{"wal", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{WAL: "/r/db-wal"}}, "local", "wal-open"},
		{"refusal beats exhaustion", placeRequest{Remotes: one, Slots: slots(0), Facts: refusalFacts{Sudo: true}}, "local", "sudo"},

		{"where local", placeRequest{Where: "local", Remotes: one, Slots: slots(1)}, "local", "where-local"},
		{"where host", placeRequest{Where: "server", Remotes: one, Slots: slots(1)}, "server", "where-remote"},
		{"where host overrides tty", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Tty: true}}, "server", "where-remote"},
		{"where host overrides sudo", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Sudo: true}}, "server", "where-remote"},
		{"where host overrides watch or serve", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{WatchServe: "vite"}}, "server", "where-remote"},
		{"where host does not override tmpfs", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Tmpfs: "x"}}, "local", "tmpfs"},
		{"remote-local", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{RemoteLocal: "argument ~/.cache/go-build"}}, "local", "remote-local"},
		{"where host does not override remote-local", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{RemoteLocal: "x"}}, "local", "remote-local"},
		{"where host does not override wal", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{WAL: "x"}}, "local", "wal-open"},
		{"wal bare", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{WALBare: "/r/app.db"}}, "local", "wal-bare"},
		{"too long", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{TooLong: 200000}}, "local", "too-long"},
		{"where host does not override too long", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{TooLong: 200000}}, "local", "too-long"},
		{"root", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Root: true}}, "local", "root"},
		{"where host does not override root", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Root: true}}, "local", "root"},
		{"special file", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Special: "ff is a FIFO on dev"}}, "local", "special-file"},
		{"where host does not override a special file", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Special: "ff is a FIFO on dev"}}, "local", "special-file"},
		{"unseen holder", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Unseen: "3 processes"}}, "local", "unseen-holder"},
		{"where host does not override an unseen holder", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Unseen: "3 processes"}}, "local", "unseen-holder"},
		{"foreign holder", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Foreign: "/data/app.db-wal"}}, "local", "foreign-holder"},
		{"where host does not override a foreign holder", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Foreign: "/data/app.db-wal"}}, "local", "foreign-holder"},
		{"newline in a path", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Newline: "/a\nb"}}, "local", "newline-path"},
		{"where host does not override a newline", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Newline: "/a\nb"}}, "local", "newline-path"},
		{"too many groups", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Groups: 17}}, "local", "groups"},
		{"where host does not override too many groups", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Groups: 17}}, "local", "groups"},
		{"read-only cwd", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{ReadOnlyCwd: "/home/testuser/Downloads"}}, "local", "read-only-cwd"},
		{"where host overrides a read-only cwd", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{ReadOnlyCwd: "/home/testuser/Downloads"}}, "server", "where-remote"},
		{"flock held", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Flock: "x"}}, "local", "flock-held"},
		{"where host does not override a held flock", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Flock: "x"}}, "local", "flock-held"},
		{"closed stdio", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{ClosedStdio: "stdout"}}, "local", "closed-stdio"},
		{"where host does not override an output file it cannot reach", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{OutputFile: "o"}}, "local", "output-file"},
		{"inherited fd", placeRequest{Remotes: one, Slots: slots(1), Facts: refusalFacts{Fd: "fd 63 (pipe:[1])"}}, "local", "inherited-fd"},
		{"where host does not override an inherited fd", placeRequest{Where: "server", Remotes: one, Slots: slots(1), Facts: refusalFacts{Fd: "fd 3 (/x)"}}, "local", "inherited-fd"},
		{"where host still needs a free slot", placeRequest{Where: "server", Remotes: one, Slots: slots(0)}, "local", "no-free-slot"},
		{"where local with wal is local", placeRequest{Where: "local", Remotes: one, Slots: slots(1), Facts: refusalFacts{WAL: "x"}}, "local", "where-local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := place(tt.req)
			if got.Placement != tt.placement || got.Rule != tt.rule {
				t.Errorf("place = %q by %q, want %q by %q", got.Placement, got.Rule, tt.placement, tt.rule)
			}
			if got.Reason == "" {
				t.Errorf("decision has no reason")
			}
		})
	}
}

func TestGroupsOverNFSIsTheCountPastSixteen(t *testing.T) {
	if got := groupsOverNFS(make([]int, 16)); got != 0 {
		t.Errorf("16 groups: got %d, want 0", got)
	}
	if got := groupsOverNFS(make([]int, 17)); got != 17 {
		t.Errorf("17 groups: got %d, want 17", got)
	}
}

// A remote with no free slot is never picked, and with none free the job runs here.
func TestTakeSlotSkipsAFullRemote(t *testing.T) {
	r := placeRequest{Slots: map[string]slotState{"a": {Total: 2, Free: 0}, "b": {Total: 1, Free: 1}}}
	if d := takeSlot(r, []string{"a", "b"}, "free-slot"); d.Placement != "b" {
		t.Errorf("got %+v, want b", d)
	}
	if d := takeSlot(r, []string{"a"}, "free-slot"); d.Placement != local || d.Rule != "no-free-slot" || d.Reason != "a has no free slot" {
		t.Errorf("got %+v, want local by no-free-slot, naming a", d)
	}
}
