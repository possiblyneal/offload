package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shell runs script under sh in dir with the given stdin (nil: none) and returns
// its stdout and exit code. $OFFLOAD is the test build of offload.
func shell(t *testing.T, dir, script string, stdin *strings.Reader) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD="+offloadBin)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %q: %v", script, err)
	}
	return stdout.String(), code
}

func TestWhileReadLoopKeepsItsInput(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "list"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := shell(t, dir, `while read f; do "$OFFLOAD" --where server -- echo "$f"; done < list`, nil)
	if code != 0 || out != "one\ntwo\nthree\n" {
		t.Errorf("code %d, stdout %q", code, out)
	}
}

func TestStdinFlagPassesAPipeToTheJob(t *testing.T) {
	out, code := shell(t, repo(t), `"$OFFLOAD" --where server --stdin -- cat`, strings.NewReader("a\nb\n"))
	if code != 0 || out != "a\nb\n" {
		t.Errorf("code %d, stdout %q", code, out)
	}
}

func TestStdinFlagPassesAFileToTheJob(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "in"), []byte("from a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := shell(t, dir, `"$OFFLOAD" --where server --stdin -- cat < in`, nil)
	if code != 0 || out != "from a file\n" {
		t.Errorf("code %d, stdout %q", code, out)
	}
}

func TestAPipeIsNotReadWithoutTheStdinFlag(t *testing.T) {
	out, code := shell(t, repo(t), `printf 'a\nb\n' | { "$OFFLOAD" --where server -- cat; echo "rest: $(cat)"; }`, nil)
	if code != 0 || out != "rest: a\nb\n" {
		t.Errorf("job read the caller's stdin, or the caller lost it: code %d, stdout %q", code, out)
	}
}

func TestStdinFlagOnATerminalIsRefused(t *testing.T) {
	// /dev/null is a character device, as a terminal is: neither a pipe nor a file.
	out, code := shell(t, repo(t), `"$OFFLOAD" --where server --stdin -- cat 2>&1 < /dev/null`, nil)
	if code != 2 || !strings.Contains(out, "not a pipe or a file") {
		t.Errorf("code %d, output %q", code, out)
	}
}

// alternate writes to stdout and stderr in turn, so any reordering shows.
const alternate = `i=0; while [ $i -lt 40 ]; do echo out$i; echo err$i >&2; i=$((i+1)); done`

func TestMergedOutputKeepsTheLocalOrder(t *testing.T) {
	dir := repo(t)
	local, code := shell(t, dir, `sh -c '`+alternate+`' 2>&1 | cat`, nil)
	if code != 0 || !strings.HasPrefix(local, "out0\nerr0\nout1\n") {
		t.Fatalf("local run: code %d, output %q", code, local)
	}
	for i := 0; i < 20; i++ {
		remote, code := shell(t, dir, `"$OFFLOAD" --where server -- sh -c '`+alternate+`' 2>&1 | cat`, nil)
		if code != 0 || remote != local {
			t.Fatalf("run %d: code %d, offloaded output differs from local\nlocal   %q\nremote  %q", i, code, local, remote)
		}
	}
}

func TestSeparateOutputsStaySeparate(t *testing.T) {
	dir := repo(t)
	cmd := exec.Command("sh", "-c", `"$OFFLOAD" --where server -- sh -c 'echo out; echo err >&2' 2>err.txt`)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OFFLOAD="+offloadBin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	errText, _ := os.ReadFile(filepath.Join(dir, "err.txt"))
	if string(out) != "out\n" || string(errText) != "err\n" {
		t.Errorf("stdout %q, stderr %q", out, errText)
	}
}

// A job whose stdout is a regular file it alone holds writes the file itself, as a local
// run's would: from the caller's offset, seeking it, appending, reopening it and reading it
// back. Each script runs once locally and once offloaded, the job exec'd from a subshell so
// no other process shares its descriptor, and the two files must match.
func TestAnOutputFileEndsAsALocalRunLeavesIt(t *testing.T) {
	dir := repo(t)
	here, _ := os.Hostname()
	seek := `python3 -c 'import os; os.write(1, b"12345"); os.lseek(1, 1, 0); os.write(1, b"Z"); os.write(2, b"e")'`
	for _, c := range []struct{ name, script string }{
		{"seek and write on", `( printf head; run ` + seek + ` ) >f 2>&1`},
		{"append", `printf old >f; ( run sh -c 'printf new; printf err >&2' ) >>f 2>&1`},
		{"two appends of one file", `printf old >f; ( run sh -c 'printf new; printf err >&2' ) >>f 2>>f`},
		{"reopen /dev/stdout", `( printf head; run sh -c 'printf a; printf b >>/dev/stdout' ) >f`},
		{"read back", `( run sh -c 'printf written; cat f >&2' 2>g ) >f; cat g >>f`},
		{"read-write", `printf abcdefgh >f; ( run sh -c 'exec 3>&1; printf XY; v=$(head -c 3 <&3); printf "$v"' ) 1<>f`},
		{"stderr alone", `( run sh -c 'printf out; printf err >&2' 2>f ) | cat >g; cat g >>f`},
	} {
		var got [2]string
		for i, run := range []string{`run() { exec "$@"; }`, `run() { exec "$OFFLOAD" --where server -- sh -c 'hostname >"$0"; exec "$@"' "$PWD/host" "$@"; }`} {
			os.Remove(filepath.Join(dir, "f"))
			os.Remove(filepath.Join(dir, "g"))
			os.Remove(filepath.Join(dir, "host"))
			if _, code := shell(t, dir, run+"\n"+c.script, nil); code != 0 {
				t.Fatalf("%s: exit %d (run %q)", c.name, code, run)
			}
			b, _ := os.ReadFile(filepath.Join(dir, "f"))
			got[i] = string(b)
			if host, _ := os.ReadFile(filepath.Join(dir, "host")); i == 1 && (len(host) == 0 || string(host) == here+"\n") {
				t.Errorf("%s: the offloaded job ran on %q", c.name, host)
			}
		}
		if got[0] != got[1] {
			t.Errorf("%s: local file %q, offloaded %q", c.name, got[0], got[1])
		}
	}
}

