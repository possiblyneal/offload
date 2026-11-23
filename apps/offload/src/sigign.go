package main

// The Go runtime replaces an inherited SIG_IGN with its own handler for every signal but
// SIGHUP and SIGINT before main runs, and exec resets a handled signal to its default, so a
// command offload starts would lose an ignore its caller set (bash ignores SIGQUIT for a
// script's background job; `trap '' TERM`). The runtime also unblocks SIGINT, SIGTERM and
// the other signals that would end it, and each it is asked to catch, in every thread, so a
// command it starts would not inherit them blocked. The wrapper still catches a signal the
// caller blocked and passes it on: the command, blocking it, leaves it pending. This
// constructor runs before the runtime starts and records which signals the process
// inherited ignored, and which blocked. It also records which of stdin, stdout and stderr
// the caller passed closed: the runtime opens /dev/null on each, and a command offload
// starts would write there instead of failing with EBADF (`yes >&-` would never end).

/*
#include <pthread.h>
#include <signal.h>
#include <fcntl.h>
#include <sys/prctl.h>
#include <sys/resource.h>
static unsigned long long inherited_ignores, inherited_blocks;
static int closed_stdio;
__attribute__((constructor)) static void record_inherited_ignores(void) {
	for (int fd = 0; fd < 3; fd++) {
		if (fcntl(fd, F_GETFD) == -1) closed_stdio |= 1 << fd;
	}
	sigset_t blocked;
	int masked = pthread_sigmask(SIG_BLOCK, 0, &blocked) == 0;
	for (int s = 1; s < 32; s++) {
		struct sigaction a;
		if (sigaction(s, 0, &a) == 0 && a.sa_handler == SIG_IGN) inherited_ignores |= 1ULL << (s - 1);
		if (masked && sigismember(&blocked, s) == 1) inherited_blocks |= 1ULL << (s - 1);
	}
}
static unsigned long long inheritedIgnores(void) { return inherited_ignores; }
static unsigned long long inheritedBlocks(void) { return inherited_blocks; }
static int closedStdio(void) { return closed_stdio; }
// die_by ends the process by signal s, with no core file. Set here, the default action
// bypasses the Go runtime's handler, which would print a stack trace for SIGQUIT.
static void die_by(int s) {
	struct rlimit none = {0, 0};
	setrlimit(RLIMIT_CORE, &none);
	// Piped core handlers ignore RLIMIT_CORE.
	prctl(PR_SET_DUMPABLE, 0L, 0L, 0L, 0L);
	struct sigaction a = {0};
	a.sa_handler = SIG_DFL;
	sigaction(s, &a, 0);
	sigset_t m;
	sigemptyset(&m);
	sigaddset(&m, s);
	pthread_sigmask(SIG_UNBLOCK, &m, 0);
	raise(s);
}
*/
import "C"

import (
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// keptIgnores are the signals whose inherited ignore keepInheritedIgnores restores. SIGCHLD
// is left out (ignored, the wrapper could not wait for its command), as are the runtime's
// own (SIGURG, SIGPROF).
var keptIgnores = []syscall.Signal{
	syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGTSTP,
	syscall.SIGTTIN, syscall.SIGTTOU, syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2,
	syscall.SIGALRM,
}

// keepInheritedIgnores ignores again each of keptIgnores the process inherited ignored, so
// the wrapper does not catch it (notifyUnignored) and a command it starts inherits the
// ignore through exec, as it would run plainly.
func keepInheritedIgnores() {
	ign := uint64(C.inheritedIgnores())
	for _, s := range keptIgnores {
		if ign&(1<<(uint(s)-1)) != 0 {
			signal.Ignore(s)
		}
	}
}

// stdioClosed reports whether the caller passed descriptor fd (0, 1 or 2) closed.
func stdioClosed(fd int) bool { return C.closedStdio()&(1<<fd) != 0 }

// childStdio is f for a command's descriptor fd, or, when the caller passed fd closed, a
// nil *os.File: exec.Cmd hands a nil *os.File to the child as descriptor -1, which closes
// fd there, so the command sees it closed as it would run plainly.
func childStdio(fd int, f *os.File) *os.File {
	if stdioClosed(fd) {
		return nil
	}
	return f
}

// closedStdioFacts names the first of stdin, stdout and stderr the caller passed closed,
// or "": a remote job's would be ssh's pipes.
func closedStdioFacts() string {
	for fd, name := range []string{"stdin", "stdout", "stderr"} {
		if stdioClosed(fd) {
			return name
		}
	}
	return ""
}

// relays counts the notifyUnignored registrations in force, the times something relays
// SIGQUIT on to a command.
var relays atomic.Int32

// quitQuietly catches SIGQUIT for the wrapper's whole life, unless the caller ignored it,
// and ends the wrapper by it whenever nothing relays it (relays): the Go runtime's own
// handler would print every goroutine's stack and exit 2, where a plain command dies by
// the signal, as the wrapper does by every other signal it has not caught yet.
func quitQuietly() {
	if signal.Ignored(syscall.SIGQUIT) {
		return
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGQUIT)
	go func() {
		for range c {
			if relays.Load() == 0 {
				dieBy(syscall.SIGQUIT)
			}
		}
	}()
}

// dieBy ends the wrapper by signal s, as its command ended, without a core file. It
// returns only for a signal whose default action is not to terminate.
func dieBy(s syscall.Signal) { C.die_by(C.int(s)) }

// inheritedIgnore reports whether the process inherited s ignored.
func inheritedIgnore(s syscall.Signal) bool {
	return uint64(C.inheritedIgnores())&(1<<(uint(s)-1)) != 0
}

// inheritedIgnoreList is the signals the process inherited ignored, as numbers joined by
// commas for env's --ignore-signal, or "" for none.
func inheritedIgnoreList() string { return signalList(uint64(C.inheritedIgnores())) }

// inheritedBlockList is the signals the process inherited blocked, as numbers joined by
// commas for env's --block-signal, or "" for none.
func inheritedBlockList() string { return signalList(uint64(C.inheritedBlocks())) }

// signalList is the signals set in mask (bit n-1 for signal n), as numbers joined by commas.
func signalList(mask uint64) string {
	var nums []string
	for s := 1; s < 32; s++ {
		if mask&(1<<(s-1)) != 0 {
			nums = append(nums, strconv.Itoa(s))
		}
	}
	return strings.Join(nums, ",")
}
