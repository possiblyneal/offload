package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// autoEnv is the environment of a caller with no display, so nothing but the rule under
// test can keep a job local.
var autoEnv = []string{"DISPLAY=", "WAYLAND_DISPLAY="}

// scopeOf is a job's cgroup and, when it runs in a user scope here, that scope's CPUWeight.
// scopeProbe names the host and the job's scope with its CPUWeight. A local run gives the
// command no XDG_RUNTIME_DIR it lacked, so the probe asks logind for it: spelled out, the
// /run path would keep every probe local.
const scopeProbe = `hostname; c=$(cut -d: -f3 /proc/self/cgroup); echo "$c"; ` +
	`XDG_RUNTIME_DIR=$(loginctl show-user "$(id -u)" -p RuntimePath --value) systemctl --user show -p CPUWeight "$(basename "$c")" 2>/dev/null || true`

func serverHostname(t *testing.T) string {
	return strings.TrimSpace(mustSh(t, true, "hostname"))
}

// runOffload runs bin with args in dir and returns exit code, stdout and stderr.
func runOffload(t *testing.T, bin, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	return exitOf(cmd.Run()), stdout.String(), stderr.String()
}

// freeSlots is how many of the server's slots are free, read from the launcher.
func freeSlots(t *testing.T, of int) string {
	return strings.TrimSpace(mustSh(t, true, "sudo -n /usr/local/libexec/offload-launch --slots "+strconv.Itoa(of)))
}

func TestRefusalsKeepTheJobLocalAndSayWhy(t *testing.T) {
	here, _ := os.Hostname()
	cases := []struct {
		name string
		env  []string
		argv []string
		why  string
	}{
		{"sudo", autoEnv, []string{"sudo", "-n", "true"}, "sudo"},
		{"sudo in a shell string", autoEnv, []string{"sh", "-c", "true && sudo -n true"}, "sudo"},
		{"xvfb-run", autoEnv, []string{"xvfb-run", "true"}, "local-only"},
		{"orca", autoEnv, []string{"orca", "--help"}, "local-only"},
		{"display and local-only", []string{"DISPLAY=:0"}, []string{"orca", "--help"}, "local-only"},
		{"local-only in a shell string", autoEnv, []string{"sh", "-c", "true && orca --help"}, "local-only"},
		{"local-only behind env and nice", autoEnv, []string{"env", "A=1", "nice", "xvfb-run", "true"}, "local-only"},
		{"sudo behind ionice", autoEnv, []string{"ionice", "-c3", "sudo", "-n", "true"}, "sudo"},
		{"sudo behind stdbuf and setsid", autoEnv, []string{"stdbuf", "-o", "L", "setsid", "sudo", "-n", "true"}, "sudo"},
		{"an argument holding an unexported mount", autoEnv, []string{"ls", "/boot"}, "holds"},
		{"an option value holding an unexported mount", autoEnv, []string{"tar", "--directory=/boot", "-cf", "/dev/null", "."}, "holds"},
		{"a script entering an unexported mount's parent", autoEnv, []string{"sh", "-c", "cd /boot && ls"}, "holds"},
		{"a package manager's dev script", autoEnv, []string{"yarn", "dev"}, "local-only"},
		{"a listed program behind npx", autoEnv, []string{"npx", "--no-install", "vite", "--version"}, "local-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stdout, stderr := runOffload(t, offloadBin, repo(t), c.env, append([]string{"--"}, c.argv...)...)
			if !strings.Contains(stderr, "offload: running locally: ") || !strings.Contains(stderr, c.why) {
				t.Errorf("stderr %q does not say why it stayed local (%s)", stderr, c.why)
			}
			if c.argv[0] == "hostname" && stdout != here+"\n" {
				t.Errorf("ran on %q, want here %q", stdout, here)
			}
		})
	}
}

// A command line longer than ssh carries runs here whole, even under --where server,
// rather than failing on the remote with "argument list too long".
func TestACommandLineTooLongForSSHRunsLocally(t *testing.T) {
	big := strings.Repeat("x", 40*1024)
	here, _ := os.Hostname()
	code, stdout, stderr := runOffload(t, offloadBin, repo(t), autoEnv,
		"--where", "server", "--", "sh", "-c", `hostname; echo "$#"`, "_", big, big, big, big)
	if code != 0 || stdout != here+"\n4\n" || !strings.Contains(stderr, "more than ssh carries") {
		t.Errorf("code %d, stdout %q, stderr %q: want a local run with all four arguments that says why", code, stdout, stderr)
	}
}

