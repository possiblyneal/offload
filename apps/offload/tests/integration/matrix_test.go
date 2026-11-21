package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scenarios of the integration matrix that the other files do not cover: dev changing
// the repository under a running job, a database written from both sides, and a link that dies for good.

func TestGitGcOnDevDuringARemoteGitLogLeavesTheHistoryWhole(t *testing.T) {
	dir := repo(t)
	const commits, rounds = 1500, 100
	mustSh(t, false, fmt.Sprintf(`cd %s && tree=$(git hash-object -t tree -w /dev/null) && p= &&
for i in $(seq %d); do p=$(git -c user.name=t -c user.email=t@t commit-tree "$tree" ${p:+-p $p} -m "c$i"); done &&
git update-ref refs/heads/main "$p" && git symbolic-ref HEAD refs/heads/main`, shq(dir), commits))

	script := fmt.Sprintf(`touch up
for i in $(seq %d); do git log --format=%%H >log || exit 1; wc -l <log; done | sort | uniq -c
exit ${PIPESTATUS[0]}`, rounds)
	cmd, stdout, stderr := start(t, offloadBin, "server", dir, nil, "bash", "-c", script)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")

	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var err error
	gcs := 0
	for running := true; running; {
		select {
		case err = <-finished:
			running = false
		default:
			// Each pass deletes the packs and loose objects the job's git has open or has listed.
			mustSh(t, false, "cd "+shq(dir)+" && git gc --prune=now -q && git repack -adfq")
			gcs++
		}
	}
	t.Logf("%d gc passes while the job ran", gcs)
	if gcs == 0 {
		t.Fatal("the job ended before dev's first gc: the test overlapped nothing")
	}
	if exitOf(err) != 0 {
		t.Errorf("exit code %d, want 0; stderr %q", exitOf(err), stderr)
	}
	if want := fmt.Sprintf("%7d %d\n", rounds, commits); stdout.String() != want {
		t.Errorf("stdout %q, want %q (every git log shows all %d commits)", stdout, want, commits)
	}
}

