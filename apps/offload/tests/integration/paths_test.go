package integration

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codeRepo makes a scratch repository directly under ~/code, where the Operator's
// `Edit(~/code/**)` rule makes it writable and `Deny Edit(~/code/*/TASKS.md)` applies.
func codeRepo(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir, err := os.MkdirTemp(filepath.Join(home, "code"), "offload-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "TASKS.md"), []byte("tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWritesFollowTheEditRules(t *testing.T) {
	dir := codeRepo(t)
	r := execIn(t, dir, true, "sh", "-c", "echo hi > new.txt")
	if r.code != 0 || r.files["new.txt"].content != "hi\n" {
		t.Fatalf("write under ~/code: code %d, stderr %q, files %v", r.code, r.stderr, r.files)
	}

	denied := execIn(t, dir, true, "sh", "-c", "echo x >> TASKS.md")
	if denied.code == 0 {
		t.Errorf("write to a denied path succeeded")
	}
	if !strings.Contains(denied.stderr, "TASKS.md") || !strings.Contains(denied.stderr, "Read-only file system") {
		t.Errorf("stderr should name the denied path: %q", denied.stderr)
	}
	if denied.files["TASKS.md"].content != "tasks\n" {
		t.Errorf("TASKS.md changed on dev: %q", denied.files["TASKS.md"].content)
	}
	if read := execIn(t, dir, true, "cat", "TASKS.md"); read.stdout != "tasks\n" {
		t.Errorf("a denied file should stay readable: %q", read.stdout)
	}
}

// Dev's /tmp and /var/tmp are the job's: what dev put there the job reads, and what the
// job writes there lands on dev, through a link in the repository too. /run is empty.
func TestTmpAndVarTmpAreDevsAndRunIsEmpty(t *testing.T) {
	name := fmt.Sprintf("offload-it-%d", os.Getpid())
	for _, d := range []string{"/tmp", "/var/tmp"} {
		p := filepath.Join(d, name)
		t.Cleanup(func() { os.Remove(p); os.Remove(p + ".link") })
	}
	if err := os.WriteFile("/tmp/"+name, []byte("dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := repo(t)
	writeScript(t, dir, "write.sh", "hostname; cat /tmp/"+name+"; echo job > /var/tmp/"+name+
		"; ln -s /tmp tmplink && echo link > tmplink/"+name+".link && rm tmplink; ls -A /run\n")
	r := execIn(t, dir, true, "sh", "write.sh")
	if want := serverHostname(t) + "\ndev\n"; r.code != 0 || r.stdout != want {
		t.Fatalf("code %d, stdout %q, stderr %q: want %q (the server, dev's file, an empty /run)", r.code, r.stdout, r.stderr, want)
	}
	for p, want := range map[string]string{"/var/tmp/" + name: "job\n", "/tmp/" + name + ".link": "link\n"} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s on dev: %q %v, want %q", p, b, err, want)
		}
	}
}

// A unix socket in /tmp serves the job's own processes, as it would locally.
func TestAJobsSocketInTmpWorks(t *testing.T) {
	dir := repo(t)
	sock := fmt.Sprintf("/tmp/offload-it-%d.sock", os.Getpid())
	t.Cleanup(func() { os.Remove(sock) })
	writeScript(t, dir, "sock.py", `import os, socket
p = "`+sock+`"
s = socket.socket(socket.AF_UNIX); s.bind(p); s.listen()
c = socket.socket(socket.AF_UNIX); c.connect(p); a, _ = s.accept()
c.send(b"ok"); print(a.recv(2).decode()); os.unlink(p)
`)
	if r := execIn(t, dir, true, "python3", "sock.py"); r.code != 0 || r.stdout != "ok\n" {
		t.Errorf("code %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// The job's /dev/shm and POSIX message queues are its own: it sees none of the server's
// (podman keeps its locks in /dev/shm there), the server sees none of its, and they end
// with it.
func TestDevShmAndMessageQueuesArePrivateToTheJob(t *testing.T) {
	name := fmt.Sprintf("offload-it-%d", os.Getpid())
	hostIPC := strings.TrimSpace(mustSh(t, true, "sudo readlink /proc/1/ns/ipc"))
	// The scripts live in files: an argument that names /dev/shm keeps the job local.
	dir := repo(t)
	writeScript(t, dir, "shm.sh", "ls -A /dev/shm /dev/mqueue; readlink /proc/self/ns/ipc; echo x >/dev/shm/"+name+" && cat /dev/shm/"+name+" && rm /dev/shm/"+name+"\n")
	writeScript(t, dir, "list.sh", "ls -A /dev/shm\n")
	r := execIn(t, dir, true, "sh", "shm.sh")
	lines := strings.Split(r.stdout, "\n")
	if r.code != 0 || len(lines) != 6 || strings.Join(lines[:3], "\n") != "/dev/mqueue:\n\n/dev/shm:" || lines[4] != "x" {
		t.Fatalf("code %d, stdout %q, stderr %q: want empty /dev/shm and /dev/mqueue, then the file", r.code, r.stdout, r.stderr)
	}
	if ipc := lines[3]; ipc == hostIPC {
		t.Errorf("the job runs in the server's IPC namespace %s", ipc)
	}
	if _, err := sh(t, true, "test -e /dev/shm/"+name); err == nil {
		t.Errorf("the job's /dev/shm file appeared in the server's")
		sh(t, true, "rm -f /dev/shm/"+name)
	}
	if again := execIn(t, dir, true, "sh", "list.sh"); again.code != 0 || again.stdout != "" {
		t.Errorf("the next job's /dev/shm: code %d, stdout %q, stderr %q, want it empty", again.code, again.stdout, again.stderr)
	}
	// A file left there ends with the job, where a local run would have kept it: the run fails.
	writeScript(t, dir, "leave.sh", "echo x >/dev/shm/"+name+"\n")
	if left := execIn(t, dir, true, "sh", "leave.sh"); left.code != 1 || !strings.Contains(left.stderr, "/dev/shm: "+name) {
		t.Errorf("a file left in /dev/shm: code %d, stderr %q, want 1 naming it", left.code, left.stderr)
	}
}

// writeScript writes a job's script into dir, so that what it names is not in the job's
// arguments, where a private directory or a held database keeps the job local.
func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The job's /dev/shm, /dev/mqueue and /run are its own and end with it, and the export does
// not carry dev's other mounts: an argument or a variable naming any of them keeps the job
// here, where the write lands, even under --where server.
func TestNamingTheJobsOwnDirectoriesOrAnUnexportedMountRunsLocally(t *testing.T) {
	name := fmt.Sprintf("offload-it-%d", os.Getpid())
	shm := "/dev/shm/" + name
	t.Cleanup(func() { os.Remove(shm) })
	here, _ := os.Hostname()
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	cases := []struct {
		name   string
		env    []string
		argv   []string
		writes bool // the job writes x into shm
		link   bool // the job's directory holds shmlink, a symlink to /dev/shm
	}{
		{"a script writing into /dev/shm", nil, []string{"sh", "-c", "echo x > " + shm + " && hostname"}, true, false},
		{"a variable naming /dev/shm", []string{"OUT=" + shm}, []string{"sh", "-c", `echo x > "$OUT" && hostname`}, true, false},
		{"a variable naming /run", []string{"GOTMPDIR=" + runtimeDir + "/x"}, []string{"hostname"}, false, false},
		{"a script naming /dev/mqueue", nil, []string{"sh", "-c", "ls /dev/mqueue >/dev/null; hostname"}, false, false},
		{"a script naming an unexported mount", nil, []string{"sh", "-c", "ls /boot/efi >/dev/null 2>&1; hostname"}, false, false},
		{"/dev/shm spelt with a doubled slash", nil, []string{"sh", "-c", "echo x > /dev//shm/" + name + " && hostname"}, true, false},
		{"/dev/shm spelt with /./", nil, []string{"sh", "-c", "echo x > /dev/./shm/" + name + " && hostname"}, true, false},
		{"/run spelt /var/run", nil, []string{"sh", "-c", "ls /var/run >/dev/null; hostname"}, false, false},
		{"a link to /dev/shm inside a compound value", nil, []string{"sh", "-c", `echo x > "${0#*:}" && hostname`, "--out=a:shmlink/" + name}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(shm)
			dir := repo(t)
			if c.link {
				if err := os.Symlink("/dev/shm", filepath.Join(dir, "shmlink")); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := runOffload(t, offloadBin, dir, c.env, append([]string{"--where", "server", "--"}, c.argv...)...)
			if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: ") {
				t.Errorf("code %d, stdout %q, stderr %q: want a local run that says why", code, stdout, stderr)
			}
			if b, err := os.ReadFile(shm); c.writes && (err != nil || string(b) != "x\n") {
				t.Errorf("the write into /dev/shm did not land on dev: %q %v", b, err)
			}
		})
	}
	// XDG_RUNTIME_DIR names /run/user/<uid> in every login shell; it keeps nothing local.
	code, stdout, stderr := runOffload(t, offloadBin, repo(t), []string{"XDG_RUNTIME_DIR=" + runtimeDir}, "--where", "server", "--", "hostname")
	if code != 0 || strings.TrimSpace(stdout) != serverHostname(t) {
		t.Errorf("with XDG_RUNTIME_DIR: code %d, stdout %q, stderr %q: want it run on the server", code, stdout, stderr)
	}
}

// The export does not carry a mount inside a writable path: a remote sees the directory it
// hides, so the job gets it read-only and a write there fails rather than landing in the
// hidden directory. A job whose cwd is on it, or holds it, runs here.
func TestAMountInsideAWritablePathIsReadOnlyToARemoteJob(t *testing.T) {
	dir := repo(t)
	mnt := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := fmt.Sprintf("size=1m,uid=%d,gid=%d", os.Getuid(), os.Getgid())
	if out, err := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", opts, "offload-it", mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount a tmpfs: %v\n%s", err, out)
	}
	unmounted := false
	umount := func() {
		if !unmounted {
			unmounted = true
			// nfsd's export cache holds the mount a remote looked up, until it is flushed.
			if out, err := exec.Command("sudo", "-n", "exportfs", "-f").CombinedOutput(); err != nil {
				t.Errorf("exportfs -f: %v\n%s", err, out)
			}
			if out, err := exec.Command("sudo", "-n", "umount", mnt).CombinedOutput(); err != nil {
				t.Errorf("umount: %v\n%s", err, out)
			}
		}
	}
	t.Cleanup(umount)
	// The job runs beside the mount: a cwd that holds it runs here (below).
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, work, "w.sh", "hostname\necho x > ../mnt/f\n")

	code, stdout, stderr := runOffload(t, offloadBin, work, autoEnv, "--where", "server", "--explain", "--", "sh", "w.sh")
	if code == 0 || stdout != serverHostname(t)+"\n" || !strings.Contains(stderr, "Read-only file system") {
		t.Errorf("a write into the mount: code %d, stdout %q, stderr %q: want it to fail read-only on the server", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "offload: read-only "+mnt+"\n") {
		t.Errorf("--explain does not list %s read-only:\n%s", mnt, stderr)
	}
	if _, err := os.Stat(filepath.Join(mnt, "f")); err == nil {
		t.Error("the write landed in dev's mount")
	}

	here, _ := os.Hostname()
	for _, cwd := range []string{mnt, dir} { // on the mount, and holding it
		code, stdout, stderr = runOffload(t, offloadBin, cwd, autoEnv, "--where", "server", "--", "hostname")
		if code != 0 || stdout != here+"\n" || !strings.Contains(stderr, "offload: running locally: ") {
			t.Errorf("cwd %s: code %d, stdout %q, stderr %q: want a local run", cwd, code, stdout, stderr)
		}
	}

	umount()
	if _, err := os.Stat(filepath.Join(mnt, "f")); err == nil {
		t.Error("the write landed in the directory the mount hides")
	}
}

func TestExplainListsEachPathClass(t *testing.T) {
	dir := codeRepo(t)
	cmd := exec.Command(offloadBin, "--where", "server", "--explain", "--", "true")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	home, _ := os.UserHomeDir()
	for _, want := range []string{
		"writable " + filepath.Join(home, "code"),
		"read-only " + filepath.Join(dir, "TASKS.md"),
		"remote-local " + filepath.Join(home, ".cache/go-build"),
		"writable /tmp",
		"writable /var/tmp",
		"private /dev/shm",
		"empty /run",
	} {
		if !strings.Contains(stderr.String(), "offload: "+want+"\n") {
			t.Errorf("--explain lacks %q:\n%s", want, stderr.String())
		}
	}
}

// Dev's /tmp is exported: a job working there, naming it, or with TMPDIR there runs remotely.
func TestWorkUnderDevsTmpRunsRemotely(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "offload-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	want := serverHostname(t)

	cases := map[string]func() outcome{
		"cwd":      func() outcome { return execIn(t, dir, true, "hostname") },
		"argument": func() outcome { return execIn(t, repo(t), true, "sh", "-c", "hostname", dir) },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			if r := run(); r.stdout != want+"\n" {
				t.Errorf("ran on %q, want %q (stderr %q)", strings.TrimSpace(r.stdout), want, r.stderr)
			}
		})
	}
	t.Run("TMPDIR", func(t *testing.T) {
		t.Setenv("TMPDIR", dir)
		if r := execIn(t, repo(t), true, "hostname"); r.stdout != want+"\n" {
			t.Errorf("ran on %q, want %q (stderr %q)", strings.TrimSpace(r.stdout), want, r.stderr)
		}
	})
}