func TestDisplayDoesNotKeepAJobLocalAndRemoteJobsSeeNone(t *testing.T) {
	env := []string{"DISPLAY=:99", "WAYLAND_DISPLAY=wayland-1"}
	_, stdout, _ := runOffload(t, offloadBin, repo(t), env, "--", "sh", "-c", "hostname; echo \"[$DISPLAY][$WAYLAND_DISPLAY]\"")
	if want := serverHostname(t) + "\n[][]\n"; stdout != want {
		t.Errorf("got %q, want %q: ran off the server or saw a display", stdout, want)
	}
	_, _, stderr := runOffload(t, offloadBin, repo(t), env, "--explain", "--", "hostname")
	if !strings.Contains(stderr, "placement server by free-slot") || strings.Contains(stderr, "refusal display") {
		t.Errorf("--explain with DISPLAY set:\n%s", stderr)
	}
}

func TestInteractiveTerminalStaysLocalAndSaysWhy(t *testing.T) {
	// script(1) gives the child a terminal for fd 0.
	script := shq(offloadBin) + " -- bash -c hostname"
	cmd := exec.Command("script", "-qec", script, "/dev/null")
	cmd.Dir = repo(t)
	cmd.Env = append(os.Environ(), autoEnv...)
	out, _ := cmd.CombinedOutput()
	here, _ := os.Hostname()
	if !strings.Contains(string(out), "running locally: stdin is an interactive terminal") || !strings.Contains(string(out), here) {
		t.Errorf("output %q: want local, with the reason", out)
	}
}

func TestExplainPrintsSlotsPathClassesAndTheDecidingRule(t *testing.T) {
	_, stdout, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--explain", "--", "hostname")
	for _, want := range []string{"offload: slots server 2 of 2 free", "offload: writable ", "offload: placement server by free-slot"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("--explain lacks %q:\n%s", want, stderr)
		}
	}
	if got := strings.TrimSpace(stdout); got != serverHostname(t) {
		t.Errorf("ran on %q, want the server", got)
	}
	_, _, stderr = runOffload(t, offloadBin, repo(t), autoEnv, "--explain", "--", "sudo", "-n", "true")
	if !strings.Contains(stderr, "offload: placement local by sudo") {
		t.Errorf("--explain lacks the deciding rule:\n%s", stderr)
	}
}

// holdWAL starts a dev process that keeps the WAL database db open, holding one row (42),
// until the test ends, and returns an offload build that looks at its open files only.
func holdWAL(t *testing.T, db string) (bin string) {
	t.Helper()
	holder := exec.Command("python3", "-I", "-c", `
import sqlite3, sys, time
c = sqlite3.connect(sys.argv[1])
c.execute("pragma journal_mode=wal")
c.execute("create table t(x)")
c.execute("insert into t values (42)")
c.commit()
print("ready", flush=True)
time.sleep(120)
`, db)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "ready" {
		t.Fatalf("holder: %q %v", buf, err)
	}
	return binWatching(t, holder.Process.Pid)
}

