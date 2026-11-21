package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// forward is one dev TCP listener that a job reaches on its own loopback: the job's
// 127.0.0.1:port is carried over the job's ssh connection to dest:port on dev.
type forward struct {
	port int
	dest string // the loopback address on dev that answers: 127.0.0.1 or ::1
}

// socket is the unix socket sshd binds on the remote for this forward; the job's
// forwarder relays its loopback port to it. id is unique to the job.
func (f forward) socket(id string) string {
	return fmt.Sprintf("/tmp/offload-%s-%d.sock", id, f.port)
}

// sshSpec is the argument of `ssh -R` for this forward.
func (f forward) sshSpec(id string) string {
	dest := f.dest
	if strings.Contains(dest, ":") {
		dest = "[" + dest + "]"
	}
	return fmt.Sprintf("%s:%s:%d", f.socket(id), dest, f.port)
}

// parseListeners reads `ss -ltnH` output and returns one forward per port that answers
// on dev's loopback: listeners on 127.0.0.1, ::1, 0.0.0.0, * and [::]. A port that
// listens on IPv4 is dialled there, a port that listens only on IPv6 on ::1. Other
// addresses (a LAN address, a container bridge) do not answer on loopback and are
// skipped. The result is sorted by port.
func parseListeners(ss string) []forward {
	byPort := map[int]forward{}
	for _, line := range strings.Split(ss, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		i := strings.LastIndex(f[3], ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(f[3][i+1:])
		if err != nil {
			continue
		}
		var dest string
		switch f[3][:i] {
		case "127.0.0.1", "0.0.0.0", "*":
			dest = "127.0.0.1"
		case "[::1]", "[::]":
			dest = "::1"
		default:
			continue
		}
		if cur, ok := byPort[port]; !ok || dest == "127.0.0.1" && cur.dest != "127.0.0.1" {
			byPort[port] = forward{port: port, dest: dest}
		}
	}
	out := make([]forward, 0, len(byPort))
	for _, f := range byPort {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].port < out[j].port })
	return out
}
