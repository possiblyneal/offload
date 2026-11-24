package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeDoesNotInheritCallerPaths(t *testing.T) {
	// The listener itself needs a short address even when the runner's TMPDIR is deep.
	t.Setenv("TMPDIR", "/tmp")
	root := t.TempDir()
	state := filepath.Join(root, strings.Repeat("x", 110))
	t.Setenv("OFFLOAD_TEST_UNEXPORTED", root)
	t.Setenv("SSH_AUTH_SOCK", "link/../agent.sock")
	t.Setenv("OFFLOAD_JOB", "1")
	target := filepath.Join(root, "nested")
	for _, dir := range []string{state, target} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(state, "link")); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	wrapper := filepath.Join(state, "wrapper")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
set -eu
[ "${OFFLOAD_TEST_UNEXPORTED-unset}" = unset ]
[ "$HOME" = "$PWD" ]
[ "${OFFLOAD_DECISIONS_LOG%/*}" = "$PWD" ]
[ "$PWD" != "${0%/*}" ]
[ "${#SSH_AUTH_SOCK}" -lt 108 ]
[ "${SSH_AUTH_SOCK%/*}" != "$PWD" ]
[ -S "$SSH_AUTH_SOCK" ]
[ "$SSH_AUTH_SOCK" -ef "${0%/*}/link/../agent.sock" ]
[ "$OFFLOAD_JOB" = 1 ]
printf '{"placement":"worker"}\n' > "$OFFLOAD_DECISIONS_LOG"
`), 0700); err != nil {
		t.Fatal(err)
	}
	if got := runProbe(wrapper, state, "worker"); got != (probe{remote: "worker"}) {
		t.Fatalf("probe = %+v", got)
	}
}

func TestAssess(t *testing.T) {
	since := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return since.Add(time.Duration(min) * time.Minute) }
	up := []probe{{remote: "server"}}
	tests := []struct {
		name   string
		recs   []record
		probes []probe
		status string
		title  string
		body   []string // substrings the body must hold
	}{
		{
			name:   "quiet",
			probes: up,
			status: "ok", title: "offload: healthy",
		},
		{
			name: "a job's own failures are not offload's",
			recs: []record{
				{Time: at(1), Placement: "server", ExitCode: 1},
				{Time: at(2), Placement: local, Rule: "tmpfs", ExitCode: 255},
				{Time: at(3), Placement: local, Rule: "no-free-slot"},
			},
			probes: up,
			status: "ok", title: "offload: healthy",
		},
		{
			name: "faults before the window are already reported",
			recs: []record{
				{Time: at(-1), Placement: "server", ExitCode: 255},
				{Time: at(-1), Placement: local, Rule: "remote-unreachable"},
			},
			probes: up,
			status: "ok", title: "offload: healthy",
		},
		{
			name: "lost contact, failed starts and fallbacks warn",
			recs: []record{
				{Time: at(1), Placement: "server", ExitCode: 255, Fault: "lost contact"},
				{Time: at(2), Placement: "server", ExitCode: 255, Fault: "watchdog"},
				{Time: at(4), Placement: local, Rule: "remote-unreachable"},
			},
			probes: up,
			status: "warn", title: "offload: 3 job(s) hit a remote fault",
			body: []string{
				"2 job(s) lost contact with their remote (exit 255): server 2",
				"1 job(s) ran on dev because their remote never took them",
			},
		},
		{
			name: "a long job counts when it ends, not when it starts",
			recs: []record{
				{Time: at(-50), WallSeconds: 3600, Placement: "server", ExitCode: 255, Fault: "lost"},
				{Time: at(50), WallSeconds: 3600, Placement: "server", ExitCode: 255, Fault: "ends after this run"},
			},
			probes: up,
			status: "warn", title: "offload: 1 job(s) hit a remote fault",
		},
		{
			name:   "a remote that cannot run the test job fails",
			recs:   []record{{Time: at(1), Placement: local, Rule: "remote-unreachable"}},
			probes: []probe{{remote: "server", err: "ran on dev: no answer"}, {remote: "spare"}},
			status: "fail", title: "offload: server cannot run jobs",
			body: []string{"test job on server: ran on dev: no answer", "remote-unreachable"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := assess(tt.recs, since, since.Add(time.Hour), tt.probes)
			if v.Status != tt.status || v.Title != tt.title {
				t.Fatalf("got %s %q, want %s %q", v.Status, v.Title, tt.status, tt.title)
			}
			for _, s := range tt.body {
				if !strings.Contains(v.Body, s) {
					t.Errorf("body lacks %q:\n%s", s, v.Body)
				}
			}
		})
	}
}

func TestJudgeProbe(t *testing.T) {
	tests := []struct {
		name   string
		recs   []record
		runErr string
		want   string // the probe's err; empty is up
	}{
		{name: "ran there", recs: []record{{Placement: "server"}}},
		{name: "busy remote is up", recs: []record{{Placement: local, Rule: "no-free-slot"}}},
		{name: "a held jobs lock is not the remote", recs: []record{{Placement: local, Rule: "jobs-lock"}}},
		{
			name: "never took the job",
			recs: []record{{Placement: local, Rule: "remote-unreachable", Reason: "server never let the job go"}},
			want: "ran on dev (remote-unreachable): server never let the job go",
		},
		{name: "lost contact", recs: []record{{Placement: "server", ExitCode: 255, Fault: "lost contact"}}, want: "lost contact"},
		{name: "no record", runErr: "exit status 1: offload: unknown remote", want: "exit status 1: offload: unknown remote"},
		{name: "no record, no error", want: "the test job left no record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := judgeProbe("server", tt.recs, tt.runErr); got.err != tt.want {
				t.Errorf("err = %q, want %q", got.err, tt.want)
			}
		})
	}
}