// binWatching is an offload build that looks at the open files of process pid only.
func binWatching(t *testing.T, pid int) string {
	t.Helper()
	conf, _ := filepath.Abs("../../config")
	bin := filepath.Join(t.TempDir(), "offload")
	flags := ldflags(conf) + " -X main.fdGlob=/proc/" + strconv.Itoa(pid) + "/fd/*"
	if out, err := exec.Command("go", "build", "-ldflags", flags, "-o", bin, "devex/offload").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// A WAL database another dev process holds open outside the job's repository (here in
// dev's /tmp) does not keep the job here: the job sees it read-only, so it can read it, and
// a write fails loudly instead of corrupting it over NFS. The script finds it through a
// file, so no argument names it.
func TestWALDatabaseOutsideTheJobsRepositoryIsReadOnlyToARemoteJob(t *testing.T) {
	dbDir, err := os.MkdirTemp("/tmp", "offload-it-") // dev's /tmp is writable to every job
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dbDir) })
	db := filepath.Join(dbDir, "app.db")
	bin := holdWAL(t, db)
	dir := repo(t)
	writeScript(t, dir, "dbpath", db)
	writeScript(t, dir, "job.py", `import errno, socket, sqlite3
db = open("dbpath").read()
print(socket.gethostname())
print(sqlite3.connect("file:" + db + "?mode=ro", uri=True).execute("select x from t").fetchone()[0])
for f in (db, db + "-wal", db + "-shm"):
    try:
        open(f, "r+b").close()
        print("opened for writing", f)
    except OSError as e:
        print(errno.errorcode[e.errno])
try:
    c = sqlite3.connect(db)
    c.execute("insert into t values (1)")
    c.commit()
    print("inserted")
except sqlite3.Error as e:
    print("sqlite:", e)
`)
	code, stdout, stderr := runOffload(t, bin, dir, autoEnv, "--where", "server", "--explain", "--", "python3", "-I", "job.py")
	want := serverHostname(t) + "\n42\nEROFS\nEROFS\nEROFS\nsqlite: attempt to write a readonly database\n"
	if code != 0 || stdout != want {
		t.Errorf("code %d, stdout %q, stderr %q: want %q", code, stdout, stderr, want)
	}
	for _, f := range []string{db, db + "-wal", db + "-shm"} {
		if !strings.Contains(stderr, "offload: read-only "+f+"\n") {
			t.Errorf("--explain does not list %s read-only:\n%s", f, stderr)
		}
	}
	out, err := exec.Command("python3", "-I", "-c", `import sqlite3, sys
print([r[0] for r in sqlite3.connect(sys.argv[1]).execute("select x from t")])`, db).CombinedOutput()
	if err != nil || string(out) != "[42]\n" {
		t.Errorf("the database on dev: %q %v, want [42]", out, err)
	}
	// Named by an argument, the database is the job's own business: it runs here.
	here, _ := os.Hostname()
	code, stdout, stderr = runOffload(t, bin, dir, autoEnv, "--where", "server", "--", "sh", "-c", "ls \"$1\" >/dev/null && hostname", "sh", db)
	if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: ") || !strings.Contains(stderr, "app.db") {
		t.Errorf("an argument naming the database: code %d, stdout %q, stderr %q: want a local run refused for it", code, stdout, stderr)
	}
}

func TestWALDatabaseInTheJobsRepositoryRunsLocallyEvenWithWhereServer(t *testing.T) {
	dir := repo(t)
	bin := holdWAL(t, filepath.Join(dir, "app.db"))
	here, _ := os.Hostname()
	for _, where := range [][]string{{}, {"--where", "server"}} {
		code, stdout, stderr := runOffload(t, bin, dir, autoEnv, append(where, "--", "hostname")...)
		if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: ") || !strings.Contains(stderr, "app.db") {
			t.Errorf("%v: code %d, stdout %q, stderr %q: want a local run refused for the database", where, code, stdout, stderr)
		}
	}
}

// A dev process that opened the job's database through a bind of the repository names the
// bind's path; offload still finds it in the job's repository.
func TestWALDatabaseHeldThroughABindOfTheRepositoryRunsLocally(t *testing.T) {
	dir := repo(t)
	bind := filepath.Join(t.TempDir(), "bind")
	if err := os.Mkdir(bind, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSh(t, false, "sudo mount --bind "+dir+" "+bind)
	t.Cleanup(func() { mustSh(t, false, "sudo umount --lazy "+bind) })
	bin := holdWAL(t, filepath.Join(bind, "app.db"))
	here, _ := os.Hostname()
	code, stdout, stderr := runOffload(t, bin, dir, autoEnv, "--", "hostname")
	if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: ") || !strings.Contains(stderr, dir+"/app.db") {
		t.Errorf("code %d, stdout %q, stderr %q: want a local run refused for %s/app.db", code, stdout, stderr, dir)
	}
}

func TestWhereHostOverridesRefusalsAndWhereLocalRunsInTheBatchScope(t *testing.T) {
	_, stdout, _ := runOffload(t, offloadBin, repo(t), []string{"DISPLAY=:0"}, "--where", "server", "--", "sh", "-c", "hostname")
	if strings.TrimSpace(stdout) != serverHostname(t) {
		t.Errorf("--where server did not override DISPLAY: ran on %q", stdout)
	}
	_, stdout, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "local", "--", "sh", "-c", scopeProbe)
	if !strings.Contains(stdout, "CPUWeight=10") || strings.Contains(stderr, "running locally") {
		t.Errorf("--where local: stdout %q stderr %q: want the batch scope and no refusal line", stdout, stderr)
	}
}

