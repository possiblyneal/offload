package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// outsideTmp is a fresh directory outside /tmp (t.TempDir is under /tmp when TMPDIR is
// unset), removed when the test ends.
func outsideTmp(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(wd, ".test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestTmpRefusal(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// A working directory outside /tmp holding links to /tmp, /dev/shm and the unexported mount.
	work := outsideTmp(t)
	for name, target := range map[string]string{"tmplink": "/tmp", "shmlink": "/dev/shm", "dangling": "/dev/shm/offload-test-absent", "mountlink": real, "mountparent": dir} {
		if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(work, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling link whose relative target climbs out of a symlinked directory in /dev/shm.
	shm, err := os.MkdirTemp("/dev/shm", "offload-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shm) })
	if err := os.Symlink("../offload-test-absent", filepath.Join(shm, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shm, filepath.Join(work, "shmdir")); err != nil {
		t.Fatal(err)
	}
	// A chain of dangling links that ends in that one, and a dangling link whose `sub/..`
	// climbs from where its symlinked directory really is.
	for name, target := range map[string]string{"chain": "chain2", "chain2": "shmdir/x", "climb": "shmdir/../offload-test-absent"} {
		if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
	}
	up := strings.Repeat("../", strings.Count(work, "/"))
	mounts := []string{"/run/user/1000", real, "/mnt/data", "/mnt/homelab/router"}
	// A plain cwd: outside /tmp, and no ancestor of the unexported mount, wherever TMPDIR is.
	plain := outsideTmp(t)
	tests := []struct {
		name string
		cwd  string
		env  []string
		argv []string
		want string // substring of the reason, "" for none
	}{
		{"plain job", plain, nil, []string{"make"}, ""},
		{"cwd on an unexported mount", "/run/user/1000/x", nil, []string{"make"}, "cwd"},
		{"cwd symlinked onto an unexported mount", link, nil, []string{"make"}, "cwd"},
		{"cwd in /dev/shm", "/dev/shm/w", nil, []string{"make"}, "/dev/shm"},
		{"cwd through a link to /dev/shm", filepath.Join(work, "shmlink", "w"), nil, []string{"make"}, "/dev/shm"},

		// Dev's /tmp and /var/tmp are the job's too: naming them keeps nothing here.
		{"cwd in /tmp", "/tmp/w", nil, []string{"make"}, ""},
		{"cwd through a link to /tmp", filepath.Join(work, "tmplink", "w"), nil, []string{"make"}, ""},
		{"an argument in /var/tmp", plain, nil, []string{"cp", "a", "/var/tmp/b"}, ""},
		{"a script redirecting into /tmp", plain, nil, []string{"sh", "-c", "make > /tmp/out"}, ""},
		{"TMPDIR in /tmp", plain, []string{"TMPDIR=/tmp/t"}, []string{"make"}, ""},
		{"a variable naming /var/tmp", plain, []string{"GOCOVERDIR=/var/tmp/c"}, []string{"go", "test"}, ""},

		{"flag value on an unexported mount", plain, nil, []string{"tool", "--out=/run/user/1000/x"}, "argument"},
		{"flag value in /dev/shm", plain, nil, []string{"tool", "--out=/dev/shm/x"}, "/dev/shm"},
		{"argument is /dev/shm itself", plain, nil, []string{"ls", "/dev/shm"}, "/dev/shm"},
		{"a script escapes a letter of /dev/shm", plain, nil, []string{"sh", "-c", `ls /dev/sh\m/x`}, "/dev/shm"},
		{"a sibling name is not under /dev/shm, but a device", plain, nil, []string{"cp", "/dev/shmfoo"}, "dev's own device /dev/shmfoo"},

		{"relative argument climbing into /dev/shm", work, nil, []string{"cp", "a", up + "dev/shm/x"}, "/dev/shm"},
		{"relative argument through a link to /dev/shm", work, nil, []string{"cp", "a", "shmlink/x/y"}, "/dev/shm"},
		{"a disk device", work, nil, []string{"dd", "if=/dev/sda", "of=x"}, "dev's own device /dev/sda"},
		{"a device named in a script", work, nil, []string{"sh", "-c", "echo x > /dev/ttyUSB0"}, "/dev/ttyUSB0"},
		{"a device in a Python list", work, nil, []string{"python3", "-c", `import subprocess; subprocess.run(["/dev/sda1"])`}, "/dev/sda1"},
		{"a device in a JSON value", work, nil, []string{"sh", "-c", `echo '{"p":"/dev/sda"}'`}, "/dev/sda"},
		{"/sys as a dict key", work, nil, []string{"python3", "-c", `f({'/sys/x':1})`}, "kernel and hardware state"},
		{"a quoted redirection to a device", work, nil, []string{"sh", "-c", `echo x >"/dev/ttyUSB0"`}, "/dev/ttyUSB0"},
		{"a quoted redirection from /sys", work, nil, []string{"sh", "-c", `cat <"/sys/kernel/x"`}, "kernel and hardware state"},
		{"a path after a closing quote is not one", work, []string{"d=x"}, []string{"sh", "-c", `ls "$d"/dev/sda "${d}"/sys "$(pwd)"/dev/sda`}, ""},
		{"/dev itself", work, nil, []string{"ls", "/dev"}, "dev's own device /dev"},
		{"a device in a variable", work, []string{"PORT=/dev/ttyACM0"}, []string{"make"}, "/dev/ttyACM0"},
		{"/sys", work, nil, []string{"cat", "/sys/class/net/eth0/address"}, "kernel and hardware state"},
		{"a dev process by pid", work, nil, []string{"cat", "/proc/1/status"}, "the dev process 1"},
		{"the null device in a script", work, nil, []string{"sh", "-c", "make 2>/dev/null"}, ""},
		{"stdin and an fd", work, nil, []string{"cat", "/dev/stdin", "/dev/fd/0", "/dev/stdout", "/dev/stderr", "/proc/thread-self/status"}, ""},
		{"the job's own process through a link", work, nil, []string{"cat", "/etc/mtab"}, ""},
		{"dev's own device through the process's root", work, nil, []string{"cat", "/proc/self/root/dev/sda"}, "dev's own device /dev/sda"},
		{"dev's /sys through the thread's root", work, nil, []string{"ls", "/proc/thread-self/root/sys/class"}, "kernel and hardware state"},
		{"the caller's terminal in a variable", work, []string{"SSH_TTY=/dev/pts/3", "GPG_TTY=/dev/pts/3"}, []string{"make"}, ""},
		{"bash's network redirection", work, nil, []string{"bash", "-c", "exec 3<>/dev/tcp/10.0.0.1/80 4<>/dev/udp/10.0.0.1/53"}, ""},
		{"random bytes", work, nil, []string{"head", "-c", "8", "/dev/urandom", "/dev/random", "/dev/zero", "/dev/full"}, ""},
		{"the job's own process", work, nil, []string{"cat", "/proc/self/status", "/proc/cpuinfo"}, ""},
		{"systemd's memory pressure file", work, []string{"MEMORY_PRESSURE_WATCH=/sys/fs/cgroup/x/memory.pressure"}, []string{"make"}, ""},
		{"argument that is a dangling link into /dev/shm", work, nil, []string{"tee", "dangling"}, "/dev/shm"},
		{"argument that is a dangling link climbing out of a linked /dev/shm directory", work, nil, []string{"touch", "shmdir/x"}, "/dev/shm"},
		{"argument that is a chain of dangling links into /dev/shm", work, nil, []string{"touch", "chain"}, "/dev/shm"},
		{"argument that is a dangling link climbing out of a linked directory it names", work, nil, []string{"touch", "climb"}, "/dev/shm"},
		{"script path through a link onto an unexported mount", work, nil, []string{"sh", "-c", "touch " + work + "/mountlink/x"}, "not exported"},
		{"script path through the process's root into /run", work, nil, []string{"sh", "-c", "cat /proc/self/root/run/x"}, "/run"},
		{"variable through a link onto an unexported mount", work, []string{"BUILD=" + work + "/mountlink/x"}, []string{"make"}, "not exported"},
		{"variable through a link to /dev/shm", work, []string{"OUT=" + work + "/shmlink/x"}, []string{"make"}, "/dev/shm"},
		{"script naming a mount under ~", work, []string{"HOME=" + dir}, []string{"sh", "-c", "ls ~/real"}, "not exported"},
		{"script naming a mount under ${HOME}", work, []string{"HOME=" + dir}, []string{"sh", "-c", "ls ${HOME}/real/x"}, "not exported"},
		{"variable naming a mount under $HOME", work, []string{"HOME=" + dir, "DATA=$HOME/real"}, []string{"make"}, "not exported"},
		{"relative argument through a link onto an unexported mount", work, nil, []string{"cp", "a", "mountlink/x"}, "not exported"},
		{"flag value through a link to /dev/shm", work, nil, []string{"tool", "--out=shmlink/x"}, "/dev/shm"},
		{"NAME= operand through a link to /dev/shm", work, nil, []string{"dd", "if=a", "of=shmlink/x"}, "/dev/shm"},
		{"the repository's own tmp/ stays", work, nil, []string{"cp", "a", "tmp/b"}, ""},
		{"a relative argument that resolves nowhere special", work, nil, []string{"cp", "a", "build/out"}, ""},

		{"attached short option value in /dev/shm", plain, nil, []string{"gcc", "-o/dev/shm/x", "a.c"}, "/dev/shm"},
		{"attached short option value through a link", work, nil, []string{"gcc", "-oshmlink/x", "a.c"}, "/dev/shm"},
		{"short option value as its own word", plain, nil, []string{"gcc", "-o", "/dev/shm/x", "a.c"}, "/dev/shm"},

		{"bash -c script with no space before /dev/shm", plain, nil, []string{"bash", "-c", "make >/dev/shm/out 2>&1"}, "/dev/shm"},
		{"a script naming /dev//shm", plain, nil, []string{"sh", "-c", "ls /dev//shm"}, "/dev/shm"},
		{"a script naming /dev/./shm", plain, nil, []string{"sh", "-c", "ls /dev/././shm/x"}, "/dev/shm"},
		{"a script naming /var/run, a link to /run", plain, nil, []string{"sh", "-c", "ls /var/run/user"}, "/var/run"},
		{"a script naming /var/lock, a link to /run/lock", plain, nil, []string{"sh", "-c", "ls /var/lock/x"}, "/var/lock"},
		{"a list longer than PATH_MAX naming a link to /dev/shm", work, nil, []string{"tool", "--dirs=" + strings.Repeat("lib:", 1100) + "shmlink/x"}, "/dev/shm"},
		{"a variable naming /var/lock", plain, []string{"LOCKDIR=/var/lock/x"}, []string{"make"}, "/var/lock"},
		{"a :-list value through a link to /dev/shm", work, nil, []string{"tool", "--path=a:shmlink/x"}, "/dev/shm"},
		{"a ,-list value through a link to /dev/shm", work, nil, []string{"tool", "--paths=a,shmlink/x"}, "/dev/shm"},
		{"an @file through a link to /dev/shm", work, nil, []string{"tool", "@shmlink/args"}, "/dev/shm"},
		{"a file: URL through a link to /dev/shm", work, nil, []string{"tool", "file:shmlink/x"}, "/dev/shm"},
		{"a :-list of ordinary paths", work, nil, []string{"tool", "--path=a:build/b"}, ""},
		{"quoted /dev/shm path in a script", plain, nil, []string{"sh", "-c", "cp a '/dev/shm/b'"}, "/dev/shm"},
		{"a file URL into /dev/shm", plain, nil, []string{"curl", "-o", "x", "file:///dev/shm/y"}, "/dev/shm"},
		{"a script naming /dev/shmfoo names a device, not /dev/shm", plain, nil, []string{"sh", "-c", "ls /dev/shmfoo"}, "dev's own device /dev/shmfoo"},

		{"TMPDIR in /dev/shm", plain, []string{"TMPDIR=/dev/shm/t"}, []string{"make"}, "TMPDIR"},
		{"TMPDIR on an unexported mount", plain, []string{"TMPDIR=/run/user/1000/t"}, []string{"make"}, "TMPDIR"},
		{"a relative TMPDIR from the cwd", work, []string{"TMPDIR=" + up + "run/t"}, []string{"make"}, "TMPDIR"},
		{"a list variable naming /dev/shm", plain, []string{"CACHES=/home/testuser/c:/dev/shm/c"}, []string{"make"}, "CACHES"},
		{"the ssh agent socket in /dev/shm refuses nothing", plain, []string{"SSH_AUTH_SOCK=/dev/shm/agent.1"}, []string{"make"}, ""},
		{"the session bus refuses nothing", plain, []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"}, []string{"make"}, ""},
		{"tmux's socket refuses nothing", plain, []string{"TMUX=/run/user/1000/tmux-1000/default,1,0"}, []string{"make"}, ""},

		// The job's /dev/shm, /dev/mqueue and /run are its own and end with it, and the export
		// does not carry an unexported mount: a mention of any of them keeps the job here.
		{"a script writing into /dev/shm", plain, nil, []string{"sh", "-c", "make > /dev/shm/out"}, "/dev/shm"},
		{"an argument in /dev/shm", plain, nil, []string{"cp", "a", "/dev/shm/b"}, "/dev/shm"},
		{"a flag value in /dev/mqueue", plain, nil, []string{"tool", "--queue=/dev/mqueue/q"}, "/dev/mqueue"},
		{"a script naming /run", plain, nil, []string{"sh", "-c", "echo 1 > /run/lock/x"}, "/run"},
		{"a variable naming /dev/shm", plain, []string{"OUT=/dev/shm/r"}, []string{"make"}, "OUT"},
		{"a variable naming /run", plain, []string{"GOTMPDIR=/run/user/1000/x"}, []string{"go", "build"}, "GOTMPDIR"},
		{"a variable naming /dev/mqueue", plain, []string{"Q=/dev/mqueue/q"}, []string{"make"}, "Q"},
		{"a script naming an unexported mount", plain, nil, []string{"sh", "-c", "cp a /mnt/data/b"}, "/mnt/data"},
		{"a variable naming an unexported mount", plain, []string{"DEST=x:/mnt/data/c"}, []string{"make"}, "DEST"},
		{"the runtime directory refuses nothing", plain, []string{"XDG_RUNTIME_DIR=/run/user/1000"}, []string{"make"}, ""},
		{"gpg's agent socket refuses nothing", plain, []string{"GPG_AGENT_INFO=/run/user/1000/gnupg/S.gpg-agent:0:1"}, []string{"make"}, ""},
		{"OLDPWD on an unexported mount refuses nothing", plain, []string{"OLDPWD=/mnt/data/x"}, []string{"make"}, ""},
		{"cwd holding an unexported mount", "/mnt", nil, []string{"find"}, "holds"},
		{"cwd at the root", "/", nil, []string{"ls"}, "holds"},
		{"an argument holding an unexported mount", plain, nil, []string{"find", "/mnt", "-name", "x"}, "holds"},
		{"the root as an argument", plain, nil, []string{"du", "-sh", "/"}, "holds"},
		{"a relative argument climbing to an ancestor of a mount", work, nil, []string{"find", up + "mnt"}, "holds"},
		{"a link to an ancestor of a mount", work, nil, []string{"find", "mountparent"}, "holds"},
		{"a long option value holding an unexported mount", plain, nil, []string{"tar", "--directory=/mnt", "-cf", "x", "."}, "holds"},
		{"an attached short option value holding an unexported mount", plain, nil, []string{"tar", "-C/mnt", "-cf", "x", "."}, "holds"},
		{"an option value of /", plain, nil, []string{"rg", "foo", "--path=/"}, "holds"},
		{"a script naming an ancestor of a mount", plain, nil, []string{"bash", "-c", "du -sh /mnt/homelab"}, "holds"},
		{"a script entering an ancestor of a mount", plain, nil, []string{"bash", "-c", "cd /mnt && ls"}, "holds"},
		{"a script dividing by a slash refuses nothing", plain, nil, []string{"bash", "-c", "echo $((4 / 2))"}, ""},
		{"a script naming a sibling of a mount refuses nothing", plain, nil, []string{"bash", "-c", "ls /mnt/database /mntx"}, ""},
		{"an attached short option value of / refuses nothing", plain, nil, []string{"cut", "-d/", "-f2"}, ""},
		{"a directory beside a mount refuses nothing", plain, nil, []string{"ls", "/mnt/database"}, ""},
		{"systemd's notify socket refuses nothing", plain, []string{"NOTIFY_SOCKET=/run/systemd/notify"}, []string{"make"}, ""},
		{"go run stays", plain, nil, []string{"go", "run", "./cmd"}, ""},
		{"a script named run stays", plain, nil, []string{"sh", "-c", "scripts/run x && ~/code/x/run/y"}, ""},
		{"a script's default value naming a mount", plain, nil, []string{"sh", "-c", "ls ${D:-/mnt/data}"}, "/mnt/data"},
		{"a script's default value holding a mount", plain, nil, []string{"sh", "-c", "ls ${D:-/mnt}"}, "holds"},
		{"a script globbing an ancestor of a mount", plain, nil, []string{"bash", "-c", "ls /etc /mnt/*"}, "holds"},
		{"a script globbing beside a mount", plain, nil, []string{"bash", "-c", "du -s /mnt/homelab/r*"}, "holds"},
		{"a script brace-expanding to a mount's ancestor", plain, nil, []string{"bash", "-c", "ls -- {/mnt,/etc}"}, "holds"},
		{"a script climbing into /run", plain, nil, []string{"sh", "-c", "echo hi > /home/testuser/../../run/x"}, "/run"},
		{"a script climbing into /dev/shm", plain, nil, []string{"sh", "-c", "echo hi > /tmp/../dev/shm/x"}, "/dev/shm"},
		{"a script's default value in /dev/shm", plain, nil, []string{"sh", "-c", "echo hi > ${OUT:-/dev/shm/x}"}, "/dev/shm"},
		{"a script escaping a slash", plain, nil, []string{"sh", "-c", `echo hi >\/dev/shm/x`}, "/dev/shm"},
		{"a script naming a variable below a mount's ancestor", plain, nil, []string{"sh", "-c", "ls /mnt/$X"}, "holds"},
		{"a variable on an unexported mount", plain, []string{"DIR=/mnt/data/x"}, []string{"make"}, "/mnt/data"},
		{"a variable in a private directory", plain, []string{"OUT=${X:-/dev/shm/o}"}, []string{"make"}, "/dev/shm"},
		{"a variable holding an unexported mount stays", plain, []string{"DIR=/mnt"}, []string{"make"}, ""},
		{"home and variable paths stay", plain, nil, []string{"sh", "-c", "ls ~/run ${HOME}/run $(pwd)/run ./run ../run x-y/run"}, ""},
		{"a URL stays", plain, nil, []string{"sh", "-c", "curl https://example.com/run/x"}, ""},
		{"a glob below a plain directory stays", plain, nil, []string{"sh", "-c", "rm -f /home/testuser/code/x/*.o"}, ""},
		{"a script finding from /", plain, nil, []string{"bash", "-c", "find / -name '*.conf'"}, "holds"},
		{"a script entering /", plain, nil, []string{"bash", "-c", "cd / && du -sh *"}, "holds"},
		{"a script globbing /", plain, nil, []string{"bash", "-c", "du -s /*"}, "holds"},
		{"another interpreter listing /", plain, nil, []string{"node", "-e", "console.log(require('fs').readdirSync('/'))"}, "holds"},
		{"a script holding / through a variable", plain, nil, []string{"sh", "-c", "ls /$X"}, "holds"},
		{"tar entering / by an attached -C", plain, nil, []string{"tar", "-C/", "-cf", "/home/testuser/x.tar", "boot", "mnt"}, "holds"},
		{"a delimiter option stays", plain, nil, []string{"cut", "-d/", "-f2", "x"}, ""},
		{"sed deleting blank lines stays", plain, nil, []string{"sed", "-i", "/^$/d", "f"}, ""},
		{"sed stripping a trailing slash stays", plain, nil, []string{"sed", `s/\/$//`, "f"}, ""},
		{"sed with another delimiter stays", plain, nil, []string{"sed", "s,/$,,", "f"}, ""},
		{"a regex with a class stays", plain, nil, []string{"grep", "-E", "/[0-9]+", "f"}, ""},
		{"a script bracket-globbing at the root", plain, nil, []string{"sh", "-c", "ls /[d]ev/shm"}, "/dev/shm"},
		{"find's regex stays", plain, nil, []string{"find", ".", "-regex", ".*/[^/]*\\.go"}, ""},
		{"the shell's dirname stays", plain, nil, []string{"bash", "-c", "d=${f%/*}; b=${f##*/}"}, ""},
		{"a quoted variable's children stay", plain, nil, []string{"bash", "-c", `for f in "$d"/*.go; do cp "$src"/$name x; done`}, ""},
		{"arithmetic division stays", plain, nil, []string{"bash", "-c", "echo $((4 / 2))"}, ""},
		{"/runner and /mnt/database stay", plain, nil, []string{"sh", "-c", "ls /runner /mnt/database"}, ""},
		{"a script's attached -C on a mount", plain, nil, []string{"sh", "-c", "tar -C/mnt/data/etc -czf out.tgz ."}, "/mnt/data"},
		{"a script's attached -C of /", plain, nil, []string{"sh", "-c", "tar -C/ -czf out.tgz ."}, "holds"},
		{"bundled short options before a path", plain, nil, []string{"tar", "-cC/mnt/data/etc", "-f", "x.tgz", "."}, "/mnt/data"},
		{"a make variable's -I", plain, nil, []string{"make", "CFLAGS=-I/mnt/data/usr/include"}, "/mnt/data"},
		{"a linker option list's -L", plain, nil, []string{"gcc", "x.c", "-Wl,-L/mnt/data/lib"}, "/mnt/data"},
		{"an attached -o in a private directory", plain, nil, []string{"sh", "-c", "gcc -c x.c -o/dev/shm/x.o"}, "/dev/shm"},
		{"a variable's -I on an unexported mount", plain, []string{"CGO_CFLAGS=-I/run/user/1000/inc"}, []string{"go", "build"}, "/run"},
		{"a variable's -I on a mount", plain, []string{"CFLAGS=-O2 -I/mnt/data/inc"}, []string{"make"}, "/mnt/data"},
		{"a script's delimiters stay", plain, nil, []string{"sh", "-c", "cut -d/ -f2 x | awk -F/ '{print $1}' | sort -t/ -k2"}, ""},
		{"separate delimiter words stay", plain, nil, []string{"cut", "-d", "/", "-f2", "x"}, ""},
		{"awk's separate -F stays", plain, nil, []string{"awk", "-F", "/", "{print $1}"}, ""},
		{"tr's / stays", plain, nil, []string{"tr", " ", "/"}, ""},
		{"echo's / behind env stays", plain, nil, []string{"env", "X=1", "echo", "/"}, ""},
		{"a shell's positional / still counts", plain, nil, []string{"sh", "-c", `echo x; find "$0"`, "/"}, "holds"},
		{"sort's separate -t stays", plain, nil, []string{"sort", "-t", "/", "-k2", "x"}, ""},
		{"gawk's / stays", plain, nil, []string{"gawk", "-F", "/", "{print $1}"}, ""},
		{"column's separate -s stays", plain, nil, []string{"column", "-t", "-s", "/", "x"}, ""},
		{"paste's separate -d stays", plain, nil, []string{"paste", "-d", "/", "a", "b"}, ""},
		{"paste's long delimiter stays", plain, nil, []string{"paste", "--delimiters", "/", "a", "b"}, ""},
		{"du -s / still counts", plain, nil, []string{"du", "-s", "/"}, "holds"},
		{"ls -d / still counts", plain, nil, []string{"ls", "-d", "/"}, "holds"},
		{"a script's du -s / still counts", plain, nil, []string{"sh", "-c", "du -s / | sort -n"}, "holds"},
		{"find's / after -d-like words still counts", plain, nil, []string{"find", "-depth", "/"}, "holds"},
		{"a script's separate delimiters stay", plain, nil, []string{"sh", "-c", "cut -d / -f2 x | awk -F / '{print $1}' | tr ' ' / | sed 's/x/ /'"}, ""},
		{"a script's expr division stays", plain, nil, []string{"sh", "-c", "n=$(expr 4 / 2)"}, ""},
		{"a / inside a longer string stays", plain, nil, []string{"python3", "-c", `print("a / b")`}, ""},
		{"a / opening a longer string stays", plain, nil, []string{"python3", "-c", `print("/ is the root")`}, ""},
		{"a quoted lone / still counts", plain, nil, []string{"python3", "-c", `import os; os.listdir("/")`}, "holds"},
		{"a script's find / after a text tool still counts", plain, nil, []string{"sh", "-c", "echo start; find / -name x"}, "holds"},
		{"a script's quoted / after a delimiter word counts", plain, nil, []string{"sh", "-c", `ls "/"`}, "holds"},
		{"a / in a quoted substitution counts", plain, nil, []string{"sh", "-c", `x="$(find / -name x)"`}, "holds"},
		{"a / in an echoed substitution counts", plain, nil, []string{"sh", "-c", `echo "$(ls /)"`}, "holds"},
		{"a / in a quoted backtick counts", plain, nil, []string{"sh", "-c", "x=\"`du -s /`\""}, "holds"},
		{"a / in printf's substitution counts", plain, nil, []string{"sh", "-c", `printf "%s\n" "$(ls /)"`}, "holds"},
		{"a / default counts", plain, nil, []string{"sh", "-c", "find ${ROOT:-/} -name x"}, "holds"},
		{"a / after a closed substitution in quotes stays", plain, nil, []string{"sh", "-c", `echo "$(date) / x"`}, ""},
		{"a / after a group in a substitution counts", plain, nil, []string{"sh", "-c", `echo "$( (cd x); ls /)"`}, "holds"},
		{"a / after a closed backtick in quotes stays", plain, nil, []string{"sh", "-c", "echo \"`date` / x\""}, ""},
		{"a / in quotes before a substitution stays", plain, nil, []string{"sh", "-c", `echo "a / b $(date)"`}, ""},
		{"plain -I paths stay", plain, []string{"CFLAGS=-O2 -I/usr/include"}, []string{"gcc", "-I/usr/include", "x.c"}, ""},
		{"a dash inside a word stays", plain, nil, []string{"sh", "-c", "ls a-b/mnt x86-64/run 's/-x/run/'"}, ""},
	}
	for _, tt := range tests {
		got := tmpRefusal(mounts, tt.cwd, tt.env, tt.argv)
		if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
			t.Errorf("%s: tmpRefusal = %q, want substring %q", tt.name, got, tt.want)
		}
	}
}

// The remote-local paths that exist under home are listed, a `ro` line's marked read-only.
func TestRemoteLocalPathsListsThoseOnDevAndMarksReadOnlyOnes(t *testing.T) {
	conf, home := t.TempDir(), t.TempDir()
	for _, d := range []string{".cache/go-build", ".local/share/containers"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list := "# comment\n.cache/go-build\n.cache/uv\nro .local/share/containers\nro .cache/containers\n"
	if err := os.WriteFile(filepath.Join(conf, "remote-local-paths"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	got := remoteLocalPaths(conf, home)
	want := []remoteLocalPath{{home + "/.cache/go-build", ".cache/go-build", false},
		{home + "/.local/share/containers", ".local/share/containers", true}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A bare glob at the cwd matches every entry there but a dotted one, so it reaches an
// undotted remote-local path (`go/pkg/mod`) under the cwd, and a dotted one only by name.
func TestRemoteLocalRefusalReadsABareGlobAtTheCwd(t *testing.T) {
	home := t.TempDir()
	mod := filepath.Join(home, "go/pkg/mod")
	cache := filepath.Join(home, ".cache/go-build")
	for _, d := range []string{mod, cache, filepath.Join(home, "code")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rl := []remoteLocalPath{{mod, "go/pkg/mod", false}, {cache, ".cache/go-build", false}}
	for _, c := range []struct {
		name, cwd string
		argv      []string
		want      bool
	}{
		{"argument from home", home, []string{"find", "*", "-name", "x"}, true},
		{"script from home", home, []string{"sh", "-c", "du -sh *"}, true},
		{"variable from home", home, []string{"sh", "-c", "du -sh $D"}, true},
		{"from a parent of the entry", filepath.Join(home, "go/pkg"), []string{"sh", "-c", "rm -rf *"}, true},
		{"from where none lies below", filepath.Join(home, "code"), []string{"sh", "-c", "grep x *"}, false},
	} {
		got := remoteLocalRefusal(rl, home, c.cwd, c.argv, nil)
		if (got != "") != c.want {
			t.Errorf("%s: refusal %q, want refused %v", c.name, got, c.want)
		}
	}
}

func TestRemoteLocalRefusalKeepsAJobNamingARemoteLocalPathHere(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, ".cache/go-build")
	store := filepath.Join(home, ".local/share/containers")
	for _, d := range []string{cache, store, filepath.Join(home, "repo")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(cache, filepath.Join(home, "repo/link")); err != nil {
		t.Fatal(err)
	}
	rl := []remoteLocalPath{{cache, ".cache/go-build", false}, {store, ".local/share/containers", true}}
	repo := filepath.Join(home, "repo")
	deep := filepath.Join(home, "code/repo")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cwd  string
		argv []string
		want string // "" for no refusal, else a substring of the reason
	}{
		{"cwd inside", cache + "/ab", []string{"ls"}, "cwd " + cache + "/ab"},
		{"absolute argument", repo, []string{"du", "-sh", cache}, "argument " + cache + " reaches"},
		{"option value", repo, []string{"tool", "--out=" + cache + "/x"}, "argument " + cache + "/x reaches"},
		{"relative through a symlink", repo, []string{"ls", "link/x"}, "the remote-local " + cache},
		{"read-only store", repo, []string{"ls", store}, "argument " + store + " reaches"},
		{"script path", repo, []string{"sh", "-c", "rm -rf " + cache + "/*"}, "names " + cache + ", which reaches"},
		{"tilde form", repo, []string{"sh", "-c", "du -sh ~/.cache/go-build"}, "reaches the remote-local " + cache},
		{"HOME form", repo, []string{"sh", "-c", `ls "$HOME/.cache/go-build/x"`}, "reaches the remote-local " + cache},
		{"braced HOME form", repo, []string{"sh", "-c", "ls ${HOME}/.local/share/containers"}, "reaches the remote-local " + store},
		{"script parent by tilde", repo, []string{"sh", "-c", "du -sh ~/.cache"}, "reaches the remote-local " + cache},
		{"script parent by HOME", repo, []string{"sh", "-c", "du -sh $HOME/.cache"}, "reaches the remote-local " + cache},
		{"script parent by braced HOME", repo, []string{"sh", "-c", "du -sh ${HOME}/.cache"}, "reaches the remote-local " + cache},
		{"script glob", repo, []string{"sh", "-c", "rm -rf ~/.cache/*"}, "reaches the remote-local " + cache},
		{"script brace list", repo, []string{"sh", "-c", "ls ~/.cache/{go-build,uv}"}, "reaches the remote-local " + cache},
		{"script tar -C", repo, []string{"sh", "-c", "tar c -C ~/.cache go-build"}, "reaches the remote-local " + cache},
		{"script find home", repo, []string{"sh", "-c", "find ~ -name x"}, "reaches the remote-local"},
		{"script du home", repo, []string{"sh", "-c", "du -sh ~"}, "reaches the remote-local"},
		{"tilde after a redirection", repo, []string{"sh", "-c", "echo x >~/.cache/go-build/f"}, "reaches the remote-local " + cache},
		{"tilde after a numbered redirection", repo, []string{"sh", "-c", "make 2>~/.cache/go-build/err"}, "reaches the remote-local " + cache},
		{"tilde after an input redirection", repo, []string{"sh", "-c", "cat <~/.cache/go-build/f"}, "reaches the remote-local " + cache},
		{"tilde in backticks", repo, []string{"sh", "-c", "echo `~/.cache/go-build/x`"}, "reaches the remote-local " + cache},
		{"tilde in a brace list", repo, []string{"sh", "-c", "ls {~/.cache/go-build,x}"}, "reaches the remote-local " + cache},
		{"quoted HOME then a path", repo, []string{"sh", "-c", `rm -rf "$HOME"/.cache/go-build`}, "reaches the remote-local " + cache},
		{"quoted braced HOME then a path", repo, []string{"sh", "-c", `ls "${HOME}"/.local/share/containers`}, "reaches the remote-local " + store},
		{"HOME with a default", repo, []string{"sh", "-c", "ls ${HOME:-/x}/.cache/go-build"}, "reaches the remote-local " + cache},
		{"quoted HOME then the repo", repo, []string{"sh", "-c", `cd "$HOME"/repo && make`}, ""},
		{"HOMEDIR is not HOME", repo, []string{"sh", "-c", "ls $HOMEDIR/.cache"}, ""},
		{"HOME_X is not HOME", repo, []string{"sh", "-c", "ls $HOME_X/.cache"}, ""},
		{"tilde inside a word", repo, []string{"sh", "-c", "ls a~/.cache"}, ""},
		{"another user's home", repo, []string{"sh", "-c", "ls ~root/.cache ~x/.cache"}, ""},
		{"plain build", repo, []string{"go", "build", "./..."}, ""},
		{"sibling cache", repo, []string{"ls", filepath.Join(home, ".cache/go-buildx")}, ""},
		{"other cache by tilde", repo, []string{"sh", "-c", "ls ~/.cache/pip"}, ""},
		{"parent of a remote-local path", repo, []string{"du", "-sh", filepath.Join(home, ".cache")}, "reaches the remote-local " + cache},
		{"dot from home", home, []string{"find", "."}, "reaches the remote-local"},
		{"cwd holding one", home, []string{"git", "status"}, ""},
		{"script entering one by a relative cd", home, []string{"sh", "-c", "cd .cache/go-build && cat index"}, "under the remote-local " + cache},
		{"script entering its parent then naming it", home, []string{"sh", "-c", "cd .cache; du -sh go-build"}, "the remote-local " + cache},
		{"script entering the repo stays", home, []string{"sh", "-c", "cd repo && make"}, ""},
		{"script climbing by two cds", deep, []string{"sh", "-c", "cd .. && cd .. && cat .cache/go-build/index"}, "the remote-local " + cache},
		{"script climbing by two cds after semicolons", deep, []string{"sh", "-c", "cd ..; cd ..; rm -rf .cache/go-build/*"}, "the remote-local " + cache},
		{"script entering step by step", deep, []string{"sh", "-c", "cd ../.. && cd .cache && cd go-build && cat index"}, "the remote-local " + cache},
		{"script climbing from a quoted PWD", deep, []string{"sh", "-c", `cd "$PWD"/../.. && cat .cache/go-build/index`}, "the remote-local " + cache},
		{"script's bare cd", deep, []string{"sh", "-c", "cd && cat .cache/go-build/index"}, "the remote-local " + cache},
		{"script's bare cd then a holder", deep, []string{"sh", "-c", "cd; du -sh .cache"}, "the remote-local " + cache},
		{"script climbing once stays", deep, []string{"sh", "-c", "cd .. && make"}, ""},
		{"script's bare cd in a group", deep, []string{"sh", "-c", "{ cd; rm -rf .cache/go-build; }"}, "the remote-local " + cache},
		{"script's bare cd as an if condition", deep, []string{"sh", "-c", "if cd; then du -sh .cache; fi"}, "the remote-local " + cache},
		{"script's bare cd in a loop body", deep, []string{"sh", "-c", "for x in 1; do cd; du -sh .cache; done"}, "the remote-local " + cache},
		{"script's negated bare cd", deep, []string{"sh", "-c", "! cd || du -sh .cache"}, "the remote-local " + cache},
		{"script's bare cd through command", deep, []string{"sh", "-c", "command cd; du -sh .cache"}, "the remote-local " + cache},
		{"script's cd -P", deep, []string{"sh", "-c", "cd -P ../../repo && du -sh ../.cache"}, "the remote-local " + cache},
		{"script's pushd", deep, []string{"sh", "-c", "pushd ../../repo && du -sh ../.cache"}, "the remote-local " + cache},
		{"script's cd - back", deep, []string{"sh", "-c", "cd ../../repo; cd " + deep + "; cd -; du -sh ../.cache"}, "the remote-local " + cache},
		{"script's bare pushd back stays", deep, []string{"sh", "-c", "pushd /usr; pushd; make"}, ""},
		{"script's popd back", home, []string{"sh", "-c", "pushd " + repo + "; popd; du -sh .cache"}, "the remote-local " + cache},
		{"a word cd as an argument stays", deep, []string{"sh", "-c", "echo cd; du -sh .cache"}, ""},
		{"relative word cut at a variable", home, []string{"du", "-sh", ".ca$X"}, "the remote-local " + cache},
		{"relative script word cut at a variable", home, []string{"sh", "-c", "du -sh .ca${X}"}, "the remote-local " + cache},
		{"relative word cut at a glob", home, []string{"du", "-sh", ".ca*"}, "the remote-local " + cache},
		{"relative word cut at a brace", home, []string{"sh", "-c", "du -sh .ca{che,x}/go-build"}, "the remote-local " + cache},
		{"relative word cut inside a remote-local dir", home, []string{"sh", "-c", "rm .cache/go-build/$X"}, "the remote-local " + cache},
		{"bare glob at the cwd stays", home, []string{"sh", "-c", "grep x *"}, ""},
		{"bare variable at the cwd stays", home, []string{"sh", "-c", "cat $F"}, ""},
		{"glob elsewhere stays", deep, []string{"sh", "-c", "gofmt -l src/*.go .ca*"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := remoteLocalRefusal(rl, home, c.cwd, c.argv, nil)
			switch {
			case c.want == "" && got != "":
				t.Errorf("refused: %s", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("got %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// expandHome reads `$HOME`, `${HOME}`, `${HOME:-x}`, `"$HOME"`, a word-initial `~` and
// `~user` as the shell does, and leaves a longer name, an unknown user and a `~` inside a
// word alone.
func TestExpandHome(t *testing.T) {
	for in, want := range map[string]string{
		"$HOME/x":               "/h/x",
		"${HOME}/x":             "/h/x",
		"$HOME_X/x":             "$HOME_X/x",
		"$HOMEDIR":              "$HOMEDIR",
		"~":                     "/h",
		"ls ~ ~/x":              "ls /h /h/x",
		"a>~":                   "a>/h",
		"2>~/e":                 "2>/h/e",
		"<~/i":                  "</h/i",
		"`~/b`":                 "`/h/b`",
		"{~,~/y}":               "{/h,/h/y}",
		"P=~:~/b":               "P=/h:/h/b",
		"~root/x ~x":            "/root/x ~x",
		"~nosuchuser9/x":        "~nosuchuser9/x",
		"~root.x/y":             "~root.x/y",
		`"$HOME"/x`:             "/h/x",
		`"${HOME}"/x`:           "/h/x",
		`"$HOME/x"`:             `"/h/x"`,
		"${HOME:-/o}/x":         "/h/x",
		"${HOME-/o}/x ${HOMEX}": "/h/x ${HOMEX}",
		`"$HOME`:                `"/h`,
		"a~/x":                  "a~/x",
		"${CFG:-~/c}":           "${CFG:-/h/c}",
		"${CFG=~}":              "${CFG=/h}",
		"a-~/x":                 "a-~/x",
	} {
		if got := expandHome(in, "/h"); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
	if got := expandHome("~/x", ""); got != "~/x" {
		t.Errorf("with no home: %q", got)
	}
}

// Every job may write /tmp and /var/tmp, so when either is an unexported mount (a PrivateTmp
// caller's) no job leaves dev, whatever it names.
func TestTmpRefusalRefusesEveryJobWhenTmpIsUnexported(t *testing.T) {
	for _, m := range []string{"/tmp", "/var/tmp"} {
		if got := tmpRefusal([]string{m}, outsideTmp(t), nil, []string{"true"}); !strings.Contains(got, m+" is on dev's "+m+" mount") {
			t.Errorf("mount %s: got %q", m, got)
		}
	}
	if got := tmpRefusal([]string{"/tmp/.mount_app"}, outsideTmp(t), nil, []string{"true"}); got != "" {
		t.Errorf("a mount under /tmp refused a job that names none: %q", got)
	}
}

// A FIFO or socket the job names, as an argument or a word of a script, keeps it here: a
// remote job opens the remote's own end of the node the export carries, and blocks or
// fails where a local run would talk to dev's reader. A device node under /dev, a symlink
// to one, and a regular file refuse nothing.
// A long script repeating its words and directories is judged in time: the candidates are
// the distinct words joined with the distinct directories, not every pair of occurrences.
func TestSpecialFileRefusalIsQuickOnALongRepetitiveScript(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	for _, d := range []string{"src", "out"} {
		if err := os.Mkdir(filepath.Join(cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"dots":   strings.Repeat(". ", 6000),
		"cds":    strings.Repeat("cd /tmp; ", 1400),
		"copies": strings.Repeat("cp -r src/ out/ ; echo file . ", 400),
	} {
		start := time.Now()
		specialFileRefusal(cwd, home, []string{"bash", "-c", script}, nil)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: took %v, want under 2 s", name, d)
		}
	}
}

func TestSpecialFileRefusalKeepsAJobNamingAFIFOOrSocketHere(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(cwd, "ff"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, "hf"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bound by a relative name: a socket's path is limited to 108 bytes, which a long
	// TMPDIR exceeds.
	t.Chdir(cwd)
	l, err := net.Listen("unix", "sock")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.WriteFile(filepath.Join(cwd, "plain"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(cwd, "null")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ff", filepath.Join(cwd, "fflink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(cwd, "sub/sf"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf := filepath.Join(cwd, "sub/sf")
	for _, d := range []string{filepath.Join(cwd, "a/b"), filepath.Join(home, "h/sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pipe, spaced, hp := filepath.Join(cwd, "a/b/pipe"), filepath.Join(cwd, "my pipe"), filepath.Join(home, "h/sub/hp")
	for _, f := range []string{pipe, spaced, hp} {
		if err := syscall.Mkfifo(f, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		argv []string
		want string // the refusal's start, or "" for none
		env  []string
	}{
		{argv: []string{"sh", "-c", "cd sub && echo hi > sf"}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", "echo hi > sub/s?"}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > "$PWD/sub/sf"`}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", "echo hi > $(pwd)/sub/sf"}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > "$(pwd -P)/sub/sf"`}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > "${PWD%/}/sub/sf"`}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > "$(realpath .)/sub/sf"`}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > $(readlink -f .)/sub/sf`}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", "echo hi > `pwd -L`/sub/sf"}, want: sf + " is a FIFO"},
		{argv: []string{"sh", "-c", `echo hi > "$F"`}, want: sf + " is a FIFO", env: []string{"F=" + sf}},
		{argv: []string{"true"}, env: []string{"SOCK=" + filepath.Join(cwd, "sock"), "HOME=" + home}},
		{argv: []string{"cat", "ff"}, want: filepath.Join(cwd, "ff") + " is a FIFO"},
		{argv: []string{"tee", filepath.Join(cwd, "ff")}, want: filepath.Join(cwd, "ff") + " is a FIFO"},
		{argv: []string{"sh", "-c", "echo hi > ff"}, want: filepath.Join(cwd, "ff") + " is a FIFO"},
		{argv: []string{"sh", "-c", "echo hi >ff;true"}, want: filepath.Join(cwd, "ff") + " is a FIFO"},
		{argv: []string{"cmd", "--out=ff"}, want: filepath.Join(cwd, "ff") + " is a FIFO"},
		{argv: []string{"cat", "fflink"}, want: filepath.Join(cwd, "fflink") + " is a FIFO"},
		{argv: []string{"sh", "-c", "cat ~/hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"sh", "-c", "cat $HOME/hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "cd; cat hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "{ cd; cat hf; }"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "if cd; then cat hf; fi"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "time cd && cat hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "command cd; cat hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"bash", "-c", "builtin cd; cat hf"}, want: filepath.Join(home, "hf") + " is a FIFO"},
		{argv: []string{"socat", "-", "UNIX-CONNECT:sock"}, want: filepath.Join(cwd, "sock") + " is a socket"},
		{argv: []string{"sh", "-c", "cd a && cd b && cat pipe"}, want: pipe + " is a FIFO"},
		{argv: []string{"sh", "-c", "cd a; cd b; cat pipe"}, want: pipe + " is a FIFO"},
		{argv: []string{"sh", "-c", "cd ~/h && cd sub && cat hp"}, want: hp + " is a FIFO"},
		{argv: []string{"sh", "-c", "cat 'my pipe'"}, want: spaced + " is a FIFO"},
		{argv: []string{"sh", "-c", `cat my\ pipe`}, want: spaced + " is a FIFO"},
		{argv: []string{"sh", "-c", "cat a/{b,x}/pipe"}, want: pipe + " is a FIFO"},
		{argv: []string{"sh", "-c", `cat "$PWD"/a/b/pipe`}, want: pipe + " is a FIFO"},
		{argv: []string{"cat", "plain", "null", "/dev/null", "missing"}, want: ""},
		{argv: []string{"sh", "-c", "make > /dev/null 2>&1"}, want: ""},
		// Text naming two here-documents, the second with no body, is read without a panic.
		{argv: []string{"git", "commit", "-m", "fix: read <<EOF and <<-EOF bodies\n\nBody."}, want: ""},
		{argv: []string{"git", "commit", "-m", "fix: stream with cout << a << b\n\nBody."}, want: ""},
	} {
		got := specialFileRefusal(cwd, home, c.argv, c.env)
		if !strings.HasPrefix(got, c.want) || (c.want == "") != (got == "") {
			t.Errorf("%q: got %q, want %q…", c.argv, got, c.want)
		}
	}
}

// A remote-local path configured as a symlink is matched by its real path too: an
// argument naming the target reaches the remote's copy as surely as the link does.
func TestRemoteLocalRefusalMatchesASymlinkedRemoteLocalPathByItsTarget(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "elsewhere/go-build")
	link := filepath.Join(home, ".cache/go-build")
	for _, d := range []string{target, filepath.Dir(link), filepath.Join(home, "repo")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rl := []remoteLocalPath{{link, ".cache/go-build", false}}
	if got := remoteLocalRefusal(rl, home, filepath.Join(home, "repo"), []string{"du", "-sh", target + "/ab"}, nil); !strings.Contains(got, "reaches") {
		t.Errorf("got %q, want a refusal for %s/ab", got, target)
	}
}

// A symlink to /dev/null is as harmless as /dev/null itself: its real path is a device
// file every machine has.
func TestTmpRefusalPassesASymlinkToDevNull(t *testing.T) {
	cwd := outsideTmp(t)
	if err := os.Symlink("/dev/null", filepath.Join(cwd, "null")); err != nil {
		t.Fatal(err)
	}
	if got := tmpRefusal(nil, cwd, nil, []string{"cp", "x", filepath.Join(cwd, "null")}); got != "" {
		t.Errorf("got %q, want no refusal", got)
	}
}

// A device node outside /dev, as a container's or a chroot's, is one dev's kernel serves:
// a remote job would open the remote's device of that number instead.
func TestSpecialFileRefusalKeepsAJobNamingADeviceNodeHere(t *testing.T) {
	if exec.Command("sudo", "-n", "true").Run() != nil {
		t.Skip("needs passwordless sudo")
	}
	cwd := t.TempDir()
	node := filepath.Join(cwd, "null")
	if out, err := exec.Command("sudo", "-n", "mknod", node, "c", "1", "3").CombinedOutput(); err != nil {
		t.Fatalf("mknod: %v %s", err, out)
	}
	if got := specialFileRefusal(cwd, t.TempDir(), []string{"cat", "null"}, nil); !strings.HasPrefix(got, node+" is a device node") {
		t.Errorf("got %q, want %s refused as a device node", got, node)
	}
	// Only a FIFO counts in the environment: a variable naming a device is not opened by it.
	if got := specialFileRefusal(cwd, t.TempDir(), []string{"true"}, []string{"D=" + node}); got != "" {
		t.Errorf("an environment variable naming a device: %q", got)
	}
}

// A descriptor of the wrapper's own (a pty master here) names the job's own descriptor
// wherever it runs, though it resolves to one of dev's devices.
func TestTmpRefusalPassesTheJobsOwnDescriptorOnADevice(t *testing.T) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer f.Close()
	fd := strconv.Itoa(int(f.Fd()))
	if r := tmpRefusal(nil, outsideTmp(t), nil, []string{"cat", "/dev/fd/" + fd, "/proc/self/fd/" + fd}); r != "" {
		t.Errorf("refused the job's own descriptor: %s", r)
	}
	if r := tmpRefusal(nil, outsideTmp(t), nil, []string{"cat", "/dev/ptmx"}); r == "" {
		t.Errorf("dev's /dev/ptmx named directly was not refused")
	}
}

// A file URL or a SQLAlchemy sqlite URL names a path; any other URL names none.
func TestURLPath(t *testing.T) {
	for in, want := range map[string]string{
		"file:../x.db?mode=rw":         "../x.db",
		"file:///abs/x.db":             "/abs/x.db",
		"file://localhost/abs/x.db":    "/abs/x.db",
		"sqlite:///rel.db":             "rel.db",
		"sqlite:////abs.db":            "/abs.db",
		"sqlite+aiosqlite:///d/x.db#f": "d/x.db",
		"sqlite:///my%20db.sqlite":     "my db.sqlite",
		"https://example.com/x":        "",
		"postgres://h/db":              "",
		"plain":                        "",
	} {
		if got := urlPath(in); got != want {
			t.Errorf("urlPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// readArg reads a word once for every rule: as path candidates from the cwd, and as a
// script with home and the cwd read, its written paths and the words its cd steps reach.
func TestReadArg(t *testing.T) {
	const cwd, home = "/w/repo", "/h"
	for _, c := range []struct {
		in                         string
		candidates, written, steps []string
	}{
		{"out=x", []string{"/w/repo/out=x", "/w/repo/x"}, nil, []string{"/w/repo/out=x"}},
		{"-C/mnt", []string{"/w/repo/-C/mnt", "/mnt"}, []string{"/mnt"}, []string{"/w/repo/-C/mnt"}},
		{`cd ~/a && cat "$PWD"/b`, []string{`/w/repo/cd ~/a && cat "$PWD"/b`},
			[]string{"/h/a", "/w/repo/b"}, []string{"/h/a", "/h/a/cat", "/w/repo/b"}},
		{"cd; du .cache", []string{"/w/repo/cd; du .cache"}, nil, []string{"/h", "/h/du", "/h/.cache"}},
	} {
		r := readArg(c.in, cwd, home)
		if got := slices.Compact(r.candidates()); !slices.Equal(got, c.candidates) {
			t.Errorf("readArg(%q).candidates() = %q, want %q", c.in, got, c.candidates)
		}
		if !slices.Equal(r.written, c.written) {
			t.Errorf("readArg(%q).written = %q, want %q", c.in, r.written, c.written)
		}
		if !slices.Equal(r.steps, c.steps) {
			t.Errorf("readArg(%q).steps = %q, want %q", c.in, r.steps, c.steps)
		}
	}
}
