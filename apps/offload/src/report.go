package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// summary is what `offload report` prints for the records of one period.
type summary struct {
	Jobs, Remote, Local int
	LocalByRule         map[string]int
	RemoteCPUSeconds    float64        // CPU time spent off dev
	Failures            map[string]int // by placement: non-zero exits, including 255 (contact lost)
	PressureAvg10       float64        // mean `some avg10` at the end of local runs
	PressureSamples     int
}

// summarise totals the records at or after since.
func summarise(recs []record, since time.Time) summary {
	s := summary{LocalByRule: map[string]int{}, Failures: map[string]int{}}
	var pressure float64
	for _, r := range recs {
		if r.Time.Before(since) {
			continue
		}
		s.Jobs++
		if r.ExitCode != 0 {
			s.Failures[r.Placement]++
		}
		if r.Placement != local {
			s.Remote++
			if r.CPUSeconds != nil {
				s.RemoteCPUSeconds += *r.CPUSeconds
			}
			continue
		}
		s.Local++
		s.LocalByRule[r.Rule]++
		if r.CPUPressure != nil {
			pressure += *r.CPUPressure
			s.PressureSamples++
		}
	}
	if s.PressureSamples > 0 {
		s.PressureAvg10 = pressure / float64(s.PressureSamples)
	}
	return s
}

// parseSince reads `7d` (days) or a Go duration such as `36h`.
func parseSince(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > int(math.MaxInt64/int64(24*time.Hour)) {
			return 0, fmt.Errorf("bad --since %q", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --since %q", v)
	}
	return d, nil
}

// readRecords parses the decisions log, skipping lines that are not records.
func readRecords(r io.Reader) []record {
	var recs []record
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n') // no cap on a line's length
		var rec record
		if json.Unmarshal(line, &rec) == nil {
			recs = append(recs, rec)
		}
		if err != nil {
			return recs
		}
	}
}

// reportCmd implements `offload report [--since <dur>]`.
func reportCmd(args []string, out io.Writer) int {
	since := "7d"
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "--since":
		since = args[1]
	default:
		fmt.Fprintln(os.Stderr, "usage: offload report [--since <duration>]")
		return 2
	}
	d, err := parseSince(since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "offload:", err)
		return 2
	}
	var recs []record
	if dir, err := stateDir(); err == nil {
		if f, err := os.Open(filepath.Join(dir, "decisions.log")); err == nil {
			recs = readRecords(f)
			f.Close()
		}
	}
	printSummary(out, summarise(recs, time.Now().Add(-d)), since)
	return 0
}

func printSummary(w io.Writer, s summary, since string) {
	fmt.Fprintf(w, "offload, last %s: %d jobs: %d remote, %d local\n", since, s.Jobs, s.Remote, s.Local)
	if len(s.LocalByRule) > 0 {
		fmt.Fprintln(w, "local, by deciding rule:")
		for _, rule := range sortedKeys(s.LocalByRule) {
			fmt.Fprintf(w, "  %-18s %d\n", rule, s.LocalByRule[rule])
		}
	}
	fmt.Fprintf(w, "CPU-seconds run remotely (moved off dev): %.1f\n", s.RemoteCPUSeconds)
	if len(s.Failures) == 0 {
		fmt.Fprintln(w, "failures: none")
	} else {
		fmt.Fprintln(w, "failures (non-zero exit, 255 is contact lost), by placement:")
		for _, p := range sortedKeys(s.Failures) {
			fmt.Fprintf(w, "  %-18s %d\n", p, s.Failures[p])
		}
	}
	if s.PressureSamples == 0 {
		fmt.Fprintln(w, "dev CPU pressure during local batch runs: no samples")
	} else {
		fmt.Fprintf(w, "dev CPU pressure during local batch runs (some avg10, mean of %d): %.1f%%\n", s.PressureSamples, s.PressureAvg10)
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var someAvg10 = regexp.MustCompile(`(?m)^some avg10=([0-9.]+)`)

// parseCPUPressure reads the `some avg10` value from /proc/pressure/cpu text.
func parseCPUPressure(text string) (float64, bool) {
	m := someAvg10.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// cpuPressure samples dev's `some avg10`, nil when /proc/pressure/cpu is unreadable.
func cpuPressure() *float64 {
	b, err := os.ReadFile("/proc/pressure/cpu")
	if err != nil {
		return nil
	}
	if v, ok := parseCPUPressure(string(b)); ok {
		return &v
	}
	return nil
}