func TestEightSimultaneousCallsFillTheServerSlotsAndTheRestRunInTheBatchScope(t *testing.T) {
	bin := binWithConfig(t, "slots server 2")
	dir := repo(t)
	srv := serverHostname(t)
	script := scopeProbe + "; sleep 6"
	var mu sync.Mutex
	var onServerCount, batch int
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, stdout, stderr := runOffload(t, bin, dir, autoEnv, "--", "sh", "-c", script)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case code != 0:
				t.Errorf("code %d, stderr %q", code, stderr)
			case strings.HasPrefix(stdout, srv+"\n"):
				onServerCount++
			case strings.Contains(stdout, "CPUWeight=10") && strings.Contains(stderr, "offload: running locally: "):
				// Mostly "server has no free slot"; sshd may also shed a connection
				// of a burst, which falls back local the same way.
				batch++
			default:
				t.Errorf("neither on the server nor in the batch scope: %q %q", stdout, stderr)
			}
		}()
	}
	wg.Wait()
	if onServerCount < 1 || onServerCount > 2 || onServerCount+batch != 8 || batch < 6 {
		t.Errorf("%d on the server, %d in the batch scope; want at most 2 and at least 6", onServerCount, batch)
	}
	waitFor(t, func() bool { return freeSlots(t, 2) == "2" }, 15*time.Second, "both slots free again")
}

func TestWhereHostWithNoFreeSlotRunsLocallyAndSaysSo(t *testing.T) {
	dir := repo(t)
	for i := 0; i < 2; i++ {
		start(t, offloadBin, "server", dir, nil, "sleep", "20")
	}
	waitFor(t, func() bool { return freeSlots(t, 2) == "0" }, 20*time.Second, "both slots taken")
	here, _ := os.Hostname()
	_, stdout, stderr := runOffload(t, offloadBin, dir, nil, "--where", "server", "--", "hostname")
	if stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: server has no free slot") {
		t.Errorf("stdout %q stderr %q: want a local run that says there was no slot", stdout, stderr)
	}
}

func TestSlotIsFreedWhenTheWrapperDies(t *testing.T) {
	// An earlier test's killed wrappers hold their slots until the watchdog stops their jobs.
	waitFor(t, func() bool { return freeSlots(t, 2) == "2" }, 30*time.Second, "slots an earlier test held")
	cmd := startRunning(t, 7331)
	waitFor(t, func() bool { return freeSlots(t, 2) == "1" }, 10*time.Second, "a slot taken")
	cmd.Process.Kill()
	cmd.Wait()
	waitFor(t, func() bool { return freeSlots(t, 2) == "2" }, 20*time.Second, "the slot freed by the launcher's watchdog")
}

func readLog(t *testing.T, state string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "offload", "decisions.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestEveryRunAppendsOneLogRecord(t *testing.T) {
	state := repo(t) // not t.TempDir(): an XDG_STATE_HOME under /tmp keeps every job local
	env := append([]string{"XDG_STATE_HOME=" + state}, autoEnv...)
	dir := repo(t)
	runOffload(t, offloadBin, dir, env, "--", "sh", "-c", "exit 3")
	runOffload(t, offloadBin, dir, env, "--", "sudo", "-n", "true")
	runOffload(t, offloadBin, dir, env, "--where", "local", "--", "true")

	recs := readLog(t, state)
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3: %v", len(recs), recs)
	}
	remote, refused, forced := recs[0], recs[1], recs[2]
	if remote["placement"] != "server" || remote["rule"] != "free-slot" || remote["exit_code"] != 3.0 {
		t.Errorf("remote record = %v", remote)
	}
	if wall, _ := remote["wall_seconds"].(float64); wall <= 0 {
		t.Errorf("remote wall_seconds = %v", remote["wall_seconds"])
	}
	if remote["cwd"] != dir || remote["argv"].([]any)[0] != "sh" || remote["time"] == "" {
		t.Errorf("remote record = %v", remote)
	}
	if cpu, ok := remote["cpu_seconds"].(float64); !ok || cpu < 0 {
		t.Errorf("remote cpu_seconds = %v, want the launcher's report", remote["cpu_seconds"])
	}
	if refused["placement"] != "local" || refused["rule"] != "sudo" {
		t.Errorf("refused record = %v", refused)
	}
	if _, ok := refused["cpu_seconds"].(float64); !ok {
		t.Errorf("local cpu_seconds = %v, want rusage of the children", refused["cpu_seconds"])
	}
	if forced["placement"] != "local" || forced["rule"] != "where-local" || forced["exit_code"] != 0.0 {
		t.Errorf("forced record = %v", forced)
	}
}

