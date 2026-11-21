package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// record is one line of ${XDG_STATE_HOME:-~/.local/state}/offload/decisions.log: a
// decision and its outcome. CPUSeconds is null when the machine that ran the job did
// not report it.
type record struct {
	Time        time.Time `json:"time"`
	Argv        []string  `json:"argv"`
	Cwd         string    `json:"cwd"`
	Placement   string    `json:"placement"`
	Rule        string    `json:"rule"`
	Reason      string    `json:"reason"`
	WallSeconds float64   `json:"wall_seconds"`
	CPUSeconds  *float64  `json:"cpu_seconds"`
	ExitCode    int       `json:"exit_code"`
	// Fault is the wrapper's own account of a remote run that went wrong (lost contact,
	// a cut output stream), empty when the job simply ended with its own code.
	Fault string `json:"fault,omitempty"`
	// CPUPressure is dev's `some avg10` from /proc/pressure/cpu at the end of a local
	// run; null on a remote run or when the file cannot be read.
	CPUPressure *float64 `json:"cpu_pressure_avg10"`
}

// stateDir is the folder of the decisions log.
func stateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "offload"), nil
}

// logOverride, when set, is the file a run logs its record to in place of the decisions
// log: `offload health` points each test job at a file of its own, so the check's jobs
// never count as real ones and it reads how each was placed.
const logOverride = "OFFLOAD_DECISIONS_LOG"

// appendRecord adds r to the decisions log. A log that cannot be written is skipped:
// it must never fail the job.
func appendRecord(r record) {
	path := os.Getenv(logOverride)
	if path == "" {
		dir, err := stateDir()
		if err != nil || os.MkdirAll(dir, 0o755) != nil {
			return
		}
		path = filepath.Join(dir, "decisions.log")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}
