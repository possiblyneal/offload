package main

import (
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// launcherPath is where install.sh puts the launcher on every remote. A var so a
// test build can name a launcher of its own (-X main.launcherPath=).
var launcherPath = "/usr/local/libexec/offload-launch"

// job is everything the launcher needs to rebuild the caller's world remotely.
type job struct {
	devAddr string
	uid     int
	gid     int
	groups  []int
	repo    string // git toplevel of cwd, or "" when cwd is in no repository
	cwd     string
	argv    []string
	env     []string

	writable []string // bound read-write from dev
	readOnly []string // inside writable, bound read-only again

	forwards []forward // dev listeners carried to the job's loopback
	id       string    // unique to the job; names the forwards' sockets on the remote

	ctlID     string // names the control channel
	grace     int    // seconds from a forwarded signal to stopping the scope
	cpuQuota  string // CPUQuota of the job's scope; empty means the remote's default
	memoryMax string // MemoryMax of the job's scope; empty means the remote's default
	slots     int    // the remote's slot count; the launcher takes one for the job's life

	stdio stdioPlan // how the job's stdout and stderr reach the caller's (OFFLOAD_STDIO)

	umask   string // the caller's umask, in octal; the job runs with it
	nofile  string // the caller's soft RLIMIT_NOFILE; the job's, up to the remote's hard limit
	rlimits string // the caller's other soft limits, from callerRlimits; the job's, likewise
	ignore  string // the signals the caller left ignored, as env's --ignore-signal takes them
	block   string // the signals the caller left blocked, as env's --block-signal takes them
}

// shellQuote quotes s for the remote login shell that ssh runs the command in.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxCommandLine is the longest launcherCommand a job may have. ssh hands the remote
// command to the remote's shell as one argument, and Linux caps one argument at 128 KiB
// (MAX_ARG_STRLEN); the margin covers the dev listeners' socket list, added only as the
// job is sent.
const maxCommandLine = 120 * 1024

// commandTooLong is launcherCommand(j)'s length when it exceeds maxCommandLine, else 0.
func commandTooLong(j job) int {
	if n := len(launcherCommand(j)); n > maxCommandLine {
		return n
	}
	return 0
}

// launcherCommand is the remote command line that runs the job under the
// launcher. The environment travels on stdin (see envBlock), not here, because
// a command line is world-readable on the remote.
func launcherCommand(j job) string {
	groups := make([]string, len(j.groups))
	for i, g := range j.groups {
		groups[i] = strconv.Itoa(g)
	}
	socks := make([]string, len(j.forwards))
	for i, f := range j.forwards {
		socks[i] = f.socket(j.id)
	}
	parts := []string{
		// The forwarded ssh-agent's socket path is in the login shell's environment.
		"sudo -n --preserve-env=SSH_AUTH_SOCK " + launcherEnv(j) + launcherPath,
		shellQuote(j.devAddr),
		strconv.Itoa(j.uid),
		strconv.Itoa(j.gid),
		shellQuote(strings.Join(groups, ",")), // quoted: an empty list is still an argument
		shellQuote(j.cwd),
		shellQuote(strings.Join(j.writable, "\n")),
		shellQuote(strings.Join(j.readOnly, "\n")),
		shellQuote(strings.Join(socks, " ")),
		"--",
	}
	for _, a := range j.argv {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// envBlock encodes the caller's environment as NUL-terminated entries, ended by an
// empty one (no entry is empty), so the launcher stops reading at the end of the block
// and the caller's stdin, when forwarded, follows it. DISPLAY and WAYLAND_DISPLAY are
// left out: the remote has no display to open.
func envBlock(environ []string) []byte {
	var b []byte
	for _, e := range environ {
		// An entry with no `=` would be the command to the launcher's final `env`.
		if !strings.Contains(e, "=") || strings.HasPrefix(e, "DISPLAY=") || strings.HasPrefix(e, "WAYLAND_DISPLAY=") {
			continue
		}
		b = append(b, e...)
		b = append(b, 0)
	}
	return append(b, 0)
}

// launcherEnv is the `env NAME=value ... ` prefix that hands the launcher its control
// channel and scope limits; sudo resets the environment, so env sets them after it.
func launcherEnv(j job) string {
	vars := []string{"OFFLOAD_CTL=" + j.ctlID, "OFFLOAD_GRACE=" + strconv.Itoa(j.grace)}
	if j.slots > 0 {
		vars = append(vars, "OFFLOAD_SLOTS="+strconv.Itoa(j.slots))
	}
	if j.cpuQuota != "" {
		vars = append(vars, "OFFLOAD_CPU_QUOTA="+j.cpuQuota)
	}
	if j.memoryMax != "" {
		vars = append(vars, "OFFLOAD_MEMORY_MAX="+j.memoryMax)
	}
	if e := j.stdio.env(); e != "" {
		vars = append(vars, "OFFLOAD_STDIO="+e)
	}
	if j.umask != "" {
		vars = append(vars, "OFFLOAD_UMASK="+j.umask)
	}
	if j.nofile != "" {
		vars = append(vars, "OFFLOAD_NOFILE="+j.nofile)
	}
	if j.rlimits != "" {
		vars = append(vars, "OFFLOAD_RLIMITS="+j.rlimits)
	}
	if j.ignore != "" {
		vars = append(vars, "OFFLOAD_IGNORE="+j.ignore)
	}
	if j.block != "" {
		vars = append(vars, "OFFLOAD_BLOCK="+j.block)
	}
	env := "env "
	for _, v := range vars {
		env += shellQuote(v) + " "
	}
	return env
}

// callerLimits returns the umask and the soft RLIMIT_NOFILE this process was started
// with, as a shell prints them. They are read from a child: Go raises its own soft
// open-file limit to the hard one at startup and gives a child the original back.
func callerLimits() (umask, nofile string, err error) {
	out, err := exec.Command("/bin/sh", "-c", "umask; ulimit -Sn").Output()
	if err != nil {
		return "", "", fmt.Errorf("reading the umask and open-file limit: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return "", "", fmt.Errorf("reading the umask and open-file limit: got %q", out)
	}
	return f[0], f[1], nil
}

// rlimits are the resource limits a local job inherits besides RLIMIT_NOFILE, which
// callerLimits reads, each by prlimit's name for it.
var rlimits = []struct {
	name     string
	resource int
}{
	{"cpu", 0}, {"fsize", 1}, {"data", 2}, {"stack", 3}, {"core", 4}, {"rss", 5}, {"nproc", 6},
	{"memlock", 8}, {"as", 9}, {"locks", 10}, {"sigpending", 11}, {"msgqueue", 12},
	{"nice", 13}, {"rtprio", 14}, {"rttime", 15},
}

// callerRlimits is this process's soft limit on each of rlimits, as `name=value` words,
// value "unlimited" for none: the remote job gets them as a local one would inherit them.
// Go changes none of them at startup.
func callerRlimits() string {
	var words []string
	for _, r := range rlimits {
		var l syscall.Rlimit
		if err := syscall.Getrlimit(r.resource, &l); err != nil {
			continue
		}
		v := "unlimited" // past MaxInt64 no use reaches it, and the launcher's arithmetic would overflow
		if l.Cur <= math.MaxInt64 {
			v = strconv.FormatUint(l.Cur, 10)
		}
		words = append(words, r.name+"="+v)
	}
	return strings.Join(words, " ")
}

// controlCommand is the remote command of the second ssh session, which bridges the
// launcher's control channel to this process's pipes.
func controlCommand(id string) string {
	return "sudo -n " + launcherPath + " --control " + shellQuote(id)
}