// NFS keeps a file deleted while open as a .nfs file in its directory until it is closed,
// so removing that directory fails on a remote where it succeeds here. The job fails
// loudly, and the .nfs file is gone once the job ends.
func TestRemovingADirectoryWithAnOpenFileFailsLoudlyOnARemote(t *testing.T) {
	dir := repo(t)
	writeScript(t, dir, "rm.sh", `d=$(mktemp -d) && echo "$d" > made && exec 3>"$d/f" && rm -r "$d" && echo cleaned`+"\n")
	if local := execIn(t, dir, false, "sh", "rm.sh"); local.code != 0 || local.stdout != "cleaned\n" {
		t.Fatalf("local run: %+v", local)
	}
	remote := execIn(t, dir, true, "sh", "rm.sh")
	if remote.code == 0 || !strings.Contains(remote.stderr, "Directory not empty") {
		t.Errorf("remote: code %d, stderr %q, want a failure naming the directory", remote.code, remote.stderr)
	}
	made, _ := os.ReadFile(filepath.Join(dir, "made"))
	if d := strings.TrimSpace(string(made)); strings.HasPrefix(d, "/tmp/") {
		left, _ := filepath.Glob(filepath.Join(d, ".nfs*"))
		if len(left) != 0 {
			t.Errorf(".nfs files remain after the job: %v", left)
		}
		os.RemoveAll(d)
	} else {
		t.Errorf("the job's directory %q is not in dev's /tmp", d)
	}
}

