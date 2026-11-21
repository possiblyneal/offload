package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestSummariseCountsPlacementsAndCPUMovedOffDev(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	recs := []record{
		{Time: now.Add(-time.Hour), Placement: "server", Rule: "free-slot", CPUSeconds: f64(60)},
		{Time: now.Add(-time.Hour), Placement: "server", Rule: "where-remote", CPUSeconds: f64(40)},
		{Time: now.Add(-time.Hour), Placement: "server", Rule: "free-slot"}, // CPU unreported
		{Time: now.Add(-time.Hour), Placement: "local", Rule: "sudo", CPUSeconds: f64(5)},
		{Time: now.Add(-time.Hour), Placement: "local", Rule: "sudo", CPUSeconds: f64(5)},
		{Time: now.Add(-time.Hour), Placement: "local", Rule: "no-free-slot", CPUSeconds: f64(5)},
		{Time: now.Add(-8 * 24 * time.Hour), Placement: "server", Rule: "free-slot", CPUSeconds: f64(1000)}, // too old
	}
	s := summarise(recs, now.Add(-7*24*time.Hour))
	if s.Jobs != 6 || s.Remote != 3 || s.Local != 3 {
		t.Errorf("jobs/remote/local = %d/%d/%d, want 6/3/3", s.Jobs, s.Remote, s.Local)
	}
	if s.LocalByRule["sudo"] != 2 || s.LocalByRule["no-free-slot"] != 1 {
		t.Errorf("LocalByRule = %v", s.LocalByRule)
	}
	if s.RemoteCPUSeconds != 100 {
		t.Errorf("RemoteCPUSeconds = %v, want 100", s.RemoteCPUSeconds)
	}
}

func TestSummariseFailuresByPlacementAndPressure(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	recs := []record{
		{Time: now, Placement: "server", ExitCode: 0},
		{Time: now, Placement: "server", ExitCode: 1},
		{Time: now, Placement: "server", ExitCode: 255}, // contact lost
		{Time: now, Placement: "local", ExitCode: 137},
		{Time: now, Placement: "local", CPUPressure: f64(10)},
		{Time: now, Placement: "local", CPUPressure: f64(30)},
		{Time: now, Placement: "local"}, // pressure unreadable
	}
	s := summarise(recs, now.Add(-time.Hour))
	if s.Failures["server"] != 2 || s.Failures["local"] != 1 {
		t.Errorf("Failures = %v", s.Failures)
	}
	if s.PressureSamples != 2 || s.PressureAvg10 != 20 {
		t.Errorf("pressure = %v over %d samples, want 20 over 2", s.PressureAvg10, s.PressureSamples)
	}
}

func TestParseSinceAcceptsDaysAndGoDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		got, err := parseSince(in)
		if err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-1d", "soon", "200000d", "9999999999d"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) succeeded", in)
		}
	}
}

func TestReportPrintsSummaryFromLogInStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "offload")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	log := `{"time":"` + recent + `","placement":"server","rule":"free-slot","exit_code":0,"cpu_seconds":62.5}
{"time":"` + recent + `","placement":"local","rule":"sudo","exit_code":1,"cpu_seconds":2,"cpu_pressure_avg10":12.5}
not json
{"time":"` + old + `","placement":"server","rule":"free-slot","exit_code":0,"cpu_seconds":999}
`
	if err := os.WriteFile(filepath.Join(dir, "decisions.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := reportCmd([]string{"--since", "7d"}, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, want := range []string{"2 jobs", "1 remote", "1 local", "sudo", "62.5", "12.5"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "999") {
		t.Errorf("report counted a record older than --since:\n%s", out.String())
	}
}

func TestReportWithNoLogSaysSo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	if code := reportCmd(nil, &out); code != 0 || !strings.Contains(out.String(), "0 jobs") {
		t.Errorf("exit %d, output %q", code, out.String())
	}
}

func TestReportRejectsBadSince(t *testing.T) {
	var out bytes.Buffer
	if code := reportCmd([]string{"--since", "soon"}, &out); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestParseCPUPressureReadsSomeAvg10(t *testing.T) {
	in := "some avg10=3.25 avg60=1.00 avg300=0.50 total=12345\n"
	v, ok := parseCPUPressure(in)
	if !ok || v != 3.25 {
		t.Errorf("= %v, %v", v, ok)
	}
	if _, ok := parseCPUPressure("garbage"); ok {
		t.Error("garbage parsed")
	}
}
