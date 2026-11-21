package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// gatherFacts observes what refusals depend on, and finds the paths inside the writable
// ones (the job's, as resolvePaths gave them) that the job must see read-only: databases
// another dev process has open, and mounts the export does not carry.
func gatherFacts(configDir, cwd, repo string, argv, writable []string) (refusalFacts, []string) {
	mounts, env := devMounts(), os.Environ()
	// remote-local paths are the account's whatever HOME says; a script's `~` is the job's HOME.
	home, shellHome := accountHome(), scriptHome(env)
	// The link walk takes seconds under a large tree, so a job the tmpfs rule already
	// keeps here, which no later rule can send away, skips it.
	tmpfs := tmpRefusal(mounts, cwd, env, argv)
	var links []string
	if tmpfs == "" {
		var linkProblem string
		links, linkProblem = jobLinks(cwd, repo)
		tmpfs = cmp.Or(linkProblem, cwdLinkRefusal(mounts, links))
	}
	f := refusalFacts{
		Tty:   stdinIsTerminal(),
		Sudo:  usesSudo(argv),
		Tmpfs: tmpfs,

		RemoteLocal: cmp.Or(remoteLocalRefusal(remoteLocalPaths(configDir, home), home, cwd, argv, links),
			storeCommandRefusal(configDir, argv)),
		Special:     specialFileRefusal(cwd, shellHome, argv, env),
		Fd:          inheritedFd(),
		ClosedStdio: closedStdioFacts(),
	}
	// The built-in rules hold without the file.
	var localOnly [][]string
	if lf, err := os.Open(filepath.Join(configDir, "local-only")); err == nil {
		localOnly = parseCommandList(lf)
		lf.Close()
	}
	f.WatchServe = watchServeMatch(localOnly, argv)
	var readOnly []string
	// NFS turns a remote job's flock into a POSIX lock on dev, which never conflicts with
	// a flock: a job would take a lock a dev process holds. /proc/locks may skip a lock
	// held throughout a read of it, so the holders' fdinfo, read whole each time, names
	// every lock an open file carries.
	locks := readLocks()
	held := parseLocks(locks)
	targets, unreadable := openFileTargets()
	devs := deviceIDs(writable)
	// A scan narrowed by a test build cannot tell a hidden holder from one it skipped.
	if fdGlob == allFds {
		hidden, unseen := hiddenHolders(targets, unreadable, locks, readLocks, devs)
		f.Unseen = unseen
		targets = append(targets, hidden...)
	}
	readerLocked := map[fileID]bool{}
	for _, t := range targets {
		held[t.id] = held[t.id] || t.flock
		readerLocked[t.id] = readerLocked[t.id] || t.excludesReaders
	}
	if mi, err := os.Open(selfMountinfo); err == nil {
		if f.Unseen == "" {
			f.Unseen = lockDeviceMismatch(mi, writable)
		}
		mi.Close()
	}
	// A holder that opened a file through a bind names the bind's path, not the one the job uses.
	onDevPaths(targets, devRootBinds())
	// A holder names a file by its real path, which a symlinked writable root does not prefix.
	seen := withRealPaths(writable)
	f.WAL, readOnly = walPlan(targets, seen, cwd, repo, shellHome, argv, env)
	f.WALBare = bareWAL(readOnly, readerLocked)
	var lockReadOnly []string
	f.Flock, lockReadOnly = flockPlan(targets, held, seen, cwd, repo, shellHome, argv, env)
	f.Foreign = foreignPlan(targets, held, devs, seen, hostPathsOf)
	readOnly = append(readOnly, lockReadOnly...)
	return f, append(readOnly, readOnlyMounts(slices.Concat(mounts, devReadOnlySelfBinds()), writable)...)
}

// onDevPaths rewrites each holder's path through dev's binds of its root filesystem to the
// path the bind shows (onRootFS). A foreign holder's path is its container's, which dev's
// binds say nothing about, so it is kept as the holder named it.
func onDevPaths(targets []openFile, binds [][2]string) {
	for i := range targets {
		if !targets[i].foreign {
			targets[i].path = onRootFS(binds, targets[i].path)
		}
	}
}

// stdinIsTerminal reports whether fd 0 is a terminal (TCGETS succeeds).
func stdinIsTerminal() bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// inheritedFd names the first descriptor above 2 the caller passed on, one open without
// close-on-exec, which a local run's command inherits (`<(…)`, `3>f`, a make jobserver),
// or "". Go opens its own descriptors close-on-exec.
func inheritedFd() string {
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 3 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(n), syscall.F_GETFD, 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
			continue
		}
		t, _ := os.Readlink("/proc/self/fd/" + e.Name())
		return fmt.Sprintf("fd %d (%s)", n, t)
	}
	return ""
}

// usesSudo reports whether the command runs sudo or another way of becoming root
// (findCommand), none of which works on a remote, where the job's user has no rights.
func usesSudo(w []string) bool {
	return findCommand(w, func(c []string) string {
		if base := filepath.Base(c[0]); becomesRoot(base) || base == "sudoedit" {
			return "sudo"
		}
		return ""
	}) != ""
}

// namesFile reports whether an argument or a variable names one of files: it mentions
// the first's absolute path anywhere (a script, `--db=`; a database's path is also the
// prefix of its -wal and -shm), or it is a path, read as pathCandidates reads one and a
// relative one from the cwd, a path in a script (scriptPaths), or any word of one
// (looseWords: `cd ../x && sqlite3 app.db`), `~` and `$HOME` read as home and pwdForm as
// the cwd, that is one of them or a directory holding the first (`--data-dir`,
// `cd dir && sqlite3 app.db`), judged as written and by its real path. `/` holds every
// file and names none. A word matched wrongly only keeps the job here; one missed sends it
// where the database is read-only. A variable names one only by mentioning its path:
// HOME, PWD and the XDG directories hold every database below them.
func namesFile(files []string, cwd, home string, argv, env []string) bool {
	names := func(p string) bool {
		for _, q := range []string{filepath.Clean(p), realPath(p)} {
			if q != "/" && (slices.Contains(files, q) || within([]string{q}, files[0]) != "") {
				return true
			}
		}
		return false
	}
	for _, e := range env {
		_, v, _ := strings.Cut(e, "=")
		if strings.Contains(v, files[0]) {
			return true
		}
		// DATABASE_URL=sqlite:///data/app.db names a path from the job's cwd.
		if u := urlPath(v); u != "" && names(filepath.Join(cwd, u)) || filepath.IsAbs(u) && names(u) {
			return true
		}
	}
	for _, a := range argv {
		if strings.Contains(a, files[0]) {
			return true
		}
		r := readArg(a, cwd, home)
		if slices.ContainsFunc(slices.Concat(r.candidates(), r.written, r.steps), names) {
			return true
		}
		bases := []string{cwd}
		if bareCd.MatchString(r.script) {
			bases = append(bases, home)
		}
		for _, w := range r.loose {
			for _, base := range bases {
				p := w
				if !filepath.IsAbs(p) {
					p = base + "/" + p
				}
				if names(p) {
					return true
				}
			}
		}
	}
	return false
}