// A flock a dev process holds is invisible to a remote job's flock (NFS makes it a POSIX
// lock), so a job in the lock's directory runs here, even under --where server, and sees
// the lock held.
func TestAFlockHeldOnDevKeepsAJobInItsDirectoryLocal(t *testing.T) {
	dir := repo(t)
	holder := exec.Command("python3", "-I", "-c", `import fcntl, time
f = open("lk", "w")
fcntl.flock(f, fcntl.LOCK_EX)
print("ready", flush=True)
time.sleep(120)
`)
	holder.Dir = dir
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	if buf := make([]byte, 5); func() bool { _, err := out.Read(buf); return err != nil || string(buf) != "ready" }() {
		t.Fatal("the lock holder did not start")
	}
	here, _ := os.Hostname()
	code, stdout, stderr := runOffload(t, binWatching(t, holder.Process.Pid), dir, autoEnv, "--where", "server", "--",
		"sh", "-c", "hostname; flock -n lk true && echo got || echo busy")
	if code != 0 || stdout != here+"\nbusy\n" || !strings.Contains(stderr, "holds a flock on "+dir+"/lk") {
		t.Errorf("code %d, stdout %q, stderr %q: want a local run that sees the lock held and says why", code, stdout, stderr)
	}
}

// A descriptor past stderr that the caller passes on (`3>file`) exists only here, so the
// job runs here, even under --where server, and writes through it.
func TestAnInheritedDescriptorKeepsTheJobLocal(t *testing.T) {
	dir := repo(t)
	f, err := os.Create(filepath.Join(dir, "out3"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(offloadBin, "--where", "server", "--", "sh", "-c", "hostname; echo y >&3")
	cmd.Dir, cmd.Env, cmd.ExtraFiles = dir, append(os.Environ(), autoEnv...), []*os.File{f}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := exitOf(cmd.Run())
	here, _ := os.Hostname()
	b, _ := os.ReadFile(filepath.Join(dir, "out3"))
	if code != 0 || stdout.String() != here+"\n" || string(b) != "y\n" || !strings.Contains(stderr.String(), "fd 3") {
		t.Errorf("code %d, stdout %q, out3 %q, stderr %q: want a local run writing fd 3 that says why", code, stdout.String(), b, stderr.String())
	}
}

// An offloaded job's own offload call runs its command where the job is, in place.
func TestAnOffloadCallInsideAJobRunsInPlace(t *testing.T) {
	server := serverHostname(t)
	code, stdout, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--",
		"sh", "-c", "hostname; "+shq(offloadBin)+" --where server -- sh -c 'hostname; exit 3'")
	if code != 3 || stdout != server+"\n"+server+"\n" || !strings.Contains(stderr, "running in place") {
		t.Errorf("code %d, stdout %q, stderr %q: want both on %s, the inner exit code, and the notice", code, stdout, stderr, server)
	}
}

// A caller that set HOME elsewhere writes its caches under that HOME on dev, as it would
// locally: the remote-local caches go over the passwd home only.
func TestAnOverriddenHomeKeepsItsCachesOnDev(t *testing.T) {
	dir := repo(t)
	cache := filepath.Join(dir, "h", ".cache", "go-build")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runOffload(t, offloadBin, dir, append([]string{"HOME=" + filepath.Join(dir, "h")}, autoEnv...),
		"--where", "server", "--", "sh", "-c", `echo x > "$HOME/.cache/go-build/f"`)
	if b, err := os.ReadFile(filepath.Join(cache, "f")); code != 0 || err != nil || string(b) != "x\n" {
		t.Errorf("code %d, stderr %q, f %q (%v): want the cache file on dev", code, stderr, b, err)
	}
}

// A job has a pid namespace of its own: a pkill or a pid read from dev reaches nothing on
// the remote, so a test's teardown cannot kill the remote's own processes or another job's.
func TestAJobSeesAndSignalsOnlyItsOwnProcesses(t *testing.T) {
	mustSh(t, true, "setsid sleep 7320 >/dev/null 2>&1 < /dev/null &")
	t.Cleanup(func() { mustSh(t, true, "pkill -xf 'sleep 7320' || true") })
	theirs := strings.TrimSpace(mustSh(t, true, "pgrep -xf 'sleep 7320'"))
	code, stdout, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--", "sh", "-c",
		"hostname; pkill -xf 'sleep 7320'; echo pkill $?; kill -0 "+theirs+" 2>/dev/null; echo kill $?; ps -e -o pid= | wc -l")
	stdout, ok := strings.CutPrefix(stdout, serverHostname(t)+"\n")
	if code != 0 || !ok || !strings.HasPrefix(stdout, "pkill 1\nkill 1\n") {
		t.Errorf("code %d, stdout %q, stderr %q: want pkill and kill to find nothing", code, stdout, stderr)
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(stdout, "pkill 1\nkill 1\n"))); n == 0 || n > 8 {
		t.Errorf("the job saw %q processes, want only its own", stdout)
	}
	if got := strings.TrimSpace(mustSh(t, true, "pgrep -xf 'sleep 7320' || true")); got != theirs {
		t.Errorf("the remote's own sleep is gone: pgrep %q, want %q", got, theirs)
	}
}

