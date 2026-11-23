package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendRecordWritesOneJSONObjectPerLine(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	cpu := 1.5
	appendRecord(record{Time: time.Unix(0, 0).UTC(), Argv: []string{"true"}, Cwd: "/r", Placement: "local",
		Rule: "sudo", Reason: "why", WallSeconds: 2, CPUSeconds: &cpu, ExitCode: 3})
	appendRecord(record{Time: time.Unix(0, 0).UTC(), Argv: []string{"x", "y"}, Cwd: "/r", Placement: "server",
		Rule: "free-slot", Reason: "why", WallSeconds: 1})

	data, err := os.ReadFile(filepath.Join(state, "offload", "decisions.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), data)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"time", "argv", "cwd", "placement", "rule", "wall_seconds", "cpu_seconds", "exit_code"} {
		if _, ok := first[k]; !ok {
			t.Errorf("record has no %q: %s", k, lines[0])
		}
	}
	if first["cpu_seconds"] != 1.5 || first["exit_code"] != 3.0 || first["placement"] != "local" {
		t.Errorf("first record = %s", lines[0])
	}
	if v, ok := second["cpu_seconds"]; !ok || v != nil {
		t.Errorf("an unreported CPU time is null, got %v in %s", v, lines[1])
	}
}

func TestParseRecordsCPU(t *testing.T) {
	var s controlState
	s.feed("started")
	s.feed("cpu 1250")
	s.feed("status exit 0")
	if out := decide(s, 0); !out.cpuKnown || out.cpuMs != 1250 {
		t.Errorf("outcome = %+v, want cpu 1250 ms", out)
	}
	s = controlState{}
	s.feed("noslot")
	if out := decide(s, 0); !out.noSlot || !out.fallback {
		t.Errorf("outcome = %+v, want fallback because no slot", out)
	}
}
