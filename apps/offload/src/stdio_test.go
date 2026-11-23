package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// stdioOf runs readStdio in the test binary with its fds 1 and 2 set up by redirect, a
// shell redirection run from dir, and returns the plan's env() and the refusal.
func stdioOf(t *testing.T, dir, redirect string) (env, problem string) {
	t.Helper()
	report := filepath.Join(t.TempDir(), "report")
	cmd := exec.Command("sh", "-c", `exec "$0" `+redirect, os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_STDIO="+report)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v %s", redirect, err, out)
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	env, problem, _ = strings.Cut(string(b), "|")
	return env, problem
}

// readStdio sends a regular file to the remote job and relays everything else; it keeps a
// job here whose output a remote job could not write as a local run would.
func TestReadStdio(t *testing.T) {
	dir := t.TempDir()
	f, g := filepath.Join(dir, "f"), filepath.Join(dir, "g")
	if err := os.WriteFile(filepath.Join(dir, "ro"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, redirect, env, problem string
	}{
		{"pipes are relayed", "| cat", "", ""},
		{"stdout to a file", ">f", "out file w 0 " + f, ""},
		{"appended", ">>f", "out file a 0 " + f, ""},
		{"both to one file", ">f 2>&1", "err merged\nout file w 0 " + f, ""},
		{"stderr alone", "2>f | cat", "err file w 0 " + f, ""},
		{"two files", ">f 2>g", "out file w 0 " + f + "\nerr file w 0 " + g, ""},
		{"one file opened twice", ">f 2>f", "", "two openings of one file"},
		{"one file opened twice, once appending", ">>f 2>f", "", "two openings of one file"},
		{"one file appended to twice", ">>f 2>>f", "out file a 0 " + f + "\nerr file a 0 " + f, ""},
		{"read-only open", "1<ro", "", "opened read-only"},
		{"read-write open", "1<>f", "out file rw 0 " + f, ""},
		{"dev null is relayed", ">/dev/null", "", ""},
	} {
		os.Remove(f)
		os.Remove(g)
		env, problem := stdioOf(t, dir, c.redirect)
		if env != c.env || !strings.Contains(problem, c.problem) || (c.problem == "") != (problem == "") {
			t.Errorf("%s: env %q, problem %q; want %q and %q", c.name, env, problem, c.env, c.problem)
		}
	}
}

// A file whose open file description another process also holds is relayed, so every
// holder writes at its one offset as on a local run (`make -j CC='offload -- gcc' >log`).
func TestReadStdioRelaysAFileAnotherProcessHolds(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	report := filepath.Join(t.TempDir(), "report")
	cmd := exec.Command(os.Args[0])
	cmd.Stdout, cmd.Stderr = f, f // this process keeps f open too
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_STDIO="+report)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(report); string(b) != "err merged|" {
		t.Errorf("plan|problem %q, want both streams relayed as one", b)
	}
}