func TestFileDevCreatesAfterARemoteLookupFailedIsFoundByTheSameJob(t *testing.T) {
	dir := repo(t)
	// The job misses `late`, says so with `up`, then looks again for up to 10 s. A negative
	// lookup that the mount cached would hide the file for as long as it is kept.
	script := `test -e late && exit 3; touch up
for i in $(seq 100); do test -e late && exit 0; sleep 0.1; done; exit 1`
	cmd, _, stderr := start(t, offloadBin, "server", dir, nil, "sh", "-c", script)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to miss the file")
	if err := os.WriteFile(filepath.Join(dir, "late"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); exitOf(err) != 0 {
		t.Errorf("exit code %d, want 0 (the job saw the file); stderr %q", exitOf(err), stderr)
	}
}

// onnxPython finds a Python on dev whose environment holds onnxruntime.
func onnxPython(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, depth := range []string{"*", "*/*"} {
		hits, _ := filepath.Glob(filepath.Join(home, "code", depth, ".venv/lib/python3*/site-packages/onnxruntime"))
		for _, hit := range hits {
			py := filepath.Join(hit, "../../../../bin/python")
			if _, err := os.Stat(py); err == nil {
				return filepath.Clean(py)
			}
		}
	}
	return ""
}

// onnxAdd builds the model by hand (no onnx package needed): z = x + y over three floats.
const onnxAdd = `import numpy as np
import onnxruntime as ort
def varint(n):
    out = b""
    while True:
        b = n & 0x7F
        n >>= 7
        out += bytes([b | (0x80 if n else 0)])
        if not n:
            return out
def num(n, v): return varint(n << 3) + varint(v)
def msg(n, body): return varint(n << 3 | 2) + varint(len(body)) + body
def text(n, s): return msg(n, s.encode())
def info(name):
    return text(1, name) + msg(2, msg(1, num(1, 1) + msg(2, msg(1, num(1, 3)))))
node = text(1, "x") + text(1, "y") + text(2, "z") + text(4, "Add")
graph = msg(1, node) + text(2, "g") + msg(11, info("x")) + msg(11, info("y")) + msg(12, info("z"))
model = num(1, 8) + msg(7, graph) + msg(8, num(2, 13))
sess = ort.InferenceSession(model, providers=["CPUExecutionProvider"])
print(sess.run(None, {"x": np.array([1, 2, 3], np.float32), "y": np.array([10, 20, 30], np.float32)})[0].tolist())`

func TestOnnxruntimeJobRunsOffloadedAsItDoesLocally(t *testing.T) {
	py := onnxPython(t)
	if py == "" {
		t.Skip("no Python environment under ~/code imports onnxruntime")
	}
	local, remote := both(t, py, "-c", onnxAdd)
	if local.code != 0 || local.stdout != "[11.0, 22.0, 33.0]\n" {
		t.Fatalf("local run: code %d, stdout %q, stderr %q", local.code, local.stdout, local.stderr)
	}
	same(t, local, remote)
}

const sqliteWriter = `import sqlite3, sys
c = sqlite3.connect(sys.argv[1], timeout=120)
for i in range(int(sys.argv[3])):
    c.execute("insert into t values (?, ?)", (sys.argv[2], i))
    c.commit()
c.close()`

func TestRollbackJournalDatabaseWrittenFromDevAndAJobStaysIntact(t *testing.T) {
	dir := repo(t)
	db := filepath.Join(dir, "db.sqlite")
	const n = 1000
	check := func(sql string) string {
		out, err := exec.Command("python3", "-c",
			`import sqlite3, sys; print(sqlite3.connect(sys.argv[1]).execute(sys.argv[2]).fetchone()[0])`, db, sql).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustSh(t, false, "python3 -c 'import sqlite3, sys; c = sqlite3.connect(sys.argv[1]); c.execute(\"create table t (who, i)\"); c.commit()' "+shq(db))
	if got := check("pragma journal_mode"); got != "delete" {
		t.Fatalf("journal_mode %q, want delete", got)
	}

	cmd, _, stderr := start(t, offloadBin, "server", dir, nil,
		"sh", "-c", fmt.Sprintf("touch up; exec python3 -c '%s' db.sqlite job %d", sqliteWriter, n))
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")

	var wg sync.WaitGroup
	var devOut []byte
	var devErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		devOut, devErr = exec.Command("python3", "-c", sqliteWriter, db, "dev", fmt.Sprint(n)).CombinedOutput()
	}()
	jobErr := cmd.Wait()
	wg.Wait()
	if devErr != nil {
		t.Errorf("dev's writer failed: %v\n%s", devErr, devOut)
	}
	if exitOf(jobErr) != 0 {
		t.Errorf("job's writer exit code %d; stderr %q", exitOf(jobErr), stderr)
	}
	if got := check("pragma integrity_check"); got != "ok" {
		t.Errorf("integrity_check = %q", got)
	}
	if got := check("select count(*) from t"); got != fmt.Sprint(2*n) {
		t.Errorf("%s rows, want %d", got, 2*n)
	}
}

func TestStoppingTheJobsScopeWithDevDownTakesUnder30s(t *testing.T) {
	const pattern = "[7]319marker"
	dir := repo(t)
	// The job reads dev's filesystem without pause, so the cut leaves it waiting on dev.
	cmd, _, _ := start(t, offloadBin, "server", dir, nil, "sh", "-c",
		`touch up; while :; do : 7319marker; find /usr -xdev -type f -exec cat {} + >/dev/null 2>&1; done`)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the job to start")
	time.Sleep(time.Second)
	unit := strings.TrimSpace(mustSh(t, true,
		`for p in $(pgrep -f '`+pattern+`'); do grep -o 'offload-job-[0-9]*\.scope' /proc/$p/cgroup; done | head -1`))
	if unit == "" {
		t.Fatal("the job's scope is not on the server")
	}

	result := fmt.Sprintf("/tmp/offload-it-stop-%d", time.Now().UnixNano())
	lift := "sudo iptables -D INPUT -s " + devAddr + " -j DROP; sudo iptables -D OUTPUT -d " + devAddr + " -j DROP"
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if out, _ := sh(t, true, lift+" 2>&1"); strings.Contains(out, "Bad rule") || strings.Contains(out, "does a matching rule exist") {
				break
			}
			time.Sleep(time.Second)
		}
		sh(t, true, "sudo systemctl kill --signal=SIGKILL "+unit+" 2>/dev/null; sudo systemctl start offload-cut.service; rm -f "+result)
	})
	// Runs on the server, since dev cannot reach it while the drop stands. One stop of
	// offload-cut.service and the scope is the shutdown's transaction for them.
	script := `
trap '` + lift + ` 2>/dev/null' EXIT
sleep 1
sudo iptables -I INPUT -s ` + devAddr + ` -j DROP
sudo iptables -I OUTPUT -d ` + devAddr + ` -j DROP
sleep 4 # the job is blocked on dev by now
t0=$(date +%s.%N)
timeout 60 sudo systemctl stop offload-cut.service ` + unit + `; rc=$?
t1=$(date +%s.%N)
{ echo "rc=$rc elapsed=$(echo "$t1 - $t0" | bc)"
  for p in $(sudo sh -c 'grep -l ` + unit + ` /proc/[0-9]*/cgroup 2>/dev/null' | cut -d/ -f3); do
    ps -o pid,stat,wchan:32,args -p $p --no-headers
  done; } >` + result + `.tmp && mv ` + result + `.tmp ` + result
	if err := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", serverAddr,
		"nohup setsid bash -c "+shq(script)+" >/dev/null 2>&1 </dev/null &").Run(); err != nil {
		t.Fatal(err)
	}
	var report string
	waitFor(t, func() bool {
		out, err := sh(t, true, "cat "+result)
		report = out
		return err == nil
	}, 120*time.Second, "the stop to be measured")
	lines := strings.Split(strings.TrimSpace(report), "\n")
	var rc int
	var secs float64
	if _, err := fmt.Sscanf(lines[0], "rc=%d elapsed=%f", &rc, &secs); err != nil {
		t.Fatalf("unreadable report %q", report)
	}
	t.Logf("stop took %.1f s; report:\n%s", secs, report)
	if rc != 0 || secs > 30 {
		t.Errorf("systemctl stop rc %d after %.1f s, want 0 within 30", rc, secs)
	}
	if len(lines) > 1 {
		t.Errorf("processes left in %s:\n%s", unit, strings.Join(lines[1:], "\n"))
	}
	waitOnServer(t, pattern, false, 10*time.Second)
	cmd.Process.Kill()
	cmd.Wait()
}

