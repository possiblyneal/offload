package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// config is config/offload.conf: the address remotes mount dev on, and the
// remotes themselves by name.
type config struct {
	dev     string
	remotes map[string]string
	order   []string       // the remotes in file order: the order placement tries them
	slots   map[string]int // jobs a remote accepts at once; unset means 1

	grace     int               // seconds between the forwarded signal and stopping the scope
	cpuQuota  map[string]string // per remote; unset means threads x 100% less one core
	memoryMax map[string]string // per remote; unset means 75% of its RAM
}

const defaultGrace = 10

// parseConfig reads `dev <addr>`, `remote <name> <addr>` and `slots <name> <n>` lines; blank lines
// and `#` comments are skipped.
func parseConfig(r io.Reader) (config, error) {
	cfg := config{
		remotes:   map[string]string{},
		slots:     map[string]int{},
		grace:     defaultGrace,
		cpuQuota:  map[string]string{},
		memoryMax: map[string]string{},
	}
	lines, err := contentLines(r)
	if err != nil {
		return config{}, err
	}
	for _, line := range lines {
		f := strings.Fields(line)
		switch {
		case f[0] == "dev" && len(f) == 2:
			cfg.dev = f[1]
		case f[0] == "remote" && len(f) == 3:
			cfg.remotes[f[1]] = f[2]
			cfg.order = append(cfg.order, f[1])
		case f[0] == "slots" && len(f) == 3:
			count, err := strconv.Atoi(f[2])
			if err != nil || count < 1 {
				return config{}, fmt.Errorf("offload.conf: bad line %q", line)
			}
			cfg.slots[f[1]] = count
		case f[0] == "grace" && len(f) == 2:
			g, err := strconv.Atoi(f[1])
			if err != nil || g < 0 {
				return config{}, fmt.Errorf("offload.conf: bad line %q", line)
			}
			cfg.grace = g
		case f[0] == "cpu-quota" && len(f) == 3:
			cfg.cpuQuota[f[1]] = f[2]
		case f[0] == "memory-max" && len(f) == 3:
			cfg.memoryMax[f[1]] = f[2]
		default:
			return config{}, fmt.Errorf("offload.conf: bad line %q", line)
		}
	}
	return cfg, nil
}

// slotCount is how many jobs the remote accepts at once: its `slots` line, else 1.
func (c config) slotCount(name string) int {
	if n := c.slots[name]; n > 0 {
		return n
	}
	return 1
}

// contentLines is r's lines, trimmed, without blank lines and `#` comments: the shape
// of every file in config/.
func contentLines(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}
