package main

// The cases call run in this process with the mounts and the credential change replaced;
// the one that makes real mounts runs this test binary as offload-bind, in user and mount
// namespaces of its own, when TestMain sees bindEnv.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"devex/offload/internal/strerror"
)

const bindEnv = "OFFLOAD_BIND_UNDER_TEST"

func TestMain(m *testing.M) {
	probe := os.Getenv(bindEnv)
	if probe == "" {
		os.Exit(m.Run())
	}
	// setgroups is refused in an unprivileged user namespace.
	setCreds = func(int, int, []int) error { return nil }
	code := run(os.Args[1:], os.Stderr)
	got, _ := os.ReadFile(probe + "/f")
	fmt.Println(string(got))
	if err := os.WriteFile(probe+"/g", nil, 0o644); err != nil {
		fmt.Println(strerror.Of(errnoOf(err)))
	}
	os.Exit(code)
}

// A job root standing in for a mount of dev's filesystem.
func bindRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"a/b", "etc/target"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"a/rel":   "../etc", // relative
		"a/abs":   "/etc",   // absolute: dev's /etc, not this machine's
		"a/esc":   "../../../../../..",
		"a/up":    "/../../..",
		"a/chain": "rel/target",
		"l1":      "l2",
		"l2":      "l1",
		"a/self":  "self",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveInFollowsLinksAsIfTheJobRootWereSlash(t *testing.T) {
	root := bindRoot(t)
	cases := map[string]string{
		"/a/b":           "/a/b",
		"/a/./b/":        "/a/b",
		"/../../a":       "/a", // .. stops at the root
		"/a/b/../../..":  "/",
		"/a/rel/target":  "/etc/target",
		"/a/abs/target":  "/etc/target",
		"/a/esc":         "/", // a link climbing out stops at the root
		"/a/esc/etc":     "/etc",
		"/a/up/a/b":      "/a/b",
		"/a/chain":       "/etc/target",
		"/a/chain/../b":  "/etc/b", // .. after a link is taken from the link's target
		"/nope/x":        "/nope/x",
		"/a/b/nope/../x": "/a/b/x",
	}
	for path, want := range cases {
		if got, err := resolveIn(root, path); err != nil || got != want {
			t.Errorf("resolveIn(%q) = %q, %v, want %q", path, got, err, want)
		}
	}
}

func TestResolveInRefusesALinkLoop(t *testing.T) {
	root := bindRoot(t)
	for _, path := range []string{"/l1", "/l1/x", "/a/self"} {
		if got, err := resolveIn(root, path); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("resolveIn(%q) = %q, %v, want ELOOP", path, got, err)
		}
	}
}

// bindRun runs offload-bind on args with each mount replaced by one that records its
// source and destination and returns mount's result, the remount by one that records
// `read-only <dst>` and returns remount's, and the credential change by a no-op. It
// returns the exit code, the mounts made, one per line, and stderr.
func bindRun(t *testing.T, mount func(src string) syscall.Errno, remount syscall.Errno, args ...string) (int, string, string) {
	t.Helper()
	var mounts, stderr bytes.Buffer
	defer replace(&bindMount, func(src, dst string) syscall.Errno {
		fmt.Fprintln(&mounts, src, dst)
		return mount(src)
	})()
	defer replace(&makeReadOnly, func(dst string) syscall.Errno {
		fmt.Fprintln(&mounts, "read-only", dst)
		return remount
	})()
	defer replace(&setCreds, func(int, int, []int) error { return nil })()
	code := run(append([]string{"1000", "1000", "1000"}, args...), &stderr)
	return code, mounts.String(), stderr.String()
}

// replace sets *v to f and returns what puts the old one back.
func replace[T any](v *T, f T) func() {
	old := *v
	*v = f
	return func() { *v = old }
}

func mountsReturn(e syscall.Errno) func(string) syscall.Errno {
	return func(string) syscall.Errno { return e }
}

