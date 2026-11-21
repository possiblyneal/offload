package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runLocal runs the command here, in the caller's own terminal. The wrapper outlives
// every signal it catches, so it waits for the command and logs the run.
//
// With a terminal the command stays in the wrapper's process group, so it can read the
// terminal and gets the terminal's SIGINT, SIGHUP, SIGQUIT and SIGTSTP directly; the
// wrapper drops the first three (ignoring them would pass the ignore on through exec),
// stops itself on SIGTSTP so the shell sees the job stopped, and forwards only SIGTERM,
// SIGUSR1 and SIGUSR2, which no terminal sends (`timeout -s USR1` aims at the wrapper
// alone). Otherwise it leads a group of its own, as a remote job does,
// and the wrapper forwards SIGINT, SIGTERM, SIGHUP, SIGQUIT, SIGUSR1 and SIGUSR2 to it
// and suspends it on Ctrl-Z, so each signal arrives once, whether aimed at the wrapper or
// its group. A signal the caller ignored is never caught (keepInheritedIgnores).
//
// The command reads the caller's stdin only with --stdin (stdin) or on a terminal, which
// is what keeps an interactive command here; otherwise /dev/null, as a remote job reads
// EOF, so a `while read` loop around offload keeps its input. A wrapper killed outright
// takes the command with it (Pdeathsig), though not the command's own children.
func runLocal(argv []string, stdin bool) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = childStdio(1, os.Stdout), childStdio(2, os.Stderr)
	if stdinIsTerminal() || stdin {
		cmd.Stdin = childStdio(0, os.Stdin)
	}
	// With a terminal the command stays in the wrapper's group, as a plain local run would:
	// in a group of its own it is never the terminal's foreground, and a read of /dev/tty
	// (a passphrase prompt) would stop it for good, stdin redirected or not.
	tty := stdinIsTerminal() || hasControllingTerminal()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	sigs := make(chan os.Signal, 4)
	cmd.SysProcAttr.Setpgid = !tty
	stop := notifyUnignored(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTSTP)
	defer stop()
	// Pdeathsig fires when the thread that forked exits, not the process, and the Go
	// runtime may end an idle thread; locked here, the thread lives until Wait returns.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				switch {
				case tty && (s == syscall.SIGTERM || s == syscall.SIGUSR1 || s == syscall.SIGUSR2):
					// no terminal sends these; aimed at the wrapper, they are the command's
					cmd.Process.Signal(s)
				case tty:
					// the terminal sent it to the command too; the wrapper stops when the
					// command does (waitStopping), not on Ctrl-Z itself
				case s != syscall.SIGTSTP:
					syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
				default:
					suspendLocal(cmd.Process.Pid)
				}
			case <-done:
				return
			}
		}
	}()
	if tty {
		ws, err := waitStopping(cmd.Process.Pid)
		close(done)
		cmd.Process.Release()
		if err != nil {
			return 0, err
		}
		return statusCode(ws), nil
	}
	err := cmd.Wait()
	close(done)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return statusCode(ee.Sys().(syscall.WaitStatus)), nil
	}
	return 0, err
}

// statusCode is the shell's code for a command that ended with ws: its exit code, or
// 128+n for death by signal n, which also sets diedBy.
func statusCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		diedBy = ws.Signal()
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// waitStopping waits for the command to end, and stops the wrapper each time the command
// stops (Ctrl-Z, `kill -TSTP $$`, a background read of the terminal), so the shell sees
// the job stopped exactly when a plain run's would be; its `fg` continues both, and so
// does anything else that continues the command (stopWithCommand). A command that ignores
// Ctrl-Z runs on, and so does the wrapper.
func waitStopping(pid int) (syscall.WaitStatus, error) {
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		switch {
		case err == syscall.EINTR:
		case err != nil:
			return ws, err
		case ws.Stopped():
			stopWithCommand(pid)
		default:
			return ws, nil
		}
	}
}

// contWatchEnv, set to "<command pid> <wrapper pid>", makes the offload binary
// watchContinue instead of running.
const contWatchEnv = "OFFLOAD_CONT_WATCH"

