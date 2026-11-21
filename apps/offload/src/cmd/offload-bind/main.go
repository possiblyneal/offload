// Command offload-bind bind-mounts paths of dev's tree on the job root as the job's user,
// keeping root's capabilities.
//
//	usage: offload-bind UID GID GROUPS BIND [BIND...]
//	  BIND: [--skip-missing] [--read-only] (--src-root DIR | --src SRC) DST_ROOT PATH
//
// The binds are made in order, in one process (a process per bind cost the launch 50 ms
// each), and the first that fails stops the rest with its exit code.
//
// PATH is resolved inside DST_ROOT, which is a mount of dev's filesystem: a symlink in it,
// absolute or relative, is followed as if DST_ROOT were `/`, so it can never lead out of
// dev's tree onto this machine's disk, and `..` stops at the root. The resolved path is then
// bound from SRC (`--src`) or from the same path under DIR (`--src-root`, another mount of
// dev's filesystem) onto its place under DST_ROOT. A deny entry that is a symlink therefore
// covers its target, and a link loop is refused.
//
// Dev exports its filesystem with root_squash, and the home directory is 0700, so root
// cannot walk to a path under it: mount(8) fails with "Permission denied". Setting the
// filesystem uid and groups (not the real ones) makes the path lookups run as the user
// while CAP_SYS_ADMIN stays, which is all a bind mount needs. With --skip-missing a DST
// that does not exist is left alone, because a path under dev's read-only root cannot be
// created; one that cannot be checked (EACCES, ESTALE) fails with exit 1, since it may be a
// deny entry. A mount that fails exits 2 when DST does not exist (ENOENT), else 1.
// With --read-only the bind is then remounted read-only, so a write there fails with EROFS.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"devex/offload/internal/strerror"
)

const maxLinks = 40

// The steps a test replaces: each mount, the read-only remount, the check that a
// destination exists, and taking the job user's filesystem credentials.
var (
	bindMount    = realBindMount
	makeReadOnly = realMakeReadOnly
	lstat        = func(p string) error { var st syscall.Stat_t; return syscall.Lstat(p, &st) }
	setCreds     = realSetCreds
)

// The filesystem uid and groups are a thread's own, so every lookup and mount runs on the
// thread that took them: main's, locked to it before main starts.
func init() { runtime.LockOSThread() }

func main() {
	// The launcher ignores these so a signal forwarded to the job's group leaves it alive to
	// see $ctl/abort; Go would reset all but SIGHUP and SIGINT to fatal, so they are ignored
	// here again.
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT,
		syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2)
	os.Exit(run(os.Args[1:], os.Stderr))
}

// bind is one BIND of the command line.
type bind struct {
	skipMissing, readOnly bool
	mode, origin          string // mode is --src or --src-root
	dstRoot, path         string
}

func run(argv []string, stderr io.Writer) int {
	if len(argv) < 3 {
		fmt.Fprintln(stderr, "offload-bind: usage: offload-bind UID GID GROUPS BIND [BIND...]")
		return 1
	}
	uid, uidErr := strconv.Atoi(argv[0])
	gid, gidErr := strconv.Atoi(argv[1])
	var groups []int
	var groupErr error
	for g := range strings.SplitSeq(argv[2], ",") {
		if g == "" {
			continue
		}
		n, err := strconv.Atoi(g)
		groups, groupErr = append(groups, n), errors.Join(groupErr, err)
	}
	if err := errors.Join(uidErr, gidErr, groupErr); err != nil {
		fmt.Fprintf(stderr, "offload-bind: %v\n", err)
		return 1
	}
	binds, err := parseBinds(argv[3:])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := setCreds(uid, gid, groups); err != nil {
		fmt.Fprintf(stderr, "offload-bind: setgroups: %s\n", strerror.Of(err.(syscall.Errno)))
		return 1
	}
	for _, b := range binds {
		if code := bindOne(b, stderr); code != 0 {
			return code
		}
	}
	return 0
}

// parseBinds reads each BIND. A BIND's flags come before its mode, and its other words are
// absolute paths, so the binds need no separator.
func parseBinds(args []string) ([]bind, error) {
	var binds []bind
	for len(args) > 0 {
		var b bind
		for len(args) > 0 && (args[0] == "--skip-missing" || args[0] == "--read-only") {
			b.skipMissing = b.skipMissing || args[0] == "--skip-missing"
			b.readOnly = b.readOnly || args[0] == "--read-only"
			args = args[1:]
		}
		if len(args) < 4 || (args[0] != "--src-root" && args[0] != "--src") {
			return nil, errors.New("offload-bind: expected (--src-root DIR | --src SRC) DST_ROOT PATH")
		}
		b.mode, b.origin, b.dstRoot, b.path = args[0], args[1], args[2], args[3]
		binds, args = append(binds, b), args[4:]
	}
	return binds, nil
}