// The path is resolved inside the job root; --src binds SRC itself onto it, --src-root the
// same resolved path under another mount.
func TestBindBindsTheResolvedPathFromSrcOrUnderSrcRoot(t *testing.T) {
	root := bindRoot(t)
	for name, c := range map[string]struct{ mode, origin, want string }{
		"--src":      {"--src", "/store", "/store " + root + "/etc/target\n"},
		"--src-root": {"--src-root", "/rw", "/rw/etc/target " + root + "/etc/target\n"},
	} {
		code, out, errOut := bindRun(t, mountsReturn(0), 0, c.mode, c.origin, root, "/a/rel/target")
		if code != 0 || out != c.want {
			t.Errorf("%s: exit %d (%s), mounted %q, want %q", name, code, errOut, out, c.want)
		}
	}
}

// A failed mount exits 2 when the destination does not exist, 1 for any other error, and
// names both ends.
func TestBindExitCodeTellsAMissingDestinationFromOtherMountErrors(t *testing.T) {
	root := bindRoot(t)
	for mountErr, want := range map[syscall.Errno]int{syscall.ENOENT: 2, syscall.EPERM: 1, syscall.ESTALE: 1} {
		code, _, errOut := bindRun(t, mountsReturn(mountErr), 0, "--src-root", "/rw", root, "/a/b")
		if code != want || errOut != "offload-bind: /rw/a/b on "+root+"/a/b: "+strerror.Of(mountErr)+"\n" {
			t.Errorf("mount failing with %v: exit %d, stderr %q, want %d and a message", mountErr, code, errOut, want)
		}
	}
}

// The launcher hands every bind to one offload-bind: each is made in order with its own
// flags, and the first that fails stops the rest with its exit code.
func TestBindMakesEachBindInOrderAndStopsAtTheFirstFailure(t *testing.T) {
	root := bindRoot(t)
	binds := []string{
		"--skip-missing", "--src-root", "/rw", root, "/a/b",
		"--src-root", "/ro", root, "/a/rel/target",
		"--skip-missing", "--src", "/store", root, "/nope/x",
		"--skip-missing", "--read-only", "--src", "/cache", root, "/a/b",
	}
	code, out, errOut := bindRun(t, mountsReturn(0), 0, binds...)
	want := "/rw/a/b " + root + "/a/b\n/ro/etc/target " + root + "/etc/target\n" +
		"/cache " + root + "/a/b\nread-only " + root + "/a/b\n"
	if code != 0 || out != want {
		t.Errorf("exit %d (%s), mounted %q, want %q", code, errOut, out, want)
	}

	failRO := func(src string) syscall.Errno {
		if strings.HasPrefix(src, "/ro") {
			return syscall.ENOENT
		}
		return 0
	}
	code, out, errOut = bindRun(t, failRO, 0, binds...)
	want = "/rw/a/b " + root + "/a/b\n/ro/etc/target " + root + "/etc/target\n"
	if code != 2 || out != want || !strings.Contains(errOut, "offload-bind: /ro/etc/target on ") {
		t.Errorf("exit %d, mounted %q, stderr %q: want 2, the binds up to the failing one, and its message", code, out, errOut)
	}
}