// An output file the remote job could not write as dev's keeps the job here, saying why.
// exec'd, the wrapper alone holds the file, so it is refused rather than relayed.
func TestAnOutputFileTheRemoteCannotReachRunsLocally(t *testing.T) {
	dir := repo(t)
	here, _ := os.Hostname()
	out, code := shell(t, dir, `f=/dev/shm/offload-out-$$; (exec "$OFFLOAD" --where server -- hostname >$f 2>err); cat $f err; rm -f $f`, nil)
	if code != 0 || !strings.HasPrefix(out, here+"\n") || !strings.Contains(out, "running locally: the job's output file") {
		t.Errorf("exit %d, output %q", code, out)
	}
}

// Stdout and stderr opened separately on one file without appending overwrite each other
// at their own offsets, which two remote writers cannot reproduce: the job stays here.
func TestTwoOpensOfOneOutputFileRunLocally(t *testing.T) {
	dir := repo(t)
	here, _ := os.Hostname()
	out, code := shell(t, dir, `"$OFFLOAD" --where server -- sh -c 'hostname; printf e >&2' >f 2>f; cat f`, nil)
	// fd 2 writes offload's refusal from offset 0, fd 1 the job's hostname over its start.
	if code != 0 || !strings.HasPrefix(out, here+"\n") || !strings.Contains(out, "running locally: stdout and stderr are two openings of one file") {
		t.Errorf("exit %d, file %q", code, out)
	}
}

// The caller's file-size limit holds for the remote job as for a local one: the write past
// it dies of SIGXFSZ, leaving the file at the limit, whether the job writes the file itself
// (exec'd, it alone holds it) or the wrapper relays it (dash holds a redirect it forks for).
func TestTheCallersFileSizeLimitHolds(t *testing.T) {
	dir := repo(t)
	for _, form := range []string{`(exec %s sh -c 'head -c 9999 /dev/zero; echo past') >f`, `%s sh -c 'head -c 9999 /dev/zero; echo past' >f`} {
		script := `ulimit -f 2; ` + form + `; echo "$?"; wc -c <f`
		local, _ := shell(t, dir, fmt.Sprintf(script, ""), nil)
		remote, _ := shell(t, dir, fmt.Sprintf(script, `"$OFFLOAD" --where server --`), nil)
		if local != remote {
			t.Errorf("%s: local %q, offloaded %q", form, local, remote)
		}
	}
}

// The caller's other resource limits hold for the remote job as for a local one, each
// within both machines' hard limits, and a soft limit raised to an unlimited hard one too.
func TestTheCallersResourceLimitsHold(t *testing.T) {
	dir := repo(t)
	for _, set := range []string{`ulimit -t 100; ulimit -v 4000000; ulimit -d 3000000; ulimit -s 4096; ulimit -l 4096`, `ulimit -s unlimited`} {
		script := set + `; %s sh -c 'ulimit -t; ulimit -v; ulimit -d; ulimit -s; ulimit -l'`
		local, _ := shell(t, dir, fmt.Sprintf(script, ""), nil)
		remote, code := shell(t, dir, fmt.Sprintf(script, `"$OFFLOAD" --where server --`), nil)
		if code != 0 || local != remote {
			t.Errorf("%s: local %q, offloaded %q (exit %d)", set, local, remote, code)
		}
	}
}

// An output file shows the job's output on dev while the job runs, as a local run's does
// (`tail -f build.log`), not only once it ends.
func TestAnOutputFileShowsTheJobsOutputWhileItRuns(t *testing.T) {
	dir := repo(t)
	f := filepath.Join(dir, "f")
	// exec'd, the wrapper alone holds f, so the remote job writes it itself.
	cmd := exec.Command("sh", "-c", `exec "$0" --where server -- sh -c 'echo early; sleep 20; echo late' >f`, offloadBin)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
	waitFor(t, func() bool {
		b, _ := os.ReadFile(f)
		return string(b) == "early\n"
	}, 10*time.Second, "the job's first line on dev")
}

// Jobs sharing one open file description of their output file (`make -j CC='offload -- gcc'
// >log`) write at its one offset, as local runs would, rather than over each other.
func TestJobsSharingAnOutputFileDoNotOverwriteEachOther(t *testing.T) {
	dir := repo(t)
	out, code := shell(t, dir, `{ "$OFFLOAD" --where server -- sh -c 'sleep 1; echo aaaa' & "$OFFLOAD" --where server -- sh -c 'sleep 1; echo bbbb' & wait; } >f; sort f`, nil)
	if code != 0 || out != "aaaa\nbbbb\n" {
		t.Errorf("exit %d, file %q", code, out)
	}
}