// bindOne is 0, or the exit code for the one bind's failure, named on stderr.
func bindOne(b bind, stderr io.Writer) int {
	resolved, err := resolveIn(b.dstRoot, b.path)
	if err != nil {
		fmt.Fprintf(stderr, "offload-bind: %s: %s\n", b.path, strerror.Of(errnoOf(err)))
		return 1
	}
	src := b.origin
	if b.mode == "--src-root" {
		src += resolved
	}
	dst := b.dstRoot + resolved
	if b.skipMissing {
		// Only a path known to be absent is skipped: one that cannot be checked (EACCES,
		// ESTALE) may be a deny entry, and skipping it would leave it writable.
		if err := lstat(dst); errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return 0
		} else if err != nil {
			fmt.Fprintf(stderr, "offload-bind: %s: %s; not bound\n", dst, strerror.Of(errnoOf(err)))
			return 1
		}
	}
	if e := bindMount(src, dst); e != 0 {
		fmt.Fprintf(stderr, "offload-bind: %s on %s: %s\n", src, dst, strerror.Of(e))
		if e == syscall.ENOENT {
			return 2
		}
		return 1
	}
	if b.readOnly {
		if e := makeReadOnly(dst); e != 0 {
			fmt.Fprintf(stderr, "offload-bind: %s: read-only remount: %s\n", dst, strerror.Of(e))
			return 1
		}
	}
	return 0
}

// resolveIn is the absolute path PATH names inside ROOT, symlinks followed, as `/` +
// components. Components that do not exist are kept as written; too many links is ELOOP.
func resolveIn(root, path string) (string, error) {
	pending := components(path)
	var done []string
	links := 0
	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		switch comp {
		case ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		target, err := os.Readlink(root + "/" + strings.Join(append(done, comp), "/"))
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			done = append(done, comp) // not a link, or nothing there: keep walking as written
			continue
		} else if err != nil {
			return "", err
		}
		if links++; links > maxLinks {
			return "", syscall.ELOOP
		}
		if strings.HasPrefix(target, "/") {
			done = nil
		}
		pending = append(components(target), pending...)
	}
	return "/" + strings.Join(done, "/"), nil
}

func components(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
}

// errnoOf is err's errno, or EIO for one that carries none.
func errnoOf(err error) syscall.Errno {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return syscall.EIO
}

// realSetCreds gives this thread the job user's groups and filesystem uid and gid, leaving
// its real and effective ids, and so CAP_SYS_ADMIN, as they are. A raw syscall, since
// syscall.Setgroups changes every thread's.
func realSetCreds(uid, gid int, groups []int) error {
	gids := make([]uint32, len(groups))
	for i, g := range groups {
		gids[i] = uint32(g)
	}
	var p unsafe.Pointer
	if len(gids) > 0 {
		p = unsafe.Pointer(&gids[0])
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_SETGROUPS, uintptr(len(gids)), uintptr(p), 0); e != 0 {
		return e
	}
	syscall.RawSyscall(syscall.SYS_SETFSGID, uintptr(gid), 0, 0)
	syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(uid), 0, 0)
	return nil
}

// realBindMount is 0, or the errno mount(2) reports for binding src onto dst.
func realBindMount(src, dst string) syscall.Errno {
	return errnoOrZero(syscall.Mount(src, dst, "", syscall.MS_BIND, ""))
}

// realMakeReadOnly is 0, or the errno mount(2) reports for remounting the bind on dst
// read-only. The bind's other flags are kept: a remount that drops one the kernel has
// locked (nosuid, nodev, noexec, the atime flags) is refused with EPERM.
func realMakeReadOnly(dst string) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dst, &st); err != nil {
		return errnoOf(err)
	}
	have := uintptr(st.Flags)
	// statfs's ST_ values match MS_ for these, but for relatime (ST 4096, MS 1 << 21).
	flags := syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | have&(2|4|8|1024|2048)
	if have&4096 != 0 {
		flags |= syscall.MS_RELATIME
	}
	return errnoOrZero(syscall.Mount("", dst, "", flags, ""))
}

func errnoOrZero(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	return errnoOf(err)
}
