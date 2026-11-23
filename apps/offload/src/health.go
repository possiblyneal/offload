package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// healthFile is the check's state beside the decisions log: when it last ran and what it
// reported, so the next run scans only the records since and sends a full event only when
// the status changes.
const healthFile = "health.json"

// firstWindow is how far back a run with no saved state looks: the timer's cadence.
const firstWindow = 30 * time.Minute

type healthState struct {
	LastRun time.Time `json:"last_run"`
	Status  string    `json:"status"`
}

// probe is one remote's answer to a test job: an empty err means the job ran there.
type probe struct {
	remote, err string
}

// verdict is what one run of the check reports, in the homelab event's terms.
type verdict struct {
	Status, Title, Body string
}

// assess judges offload from the probes and the records of jobs that ended in [since,
// until): a record is written when its job ends, and stamped with its start, so a long job
// counts in the run after it ends. A remote that cannot run the probe fails the check; jobs
// that lost contact with their remote (exit 255 with the wrapper's own fault) or ran on dev
// because no remote would take them (`remote-unreachable`) warn. A job's own failure is not
// offload's, and its own 255 carries no fault.
func assess(recs []record, since, until time.Time, probes []probe) verdict {
	var down, lines []string
	for _, p := range probes {
		if p.err != "" {
			down = append(down, p.remote)
			lines = append(lines, fmt.Sprintf("test job on %s: %s", p.remote, p.err))
		}
	}
	lost := map[string]int{}
	unreachable := 0
	for _, r := range recs {
		end := r.Time.Add(time.Duration(r.WallSeconds * float64(time.Second)))
		switch {
		case end.Before(since) || !end.Before(until):
		case r.Placement == local:
			if r.Rule == "remote-unreachable" {
				unreachable++
			}
		case r.ExitCode == 255 && r.Fault != "":
			lost[r.Placement]++
		}
	}
	jobs := unreachable
	if n := total(lost); n > 0 {
		jobs += n
		lines = append(lines, fmt.Sprintf("%d job(s) lost contact with their remote (exit 255): %s", n, byRemote(lost)))
	}
	if unreachable > 0 {
		lines = append(lines, fmt.Sprintf("%d job(s) ran on dev because their remote never took them (remote-unreachable)", unreachable))
	}
	switch {
	case len(down) > 0:
		lines = append(lines, "Every job runs on dev until it answers.")
		return verdict{"fail", "offload: " + strings.Join(down, ", ") + " cannot run jobs", strings.Join(lines, "\n")}
	case jobs > 0:
		lines = append(lines, "Since "+since.Format(time.RFC3339)+"; `offload report` has the totals.")
		return verdict{"warn", fmt.Sprintf("offload: %d job(s) hit a remote fault", jobs), strings.Join(lines, "\n")}
	}
	return verdict{"ok", "offload: healthy", ""}
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func byRemote(m map[string]int) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// runProbe runs hostname through normal placement from a private directory on the /tmp
// export. The caller's cwd, state and environment may name mounts the worker cannot see.
func runProbe(self, remote string) probe {
	dir, err := os.MkdirTemp("/tmp", "offload-health-")
	if err != nil {
		return probe{remote, err.Error()}
	}
	defer os.RemoveAll(dir)
	log := filepath.Join(dir, "probe.log")
	cmd := exec.Command(self, "--where", remote, "--", "hostname")
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", logOverride + "=" + log}
	if sock, ok := os.LookupEnv("SSH_AUTH_SOCK"); ok {
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+sock)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := ""
	if err := cmd.Run(); err != nil {
		runErr = fmt.Sprintf("%v: %s", err, lastLine(stderr.String()))
	}
	var recs []record
	if f, err := os.Open(log); err == nil {
		recs = readRecords(f)
		f.Close()
	}
	return judgeProbe(remote, recs, runErr)
}

// judgeProbe reads a test job's own record. It ran on remote, or ran on dev only because
// every slot there was busy or another wrapper held the jobs lock: the remote is up.
// Anything else (no record, another reason to run on dev, a fault on the remote, a failed
// `hostname`) is the remote down.
func judgeProbe(remote string, recs []record, runErr string) probe {
	if len(recs) == 0 {
		return probe{remote, cmp.Or(runErr, "the test job left no record")}
	}
	r := recs[len(recs)-1]
	switch {
	case r.Placement == local && (r.Rule == "no-free-slot" || r.Rule == "jobs-lock"):
		return probe{remote: remote}
	case r.Placement == local:
		return probe{remote, "ran on dev (" + r.Rule + "): " + r.Reason}
	case r.Fault != "":
		return probe{remote, r.Fault}
	case runErr != "":
		return probe{remote, runErr}
	}
	return probe{remote: remote}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// healthCmd implements `offload health [--notify]`: probe every remote, judge the records
// since the last run and print the verdict. --notify also sends it to the homelab panel as
// report `offload` and saves the state; an unchanged status goes as a heartbeat, so an
// ongoing fault alerts once. A full event that cannot be sent exits 1 with the state
// unsaved, so the next run sends it again.
func healthCmd(args []string, out io.Writer) int {
	notify := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--notify":
		notify = true
	default:
		fmt.Fprintln(os.Stderr, "usage: offload health [--notify]")
		return 2
	}
	if err := health(notify, out); err != nil {
		fmt.Fprintln(os.Stderr, "offload:", err)
		return 1
	}
	return 0
}

func health(notify bool, out io.Writer) error {
	now := time.Now()
	f, err := os.Open(filepath.Join(configDir, "offload.conf"))
	if err != nil {
		return fmt.Errorf("read config (was it built by install.sh?): %w", err)
	}
	cfg, err := parseConfig(f)
	f.Close()
	if err != nil {
		return err
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	prev := healthState{LastRun: now.Add(-firstWindow)}
	if b, err := os.ReadFile(filepath.Join(dir, healthFile)); err == nil {
		if err := json.Unmarshal(b, &prev); err != nil {
			return fmt.Errorf("%s: %w", healthFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var probes []probe
	for _, name := range cfg.order {
		probes = append(probes, runProbe(self, name))
	}
	var recs []record
	if lf, err := os.Open(filepath.Join(dir, "decisions.log")); err == nil {
		recs = readRecords(lf)
		lf.Close()
	}
	v := assess(recs, prev.LastRun, now, probes)
	fmt.Fprintf(out, "%s: %s\n", v.Status, v.Title)
	if v.Body != "" {
		fmt.Fprintln(out, v.Body)
	}
	if !notify {
		return nil
	}

	heartbeat := v.Status == prev.Status
	argv := []string{"--stream", "errors", "--report", "offload", "--status", v.Status, "--title", v.Title}
	if v.Body != "" {
		argv = append(argv, "--body", v.Body)
	}
	if heartbeat {
		argv = append(argv, "--heartbeat")
	}
	send := exec.Command("homelab-notify", argv...)
	send.Stdout, send.Stderr = os.Stderr, os.Stderr
	if err := send.Run(); err != nil && !heartbeat {
		return fmt.Errorf("homelab-notify: %w", err)
	}
	b, err := json.Marshal(healthState{LastRun: now, Status: v.Status})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, healthFile), append(b, '\n'), 0o644)
}
