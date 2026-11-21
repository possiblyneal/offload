package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// A silly-rename file (.nfs plus 24 hex digits, the NFS client's own name) is what dev
// sees for a file a remote job deleted while holding it open. The remote's client removes
// it once the file is closed, but a job killed with files open can leave it behind. The
// wrapper sweeps those away on dev itself, where a walk of /tmp takes a fraction of a
// second; over NFS from the remote the same walk ran past any bound short enough to wait on.
//
// The sweep must never take a file another remote job still holds open, so every wrapper
// whose job may run on a remote holds the jobs lock shared until it exits, and the sweep
// runs only when it can take that lock exclusively at once. Every remote job on every
// remote has its wrapper here, so one lock covers them all. Its path is fixed for the
// machine: a caller's HOME or XDG_STATE_HOME must not give it a lock of its own.

// jobsLockPath is the jobs lock. /run/lock is world-writable and sticky, and a lock taken
// on a read-only descriptor works as well, so any user can create or share it.
const jobsLockPath = "/run/lock/offload-jobs.lock"

// sweepTimeout bounds the walk; one that runs out says so and leaves the rest for the next
// sweep. A var so a test can shorten it.
var sweepTimeout = 30 * time.Second

// sillyRename matches the client's silly-rename names for find's posix-extended -regex.
const sillyRename = `.*/\.nfs[0-9a-f]{24}`

// jobsLockWait bounds the wait for a shared hold. A sweep holds the lock exclusively for
// sweepTimeout at most, so a longer wait means the sweeper is stuck holding it. A var so a
// test can shorten it.
var jobsLockWait = sweepTimeout + 10*time.Second

// holdJobsLock opens the lock at path and takes it shared, waiting out a sweep for
// jobsLockWait at most and saying so on stderr while it waits. The lock lasts until the
// returned file is closed or the process exits.
func holdJobsLock(path string) (*os.File, error) {
	// Opened before it is created: in a sticky directory such as /run/lock, protected_regular
	// refuses an O_CREAT open of a file another account owns, even one that exists.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644); err == nil {
			// The creator's umask must not keep other accounts out.
			_ = f.Chmod(0o644)
		}
	}
	if err != nil {
		return nil, err
	}
	deadline, said := time.Now().Add(jobsLockWait), false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		switch {
		case err == nil:
			return f, nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, err
		case time.Now().After(deadline):
			f.Close()
			return nil, fmt.Errorf("another offload has held %s exclusively for over %v", path, jobsLockWait)
		case !said:
			fmt.Fprintln(os.Stderr, "offload: waiting for another offload to finish removing silly-rename files")
			said = true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sweepSillyRenames removes silly-rename files under dirs, each walked within its own
// filesystem, unless another job holds the jobs lock that lock is a shared hold on. It
// reports whether the sweep ran. It gives up the lock when the sweep ends, and nothing
// stops the wrapper meanwhile, so no other wrapper waits on it for longer than the walk.
func sweepSillyRenames(lock *os.File, dirs ...string) bool {
	// Caught and dropped while it runs, so an interrupt cannot cost the job's status and
	// Ctrl-Z cannot stop the wrapper while every other wrapper waits on its exclusive hold;
	// SIGPIPE too, so a message to a reader that has gone cannot end the wrapper before it
	// reports the job and logs the run.
	// Caught, not ignored: signal.Reset leaves an ignore in place, and the wrapper would
	// ignore them until it exited. One the caller ignored stays ignored.
	var stops []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGTSTP, syscall.SIGPIPE} {
		if !signal.Ignored(s) {
			stops = append(stops, s)
		}
	}
	drop := make(chan os.Signal, 1)
	signal.Notify(drop, stops...)
	defer signal.Stop(drop)
	// quitQuietly would otherwise end the wrapper by a SIGQUIT nothing else relays.
	relays.Add(1)
	defer relays.Add(-1)
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false
	}
	var roots []string
	for _, d := range dirs {
		if d != "" {
			roots = append(roots, d)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()
	args := append(slices.Clone(roots), "-xdev", "-type", "f", "-regextype", "posix-extended", "-regex", sillyRename, "-delete")
	find := exec.CommandContext(ctx, "find", args...)
	// A group of its own, so the terminal's Ctrl-C or Ctrl-Z reaches only the wrapper, which
	// drops it; find's own output (unreadable directories, which hold nothing this user
	// could remove) is discarded, since a background group writing to the terminal stops.
	find.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := find.Run()
	// Given up before saying anything: a write to a full pipe must not hold every other
	// wrapper off.
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		fmt.Fprintf(os.Stderr, "offload: removing silly-rename files from %v ran past %v; the rest are left for the next sweep\n", strings.Join(roots, " "), sweepTimeout)
	case errors.As(err, &exit) && exit.Sys().(syscall.WaitStatus).Signaled():
		fmt.Fprintf(os.Stderr, "offload: removing silly-rename files from %v was cut off by %v; the rest are left for the next sweep\n", strings.Join(roots, " "), exit.Sys().(syscall.WaitStatus).Signal())
	case err != nil && exit == nil:
		fmt.Fprintf(os.Stderr, "offload: could not remove silly-rename files from %v: %v\n", strings.Join(roots, " "), err)
	}
	return true
}