func TestKilledJobLeavesNoSillyRenameFiles(t *testing.T) {
	dir := repo(t)
	cmd := exec.Command(offloadBin, "--where", "server", "--", "sh", "-c",
		"echo x > keep && exec 3<keep && rm keep && echo ready && sleep 60")
	cmd.Dir = dir
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "ready\n" {
		t.Fatalf("job did not start: %q %v", buf, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".nfs*")); len(left) == 0 {
		t.Fatal("the probe is blind: an open deleted file leaves no .nfs file while the job runs")
	}
	// Kill the job the way the remote's own watchdog will: SIGKILL to its whole scope.
	mustSh(t, true, `sudo systemctl kill --signal=SIGKILL 'offload-job-*.scope'`)
	if err := cmd.Wait(); err == nil {
		t.Error("a killed job exited 0")
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		left, _ := filepath.Glob(filepath.Join(dir, ".nfs*"))
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("silly-rename files remain: %v", left)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestCachesStayRemoteLocalWhileDevUsesItsOwn(t *testing.T) {
	workload := os.Getenv("OFFLOAD_TEST_GO_WORKLOAD")
	if workload == "" {
		t.Skip("set OFFLOAD_TEST_GO_WORKLOAD to a Go module directory with dependencies")
	}
	dir := repo(t)
	files := map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.21\n",
		"m_test.go":      "package m\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
		"pyproject.toml": "[project]\nname = \"m\"\nversion = \"0\"\nrequires-python = \">=3.9\"\ndependencies = []\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	devDownload := exec.Command("go", "mod", "download")
	devDownload.Dir = workload
	if err := devDownload.Start(); err != nil {
		t.Fatal(err)
	}
	venv := fmt.Sprintf("/tmp/offload-it-venv-%d", os.Getpid())
	t.Cleanup(func() { os.RemoveAll(venv) })
	for _, argv := range [][]string{{"env", "GOWORK=off", "go", "test", "-count=1", "./..."}, {"env", "UV_PROJECT_ENVIRONMENT=" + venv, "uv", "sync"}} {
		if r := execIn(t, dir, true, argv...); r.code != 0 {
			t.Errorf("%v offloaded: exit %d\n%s%s", argv, r.code, r.stdout, r.stderr)
		}
	}
	if err := devDownload.Wait(); err != nil {
		t.Errorf("go mod download on dev: %v", err)
	}
	verify := exec.Command("go", "mod", "verify")
	verify.Dir = workload
	if out, err := verify.CombinedOutput(); err != nil {
		t.Errorf("dev's module cache is damaged: %v\n%s", err, out)
	}
}