// A file whose path no longer names it, and one the caller may not open as it did, keep
// the job here: the remote job opens the file anew by its path, as the caller.
func TestReadStdioRefusesAFileTheJobCouldNotOpenByItsPath(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report")
	cmd := exec.Command("sh", "-c", `exec 1>f && rm f && exec "$0"`, os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_STDIO="+report)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(report); !strings.Contains(string(b), "no path on dev names any more") {
		t.Errorf("a removed stdout: %q", b)
	}
	if os.Geteuid() == 0 {
		return // root may write a 0400 file
	}
	if err := os.WriteFile(filepath.Join(dir, "locked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", "-c", `exec 1>>locked && chmod 400 locked && exec "$0"`, os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_STDIO="+report)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(report); !strings.Contains(string(b), "may not open for writing") {
		t.Errorf("a stdout the caller may no longer open: %q", b)
	}
	cmd = exec.Command("sh", "-c", `exec 1<>locked && chmod 200 locked && exec "$0"`, os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD_TEST_STDIO="+report)
	if err := os.Chmod(filepath.Join(dir, "locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(report); !strings.Contains(string(b), "may not open for reading and writing") {
		t.Errorf("a read-write stdout the caller may no longer read: %q", b)
	}
}

// outputRefusal keeps a job here whose output file the remote would not see as dev's.
func TestOutputRefusal(t *testing.T) {
	file := func(p string) []outputFile { return []outputFile{{stream: "out", fd: 1, path: p}} }
	for _, c := range []struct {
		path, want string
	}{
		{"/home/u/log", ""},
		{"/mnt/usb/log", "not exported"},
		{"/dev/shm/log", "private directory"},
		{"/sys/class/leds/x/brightness", "remote's own"},
		{"/proc/sys/vm/swappiness", "remote's own"},
		{"/home/u/.cache/go-build/log", "remote-local"},
		{"/home/u/ro/log", "read-only"},
	} {
		got := outputRefusal(file(c.path), []string{"/mnt/usb"}, []string{"/home/u/.cache/go-build"}, []string{"/home/u/ro"})
		if !strings.Contains(got, c.want) || (c.want == "") != (got == "") {
			t.Errorf("%s: %q, want %q", c.path, got, c.want)
		}
	}
}

// at reads each file's offset as the job is sent, leaving the plan it was called on alone.
func TestStdioPlanAtReadsTheCallersOffset(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("12345"); err != nil {
		t.Fatal(err)
	}
	p := stdioPlan{files: []outputFile{{stream: "out", fd: int(f.Fd()), path: f.Name()}}}
	q := p.at()
	if q.files[0].offset != 5 || p.files[0].offset != 0 {
		t.Errorf("at: offset %d (the original's %d), want 5 and 0", q.files[0].offset, p.files[0].offset)
	}
}

// settle leaves the caller's descriptor where a local run would: where the job left it, at
// the end when that is unknown and the job may have run, and untouched when it was never sent.
func TestStdioPlanSettle(t *testing.T) {
	for _, c := range []struct {
		name   string
		offset int64
		seen   bool
		ran    bool
		want   int64
	}{
		{"reported", 3, true, true, 3},
		{"unreported, may have run", 0, false, true, 10},
		{"never sent", 0, false, false, 1},
	} {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("0123456789")
		f.Seek(1, io.SeekStart)
		// The plan's fd is the caller's stdout, 1 here; settle is called with fd-1 indexes.
		p := stdioPlan{files: []outputFile{{stream: "out", fd: 1}}}
		saved, _ := syscall.Dup(1)
		syscall.Dup2(int(f.Fd()), 1)
		p.settle([2]int64{c.offset}, [2]bool{c.seen}, c.ran)
		got, _ := syscall.Seek(1, 0, io.SeekCurrent)
		syscall.Dup2(saved, 1)
		syscall.Close(saved)
		f.Close()
		if got != c.want {
			t.Errorf("%s: offset %d, want %d", c.name, got, c.want)
		}
	}
}

// heldOutput writes nothing until flushed, then everything, later writes too, to where
// flush was told: appended past what the job wrote, leaving the descriptor where the job
// left it, or at the caller's own offset.
func TestHeldOutput(t *testing.T) {
	for _, c := range []struct {
		appended bool
		want     string
	}{{true, "job output msg late"}, {false, "job msg late"}} {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString("job output")
		f.Seek(3, io.SeekStart)
		if err := (*heldOutput)(nil).flush(f); err != nil {
			t.Fatal(err)
		}
		h := &heldOutput{f: f}
		h.Write([]byte(" msg"))
		if b, _ := os.ReadFile(f.Name()); string(b) != "job output" {
			t.Fatalf("held write reached the file early: %q", b)
		}
		to := f
		if c.appended {
			if to, err = appendTo(int(f.Fd())); err != nil {
				t.Fatal(err)
			}
			defer to.Close()
		}
		if err := h.flush(to); err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(" late"))
		at, _ := f.Seek(0, io.SeekCurrent)
		if b, _ := os.ReadFile(f.Name()); string(b) != c.want || c.appended && at != 3 {
			t.Errorf("appended %v: file %q, offset %d; want %q and the job's offset kept", c.appended, b, at, c.want)
		}
	}
}

// sameOpenFile tells one open file description from two openings of one file.
func TestSameOpenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	a, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	d, err := syscall.Dup(int(a.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(d)
	if same, ok := sameOpenFile(int(a.Fd()), d); !same || !ok {
		t.Errorf("a dup: same %v, ok %v", same, ok)
	}
	if same, ok := sameOpenFile(int(a.Fd()), int(b.Fd())); same || !ok {
		t.Errorf("two openings: same %v, ok %v", same, ok)
	}
}

// followAppended moves the caller's descriptor past a message the wrapper appended, and
// leaves it where the job left it when the wrapper appended nothing.
func TestFollowAppended(t *testing.T) {
	for _, msg := range []string{" msg", ""} {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString("job output")
		f.Seek(3, io.SeekStart)
		a, err := appendTo(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		appended = nil
		trackAppended(int(f.Fd()), a)
		a.WriteString(msg)
		followAppended()
		want := int64(3)
		if msg != "" {
			want = int64(len("job output" + msg))
		}
		if at, _ := f.Seek(0, io.SeekCurrent); at != want {
			t.Errorf("message %q: offset %d, want %d", msg, at, want)
		}
	}
	appended = nil
}