// A write dev refuses (here, to a file made immutable on dev between the job's write and
// its close) is reported by NFS only at writeback: at the close on exit, whose error no job
// reads. The launcher syncs dev's filesystem after the job, so the run fails and says
// where; a job that already failed keeps its own code.
func TestWriteDevRefusesAtWritebackFailsTheRunAndSaysSo(t *testing.T) {
	// One descriptor, never closed by the job: every close of any descriptor of the file
	// writes it back, as a shell's redirections would.
	const job = `import os, sys, time
fd = os.open("out", os.O_WRONLY | os.O_CREAT, 0o644)
os.write(fd, b"x" * 100000)
open("written", "w").close()
while not os.path.exists("frozen"):
    time.sleep(0.1)
os._exit(int(sys.argv[1]))`
	for _, c := range []struct {
		end  string
		want int
	}{{"0", 1}, {"3", 3}} {
		dir := repo(t)
		out := filepath.Join(dir, "out")
		t.Cleanup(func() { exec.Command("sudo", "chattr", "-i", out).Run() })
		cmd, _, stderr := start(t, offloadBin, "server", dir, nil, "python3", "-c", job, c.end)
		waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "written")); return err == nil }, 30*time.Second, "the job's write")
		mustSh(t, false, "sudo chattr +i "+shq(out)+" && touch "+shq(filepath.Join(dir, "frozen")))
		code := exitOf(cmd.Wait())
		if b, _ := os.ReadFile(out); len(b) != 0 {
			t.Fatalf("%s: %d bytes reached dev before the file was frozen: the probe missed; exit %d, stderr %q", c.end, len(b), code, stderr)
		}
		if code != c.want || !strings.Contains(stderr.String(), "offload: writing to dev failed under") {
			t.Errorf("%s: exit %d, stderr %q: want %d and a message naming the mount", c.end, code, stderr, c.want)
		}
	}
}

// A job's view of a file dev changed is not held stale by another job's cache: each job's
// mounts of dev have attribute caches of their own. The first job stats the unchanged file
// for half a minute, which stretches its cache's lifetime towards acregmax (60 s).
func TestJobSeesDevsChangeWhileAnotherJobHasTheFileCached(t *testing.T) {
	dir := repo(t)
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	start(t, offloadBin, "server", dir, nil, "sh", "-c", "touch up; while :; do stat -c %s f >/dev/null; sleep 1; done")
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "up")); return err == nil }, 30*time.Second, "the first job to start")
	time.Sleep(30 * time.Second)
	if err := os.WriteFile(f, []byte("xyzzy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := execIn(t, dir, true, "stat", "-c", "%s", "f"); r.code != 0 || r.stdout != "5\n" {
		t.Errorf("the second job saw size %q (exit %d, %s), want dev's 5", r.stdout, r.code, r.stderr)
	}
}
