package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// batchScope is the low-priority user scope every local run goes in; scripts/benchmark
// runs its "batch" local runs with the same properties.
var batchScope = []string{"systemd-run", "--user", "--scope", "--quiet", "--expand-environment=no", "-p", "CPUWeight=10", "--"}

// viaEnv starts every command the way the launcher starts a remote one, so a run reads
// the same wherever it lands: leading NAME=value words set variables, and a command that
// is missing exits 127 (126 when it cannot run) with env's message. It also ignores again
// every signal the wrapper inherited ignored, which the Go runtime's handlers (reset to
// the default at exec) would otherwise lose for the command, and blocks again each it
// inherited blocked.
func viaEnv() []string {
	run := []string{"/usr/bin/env"}
	if ign := inheritedIgnoreList(); ign != "" {
		run = append(run, "--ignore-signal="+ign)
	}
	if blk := inheritedBlockList(); blk != "" {
		run = append(run, "--block-signal="+blk)
	}
	return append(run, "--")
}

// assignment matches a word env and the shell both read as setting a variable.
var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandWords is argv as env must be given it. env reads every word holding `=` as
// NAME=value, so a command whose path holds one (`./dir=x/prog`) would set a variable and
// run its first argument instead; it is started by sh's exec, which keeps the caller's
// ignored and blocked signals. The leading NAME=value words stay assignments, as in a shell.
func commandWords(argv []string) []string {
	i := 0
	for i < len(argv) && assignment.MatchString(argv[i]) {
		i++
	}
	if i == len(argv) || !strings.Contains(argv[i], "=") {
		return argv
	}
	return slices.Concat(argv[:i], []string{"/bin/sh", "-c", `exec "$0" "$@"`}, argv[i:])
}

// runBatch runs the command here in the batch scope, so it never starves dev's
// interactive sessions. It returns the CPU seconds the command and its children used.
// Without a user systemd manager to make the scope (root under sudo, an account with no
// session) the command runs outside it, at normal priority, and stderr says so.
func runBatch(argv []string, stdin bool) (code int, cpuSeconds float64, err error) {
	runtimeDir, set := os.LookupEnv("XDG_RUNTIME_DIR")
	inner := viaEnv()
	var outer []string
	if !filepath.IsAbs(runtimeDir) {
		// Agent shells lack it and systemd-run --user needs it; the command, as in a plain
		// run, sees it as the caller had it: unset, or set to what systemd-run cannot use.
		if set {
			inner = append(inner, "XDG_RUNTIME_DIR="+runtimeDir)
		} else {
			inner = slices.Insert(inner, 1, "--unset=XDG_RUNTIME_DIR")
		}
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
		outer = []string{"/usr/bin/env", "XDG_RUNTIME_DIR=" + runtimeDir}
	}
	// The scope gives the command an INVOCATION_ID of its own; a plain run keeps the
	// caller's, or has none.
	if id, ok := os.LookupEnv("INVOCATION_ID"); ok {
		inner = append(inner, "INVOCATION_ID="+id)
	} else {
		inner = slices.Insert(inner, 1, "--unset=INVOCATION_ID")
	}
	run := slices.Concat(outer, batchScope, inner, commandWords(argv))
	// The manager's own socket, which `systemd-run --user` dials (not the D-Bus `bus`),
	// dialled rather than just found: one a dead manager left behind fails systemd-run with
	// exit 1 before the command runs.
	bus := filepath.Join(runtimeDir, "systemd", "private")
	if c, err := net.DialTimeout("unix", bus, time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "offload: no user systemd manager (%s: %v), so the command runs outside the batch scope, at normal priority\n", bus, err)
		run = slices.Concat(viaEnv(), commandWords(argv))
	} else {
		c.Close()
	}
	before := childrenCPU()
	code, err = runLocal(run, stdin)
	return code, childrenCPU() - before, err
}

// childrenCPU is the user and system CPU seconds of the children this process has waited for.
func childrenCPU() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru) != nil {
		return 0
	}
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}