// A job cannot change the remote's own container stores: both are bound read-only. A
// script that names one, even through a bare `cd`, is kept here by the remote-local
// refusal; one that reaches it through a home only known at run time is not, so the binds
// are the guard tested.
func TestAJobCannotChangeTheRemotesContainerStores(t *testing.T) {
	for _, dir := range []string{".local/share/containers/offload-it-", ".cache/containers/offload-it-"} {
		path := dir + strconv.Itoa(os.Getpid())
		t.Cleanup(func() { os.Remove(filepath.Join(os.Getenv("HOME"), path)) }) // a run kept here makes it on dev
		code, _, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--", "sh", "-c", "cd && mkdir -p "+path+" && rmdir "+path)
		if code != 0 || !strings.Contains(stderr, "running locally") || !strings.Contains(stderr, "remote-local") {
			t.Errorf("named mkdir %s: code %d, stderr %q: want it kept here as remote-local", path, code, stderr)
		}
		script := `cd "$(getent passwd "$(id -un)" | cut -d: -f6)" && mkdir -p ` + path
		code, _, stderr = runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--", "sh", "-c", script)
		if code == 0 || !strings.Contains(stderr, "Read-only file system") {
			t.Errorf("unnamed mkdir %s: code %d, stderr %q: want EROFS", path, code, stderr)
		}
	}
}

// An offloaded job sees the caller's environment exactly: nothing the launcher's own steps
// add (python's LC_CTYPE) reaches it; only the display, D-Bus and agent entries and
// OFFLOAD_JOB differ.
func TestAnOffloadedJobSeesTheCallersEnvironmentExactly(t *testing.T) {
	env := append([]string{"OFFLOAD_IT_MARK=1"}, autoEnv...)
	code, stdout, stderr := runOffload(t, offloadBin, repo(t), env, "--where", "server", "--", "env", "-0")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	want := map[string]string{"OFFLOAD_JOB": "1"}
	for _, e := range append(os.Environ(), env...) {
		name, value, _ := strings.Cut(e, "=")
		switch {
		case name == "SSH_AUTH_SOCK", name == "DISPLAY", name == "WAYLAND_DISPLAY", strings.HasPrefix(name, "DBUS_"):
		default:
			want[name] = value
		}
	}
	got := map[string]string{}
	for _, e := range strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00") {
		name, value, _ := strings.Cut(e, "=")
		if name != "SSH_AUTH_SOCK" {
			got[name] = value
		}
	}
	for name, value := range got {
		if w, ok := want[name]; !ok || w != value {
			t.Errorf("job saw %s=%q, caller had %q (set: %v)", name, value, w, ok)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("job lost %s", name)
		}
	}
}

// sudo run in an offloaded job would act as root on the remote, not on dev: it fails instead.
func TestSetuidProgramsFailInAnOffloadedJob(t *testing.T) {
	code, _, stderr := runOffload(t, offloadBin, repo(t), autoEnv, "--where", "server", "--", "sudo", "-n", "true")
	if code == 0 || !strings.Contains(stderr, "no new privileges") {
		t.Errorf("code %d, stderr %q: want sudo refused by no_new_privs", code, stderr)
	}
}