// stopWithCommand stops the wrapper for as long as the command pid stays stopped. Stopped,
// the wrapper cannot see the command continue, so a watcher process (the binary again,
// under contWatchEnv) continues it once the command runs or ends, whoever continued it:
// `kill -CONT <pid>` from outside would otherwise leave the wrapper stopped for good. A
// stop already undone when it is read (`kill -STOP -<pgid>; kill -CONT -<pgid>`) stops
// nothing.
func stopWithCommand(pid int) {
	if procState(pid) != 'T' {
		return
	}
	w := exec.Command("/proc/self/exe")
	w.Env = append(os.Environ(), fmt.Sprintf("%s=%d %d", contWatchEnv, pid, os.Getpid()))
	w.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // out of the terminal's signals
	if err := w.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "offload: cannot watch the stopped command (%v); if it is continued other than by fg, continue offload too (kill -CONT %d)\n", err, os.Getpid())
		suspendSelf()
		return
	}
	suspendSelf()
	w.Process.Kill()
	w.Wait()
}

// watchContinue is the watcher stopWithCommand starts, given "<command pid> <wrapper pid>":
// while the wrapper is stopped it continues it once the command is no longer stopped. It
// ends then, or when the wrapper is gone; a wrapper that runs again kills it.
func watchContinue(v string) int {
	var cmd, wrapper int
	if _, err := fmt.Sscanf(v, "%d %d", &cmd, &wrapper); err != nil {
		return 2
	}
	for {
		switch procState(wrapper) {
		case 0:
			return 0
		case 'T':
			if procState(cmd) != 'T' {
				syscall.Kill(wrapper, syscall.SIGCONT)
				return 0
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// procState is the state letter of /proc/<pid>/stat (`T` stopped, `Z` a zombie), or 0
// when pid is gone.
func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) == 0 {
		return 0
	}
	return f[0][0]
}

// notifyUnignored relays to c each of sigs the caller has not ignored. Catching an ignored
// signal would undo the caller's ignore: `nohup offload -- cmd` would pass a hangup on to a
// command that, run plainly, inherits the ignore through exec and survives it. Until stop
// is called, a SIGQUIT is relayed rather than ending the wrapper (quitQuietly).
func notifyUnignored(c chan os.Signal, sigs ...os.Signal) (stop func()) {
	for _, s := range sigs {
		if !signal.Ignored(s) {
			signal.Notify(c, s)
		}
	}
	relays.Add(1)
	return func() {
		relays.Add(-1)
		signal.Stop(c)
	}
}

// hasControllingTerminal reports whether the wrapper has a controlling terminal, the one
// /dev/tty opens; without one, opening it fails with ENXIO.
func hasControllingTerminal() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// suspendLocal is Ctrl-Z for a local run in a group of its own: the group stops, then the
// wrapper does, and the group resumes once the shell continues the wrapper.
func suspendLocal(pgid int) {
	if groupOrphaned() {
		return
	}
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	syscall.Kill(-pgid, syscall.SIGSTOP)
	suspendSelf()
	<-cont
	syscall.Kill(-pgid, syscall.SIGCONT)
}

// groupOrphaned reports whether the wrapper's process group is orphaned: no member's
// parent is in another group of the same session, so no shell can continue it. The kernel
// discards a stop signal sent to such a group, so the wrapper drops a SIGTSTP it caught
// rather than stop for good. When /proc cannot be read it reports false. A var so a test
// does not depend on how its runner started it (a CI job runs in a session of its own).
var groupOrphaned = func() bool {
	pgrp := syscall.Getpgrp()
	_, sid, err := procGroup(os.Getpid())
	if err != nil {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, g, _, err := procStat(pid)
		if err != nil || g != pgrp {
			continue
		}
		if pg, ps, err := procGroup(ppid); err == nil && pg != pgrp && ps == sid {
			return false
		}
	}
	return true
}

// procGroup is the process group and session of pid.
func procGroup(pid int) (pgrp, sid int, err error) {
	_, pgrp, sid, err = procStat(pid)
	return pgrp, sid, err
}

// procStat is the parent, process group and session of pid, from /proc/<pid>/stat.
func procStat(pid int) (ppid, pgrp, sid int, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, 0, err
	}
	// The command name, in parentheses, may hold anything; the fields after it are
	// state, ppid, pgrp and session.
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 4 {
		return 0, 0, 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ppid, _ = strconv.Atoi(f[1])
	pgrp, _ = strconv.Atoi(f[2])
	sid, _ = strconv.Atoi(f[3])
	return ppid, pgrp, sid, nil
}