// --skip-missing leaves a destination that is not there alone, and binds one that is.
func TestBindSkipMissingSkipsOnlyAnAbsentDestination(t *testing.T) {
	root := bindRoot(t)
	if err := os.WriteFile(filepath.Join(root, "etc", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/nope/x":       "",
		"/etc/file/x":   "", // under a file: cannot exist
		"/a/b":          "/rw/a/b " + root + "/a/b\n",
		"/a/abs/target": "/rw/etc/target " + root + "/etc/target\n",
	} {
		code, out, errOut := bindRun(t, mountsReturn(0), 0, "--skip-missing", "--src-root", "/rw", root, path)
		if code != 0 || out != want {
			t.Errorf("%s: exit %d (%s), mounted %q, want %q", path, code, errOut, out, want)
		}
	}
}

// A destination that cannot be checked may be a deny entry: skipping it would leave the
// path writable, so it fails loudly and binds nothing.
func TestBindSkipMissingFailsOnADestinationItCannotCheck(t *testing.T) {
	root := bindRoot(t)
	dst := root + "/a/b"
	real := lstat
	restore := replace(&lstat, func(p string) error {
		if p == dst {
			return &os.PathError{Op: "lstat", Path: p, Err: syscall.ESTALE}
		}
		return real(p)
	})
	code, out, errOut := bindRun(t, mountsReturn(0), 0, "--skip-missing", "--src-root", "/rw", root, "/a/b")
	restore()
	if code != 1 || out != "" || errOut != "offload-bind: "+dst+": Stale file handle; not bound\n" {
		t.Errorf("exit %d, mounted %q, stderr %q: want 1, no mount and a message naming %s", code, out, errOut, dst)
	}
	if os.Geteuid() == 0 {
		return // root walks a 0700 directory
	}
	locked := filepath.Join(root, "a", "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if code, out, errOut := bindRun(t, mountsReturn(0), 0, "--skip-missing", "--src-root", "/rw", root, "/a/locked/x"); code != 1 || out != "" || !strings.Contains(errOut, "Permission denied") {
		t.Errorf("under an unreadable directory: exit %d, mounted %q, stderr %q, want 1 and no mount", code, out, errOut)
	}
}

// --read-only remounts the bind read-only after binding it; a failed remount fails the
// bind loudly, so a path meant read-only is never left writable.
func TestBindReadOnlyRemountsTheBindAndFailsLoudlyWhenItCannot(t *testing.T) {
	root := bindRoot(t)
	for remountErr, want := range map[syscall.Errno]int{0: 0, syscall.EPERM: 1} {
		code, out, errOut := bindRun(t, mountsReturn(0), remountErr, "--skip-missing", "--read-only", "--src", "/store", root, "/a/b")
		wantOut := "/store " + root + "/a/b\nread-only " + root + "/a/b\n"
		if code != want || out != wantOut {
			t.Errorf("remount %v: exit %d (%s), mounts %q, want %d and %q", remountErr, code, errOut, out, want, wantOut)
		}
		if want != 0 && errOut != "offload-bind: "+root+"/a/b: read-only remount: Operation not permitted\n" {
			t.Errorf("remount %v: stderr %q, want a message", remountErr, errOut)
		}
	}
}

// A malformed command line binds nothing and fails.
func TestBindRefusesAMalformedCommandLine(t *testing.T) {
	root := bindRoot(t)
	for name, args := range map[string][]string{
		"too few":      {"1000", "1000"},
		"bad uid":      {"x", "1000", "1000", "--src", "/s", root, "/a/b"},
		"bad group":    {"1000", "1000", "1000,x", "--src", "/s", root, "/a/b"},
		"short bind":   {"1000", "1000", "", "--src", "/s", root},
		"no bind mode": {"1000", "1000", "", "--skip-missing", "/s", root, "/a/b", "/x"},
	} {
		var mounts int
		restore := replace(&bindMount, func(string, string) syscall.Errno { mounts++; return 0 })
		var stderr bytes.Buffer
		code := run(args, &stderr)
		restore()
		if code != 1 || mounts != 0 || !strings.HasPrefix(stderr.String(), "offload-bind: ") {
			t.Errorf("%s: exit %d, %d mounts, stderr %q, want 1, none and a message", name, code, mounts, stderr.String())
		}
	}
}

// The real mount calls: in a user and mount namespace of the test's own, a --read-only
// bind takes the source's files and refuses a write with EROFS.
func TestBindReadOnlyMakesWritesFailWithEROFS(t *testing.T) {
	if out, err := exec.Command("unshare", "-rm", "sh", "-c", `d=$(mktemp -d) && mount --bind "$d" "$d"; r=$?; rmdir "$d"; exit $r`).CombinedOutput(); err != nil {
		t.Skipf("needs `unshare -rm` able to bind-mount: %v %s", err, out)
	}
	root, store := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rm", "--propagation", "private", exe, "0", "0", "", "--read-only", "--src", store, root, "/dst")
	cmd.Env = append(os.Environ(), bindEnv+"="+root+"/dst")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "x\nRead-only file system\n" {
		t.Errorf("exit %v, output %q: want 0, the source's file, then EROFS on a write", err, out)
	}
}
