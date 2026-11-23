package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestUsesSudo(t *testing.T) {
	tests := []struct {
		argv []string
		want bool
	}{
		{[]string{"sudo", "true"}, true},
		{[]string{"/usr/bin/sudo", "true"}, true},
		{[]string{"true"}, false},
		{[]string{"grep", "sudo", "file"}, false},
		{[]string{"sh", "-c", "sudo true"}, true},
		{[]string{"npm", "--call", "sudo id", "exec"}, true},
		{[]string{"npm", "-c", "sudo id", "run", "dev"}, false}, // npm runs no call for run
		// A substitution in double quotes ends where bash ends it, past its own quotes and
		// case patterns, and not at an escaped quote's `)`.
		{[]string{"bash", "-c", `x="$(printf '%s)' a; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(echo "a)" ; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(case a in a) sudo id;; esac)"`}, true},
		{[]string{"bash", "-c", `echo "$(if true; then case a in a) true;; esac; fi; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(for i in 1; do case a in a) true;; esac; done; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$({ case a in a) true;; esac; }; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(! case a in a) true;; esac; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$( ( case a in a) true;; esac); sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(echo $(case a in a) true;; esac); sudo id)"`}, true},
		{[]string{"bash", "-c", "echo \"$(echo a # )\nsudo id)\""}, true},
		{[]string{"bash", "-c", `echo "$(echo ${x:-)}; sudo id)"`}, true},
		{[]string{"bash", "-c", "echo \"$(cat <<EOF\n)\nEOF\nsudo id)\""}, true},
		{[]string{"bash", "-c", "echo \"$(cat <<-'EOF'\n)\n\tEOF\nsudo id)\""}, true},
		{[]string{"bash", "-c", `echo "$(echo a#b ); sudo id"`}, false},
		{[]string{"bash", "-c", `echo $'it\'s'; sudo id`}, true},
		{[]string{"bash", "-c", `echo "$(echo $(echo a)#b)"; sudo id`}, true},
		{[]string{"bash", "-c", `echo "$(echo ${x:-'}'})"; sudo id`}, true},
		{[]string{"bash", "-c", `echo "$(echo ${x:-$'}'})"; sudo id`}, true},
		{[]string{"bash", "-c", "echo \"$(cat <<< hi\necho x)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$(echo $((x<<y))\necho)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$(x=1; (( x <<= 1 ))\necho $x)\"; sudo id"}, true},
		{[]string{"bash", "-c", `echo "$(if case a in a) true;; esac; then sudo id; fi)"`}, true},
		{[]string{"bash", "-c", `echo "$(while case a in a) false;; esac; do :; done; sudo id)"`}, true},
		{[]string{"bash", "-c", "builtin exec sudo id"}, true},
		{[]string{"bash", "-c", "coproc sudo id"}, true},
		{[]string{"fish", "--command=sudo id"}, true},
		{[]string{"fish", "--comm", "sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$(case a in a)# it's\n true;; esac)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$(echo $((1+2))#x)\"; sudo id"}, true}, // `#` after `$((…))` is mid-word
		{[]string{"bash", "-c", "echo \"$(echo $(( 1 ))#x)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$( (echo x)# it's\n)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$( (echo x)# c (\n)\"; sudo id"}, true},
		{[]string{"bash", "-c", "echo \"$(echo $(echo a)#b)\"; sudo id"}, true},
		{[]string{"ash", "-c", "sudo id"}, true}, // each shell isShell names reads -c
		{[]string{"yash", "-c", "sudo id"}, true},
		{[]string{"posh", "-c", "sudo id"}, true},
		{[]string{"csh", "-c", "sudo id"}, true},
		{[]string{"tcsh", "-c", "sudo id"}, true},
		{[]string{"mksh", "-c", "sudo id"}, true},
		{[]string{"bash", "-c", `echo "$(echo $'a)\'b'; sudo id)"`}, true},
		{[]string{"bash", "-c", `echo "$(echo \")\"; sudo id)"`}, false},
		{[]string{"bash", "-c", `echo "$(echo ")"); sudo id"`}, false},
		{[]string{"env", "-S", "FOO='a b' sudo id"}, true}, // env -S splits as the shell does
		{[]string{"npx", "-c", "sudo id"}, true},           // npx runs its -c script in a shell
		{[]string{"npm", "exec", "--call=sudo id"}, true},
		{[]string{"npx", "-yc", "sudo id"}, true}, // npm reads short clusters
		{[]string{"npm", "exec", "-yc", "sudo id"}, true},
		{[]string{"npx", "--cal", "sudo id"}, false}, // -c -a -l: npm 10.9.9 reads `sudo id` as a package and runs nothing
		{[]string{"npx", "--registry", "http://x", "-c", "sudo id"}, true},
		{[]string{"npm", "--userconfig", "/x", "exec", "--cache", "/y", "-yc", "sudo id"}, true},
		{[]string{"npx", "-y", "vite", "-c", "sudo id"}, false}, // vite's -c, after npx's command
		{[]string{"npx", "--no", "-c", "sudo id"}, true},        // --no is --no-yes, a switch
		{[]string{"npm", "exec", "--no", "-c", "sudo id"}, true},
		{[]string{"npm", "--no", "exec", "-c", "sudo id"}, true},
		{[]string{"npx", "--no", "--call=sudo id"}, true},
		{[]string{"npx", "--npm", "x", "-c", "sudo id"}, true}, // npx drops --npm and its value
		{[]string{"npx", "-n", "-c", "sudo id"}, false},        // npx drops -n with -c as its value, then runs `sudo id` the program
		// A String option gives a following option word back to nopt (npm 10.9.9 ran each call).
		{[]string{"npx", "--message", "-c", "sudo id"}, true},
		{[]string{"npx", "-m", "-c", "sudo id"}, true},
		{[]string{"npm", "exec", "--tag", "-c", "sudo id"}, true},
		{[]string{"npx", "--message", "--call=sudo id"}, true},
		{[]string{"npm", "--pref", "exec", "-c", "sudo id"}, true},           // an ambiguous prefix is an unknown switch
		{[]string{"npx", "--registry", "-c", "sudo id"}, false},              // a url option takes -c
		{[]string{"npm", "--no-no-registry", "exec", "-c", "sudo id"}, true}, // any no- prefix makes a switch
		{[]string{"bash", "-c", "cat <<EOF\ndon't\nEOF\nsudo id"}, true},
		// npm's Boolean options and shorthands take no value (npx 10.9: vite runs with its own -c).
		{[]string{"npx", "--ignore-scripts", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "--silent", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "--verbose", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "--porcelain", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "-ddd", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "--legacy-peer-deps", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "-q", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "--no-install", "vite", "-c", "sudo id"}, false},
		{[]string{"npx", "-w", "app", "-c", "sudo id"}, true}, // -w is --workspace, which takes one
		{[]string{"npx", "-c", "echo sudo"}, false},
		{[]string{"bash", "-lc", "echo hi && sudo true"}, true},
		{[]string{"bash", "-euo", "pipefail", "-c", "sudo ls"}, true},
		{[]string{"sh", "-c", "env \\\n  sudo ls"}, true},
		{[]string{"sh", "-c", "# don't forget\nsudo make install"}, true},
		{[]string{"sh", "-c", "make # sudo later"}, false},
		{[]string{"sh", "-c", `x="$(sudo cat /etc/shadow)"; echo "$x"`}, true},
		{[]string{"sh", "-c", "echo \"`sudo id`\""}, true},
		{[]string{"sh", "-c", `echo "$(date) sudo"`}, false},
		{[]string{"bash", "-c", `echo "x \"y\""; sudo ls`}, true},
		{[]string{"bash", "-c", `echo "a \"; sudo ls\""`}, false},
		{[]string{"bash", "-co", "pipefail", "sudo ls"}, true},
		{[]string{"bash", "-c", "--", "sudo ls"}, true},
		{[]string{"bash", "-eo", "pipefail", "script.sh", "sudo"}, false},
		{[]string{"bash", "-c", "make; sudo make install"}, true},
		{[]string{"bash", "-c", "a | sudo tee /x"}, true},
		{[]string{"bash", "-c", "(sudo true)"}, true},
		{[]string{"bash", "-c", "echo sudo"}, false},
		{[]string{"bash", "-c", "cat sudoers"}, false},
		{[]string{"bash", "script.sh", "sudo"}, false},
		{[]string{"env", "sudo", "true"}, true},
		{[]string{"env", "-u", "X", "FOO=1", "sudo", "true"}, true},
		{[]string{"/usr/bin/nice", "sudo", "true"}, true},
		{[]string{"nice", "-n", "5", "sudo", "true"}, true},
		{[]string{"/usr/bin/time", "-o", "t.log", "sudo", "make", "install"}, true},
		// A short-option cluster ends in an option that takes the next word.
		{[]string{"time", "-ao", "t.log", "sudo", "make"}, true},
		{[]string{"time", "-pf", "%e", "sudo", "make"}, true},
		{[]string{"nice", "-n5", "sudo", "true"}, true},
		{[]string{"xargs", "-0n", "1", "sudo", "rm"}, true},
		{[]string{"exec", "sudo", "true"}, true},
		{[]string{"FOO=1", "sudo", "true"}, true},
		{[]string{"env", "bash", "-c", "make && sudo make install"}, true},
		{[]string{"env", "true", "sudo"}, false},
		{[]string{"nice", "make"}, false},
		{[]string{"bash", "-c", "if true; then sudo true; fi"}, true},
		{[]string{"bash", "-c", "FOO=1 sudo true"}, true},
		{[]string{"bash", "-c", "x; exec sudo true"}, true},
		{[]string{"bash", "-c", "env FOO=1 nice sudo true"}, true},
		{[]string{"bash", "-c", "echo then sudo"}, false},
		{[]string{"bash", "-c", "FOO=sudo true"}, false},
		{[]string{"stdbuf", "-oL", "sudo", "true"}, true},
		{[]string{"stdbuf", "-o", "L", "sudo", "true"}, true},
		{[]string{"ionice", "-c3", "sudo", "-n", "true"}, true},
		{[]string{"ionice", "-c", "2", "-n", "7", "sudo", "true"}, true},
		{[]string{"setsid", "-f", "sudo", "true"}, true},
		{[]string{"taskset", "0x3", "sudo", "true"}, true},
		{[]string{"taskset", "-c", "0-3", "sudo", "true"}, true},
		{[]string{"chrt", "-b", "0", "sudo", "true"}, true},
		{[]string{"flock", "f", "sudo", "true"}, true},
		{[]string{"flock", "-w", "5", "f", "sudo", "true"}, true},
		{[]string{"flock", "f", "-c", "make && sudo make install"}, true},
		{[]string{"xargs", "-n", "1", "sudo", "rm"}, true},
		{[]string{"xargs", "-I{}", "sudo", "rm", "{}"}, true},
		{[]string{"doas", "true"}, true},
		{[]string{"su", "-c", "true"}, true},
		{[]string{"sudoedit", "/etc/hosts"}, true},
		{[]string{"pkexec", "true"}, true},
		{[]string{"run0", "true"}, true},
		{[]string{"bash", "-c", "echo `sudo id`"}, true},
		{[]string{"bash", "-c", "echo $(sudo id)"}, true},
		{[]string{"npm", "--color", "always", "exec", "-c", "sudo id"}, true},
		{[]string{"pnpm", "-c", "exec", "sudo id"}, true},
		{[]string{"pnpm", "--shell-mode=false", "exec", "sudo id"}, false},
		{[]string{"pnpm", "exec", "sudo id"}, false},
		// Each word after setpriv's, prlimit's and systemd-run's options is a program, as util-linux
		// 2.41 and systemd 257 read them (`+` getopt: options stop at the first operand).
		{[]string{"setpriv", "--reuid", "1000", "sudo", "id"}, true},
		{[]string{"setpriv", "--reu", "1000", "sudo", "id"}, true},
		{[]string{"prlimit", "--nofile=4096", "sudo", "id"}, true},
		{[]string{"prlimit", "-n", "sudo", "id"}, true}, // -n's value is attached only, so sudo is the program
		{[]string{"systemd-run", "--user", "--scope", "-p", "CPUWeight=50", "sudo", "id"}, true},
		{[]string{"systemd-run", "--expand-environment", "no", "sudo", "id"}, true},
		{[]string{"busybox", "sudo", "id"}, true},
		// sg hands its first word after the group (and -c) to sh -c; script runs -c's script.
		{[]string{"sg", "wheel", "-c", "make && sudo make install"}, true},
		{[]string{"sg", "wheel", "sudo id"}, true},
		{[]string{"sg", "wheel", "echo", "sudo"}, false},
		{[]string{"script", "-q", "/dev/null", "-c", "sudo id"}, true},
		{[]string{"script", "-qc", "sudo id", "/dev/null"}, true},
		{[]string{"script", "--comm=sudo id", "/dev/null"}, true},
		{[]string{"script", "-O", "sudo", "/dev/null"}, false},
		{[]string{"taskset", "0x3", "true", "sudo"}, false},
		{[]string{"flock", "sudo", "true"}, false},
		{[]string{"xargs", "echo", "sudo"}, false},
	}
	for _, tt := range tests {
		if got := usesSudo(tt.argv); got != tt.want {
			t.Errorf("usesSudo(%q) = %v, want %v", tt.argv, got, tt.want)
		}
	}
}

// A holder's path is rewritten through dev's binds of its root filesystem, except a
// foreign holder's, which is its container's and names nothing dev's binds show.
func TestOnDevPathsKeepsAForeignHoldersPath(t *testing.T) {
	targets := []openFile{{path: "/srv/data/app.db-wal"}, {path: "/srv/data/app.db-wal", foreign: true}}
	onDevPaths(targets, [][2]string{{"/srv/data", "/home/testuser/data"}})
	if targets[0].path != "/home/testuser/data/app.db-wal" || targets[1].path != "/srv/data/app.db-wal" {
		t.Errorf("paths %q, %q; want dev's holder rewritten, the foreign one kept", targets[0].path, targets[1].path)
	}
}

// A descriptor above 2 the caller passed without close-on-exec keeps the job here; one
// opened close-on-exec is not passed on.
func TestInheritedFd(t *testing.T) {
	// The test's own runner may have passed one (a gate's pipe): close-on-exec it first,
	// so the test starts as a caller that passes nothing.
	for got := inheritedFd(); got != ""; got = inheritedFd() {
		var n int
		fmt.Sscanf(got, "fd %d", &n)
		syscall.CloseOnExec(n)
	}
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := inheritedFd(); got != "" {
		t.Errorf("a close-on-exec descriptor: %q", got)
	}
	fd := int(f.Fd())
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); errno != 0 {
		t.Fatal(errno)
	}
	if got, want := inheritedFd(), fmt.Sprintf("fd %d (/dev/null)", fd); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A holder names its file by the real path, so a writable root spelled through a symlink
// is matched by its real path too: missed, the job would see the held database writable
// and write it over NFS beside dev's holder.
func TestGatherFactsFindsAHeldDatabaseUnderASymlinkedWritableRoot(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(real, "app.db")
	if err := os.WriteFile(db+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `exec 3<"$1"; echo up; exec sleep 30`, "sh", db+"-wal")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	io.ReadFull(out, make([]byte, 3))
	defer func(g string) { fdGlob = g }(fdGlob)
	fdGlob = "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd/*"
	cwd := t.TempDir()
	f, readOnly := gatherFacts(t.TempDir(), cwd, "", []string{"make"}, []string{link})
	if f.WAL != "" || !slices.Contains(readOnly, db) {
		t.Errorf("refusal %q, read-only %q; want %s bound read-only", f.WAL, readOnly, db)
	}
}

// A database a dev process opened through a bind of the root filesystem is matched by the
// path the job reaches it by: the holder names the bind's mount point, the job the folder
// it shows.
func TestGatherFactsFindsAHeldDatabaseOpenedThroughABind(t *testing.T) {
	dir := t.TempDir()
	bind, root := filepath.Join(dir, "bind"), filepath.Join(dir, "root")
	for _, d := range []string{bind, root} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bind, "app.db-wal"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `exec 3<"$1"; echo up; exec sleep 30`, "sh", filepath.Join(bind, "app.db-wal"))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	io.ReadFull(out, make([]byte, 3))
	defer func(g string) { fdGlob = g }(fdGlob)
	fdGlob = "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd/*"
	defer func(f func() [][2]string) { devRootBinds = f }(devRootBinds)
	devRootBinds = func() [][2]string { return [][2]string{{bind, root}} }
	f, _ := gatherFacts(t.TempDir(), root, "", []string{"make"}, []string{root})
	if want := filepath.Join(root, "app.db-wal"); f.WAL != want {
		t.Errorf("refusal %q, want %q", f.WAL, want)
	}
}

// A job naming a file another account's process holds a flock on stays here: that process's
// open files are unreadable to the caller, so they are read as root, locks and all. The
// lock was taken by flock(1), which has ended, so /proc/locks names no living holder, and
// the read of /proc/locks drops it, as a page skip may: the lock is found in fdinfo.
func TestGatherFactsRefusesAJobUsingAFileAnotherAccountFlocks(t *testing.T) {
	useTestMounts(t)
	if exec.Command("sudo", "-n", "true").Run() != nil {
		t.Skip("needs passwordless sudo")
	}
	dir := t.TempDir()
	lock := filepath.Join(dir, "lockfile")
	if err := os.WriteFile(lock, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sudo", "-n", "sh", "-c", `exec 3<"$1"; flock 3; echo $$; exec sleep 30`, "sh", lock)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatal(err)
	}
	defer func() { exec.Command("sudo", "-n", "kill", strconv.Itoa(pid)).Run(); cmd.Wait() }()
	defer func(r func() string) { readLocks = r }(readLocks)
	readLocks = func() string { return "" }
	// Every process is read: a narrowed scan skips the hidden-holder check.
	f, _ := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"cat", lock}, []string{dir})
	var st syscall.Stat_t
	if err := syscall.Stat(lock, &st); err != nil {
		t.Fatal(err)
	}
	id := idOf(st)
	if mine := fmt.Sprintf("%02x:%02x:%d ", id.major, id.minor, id.ino); f.Flock != lock || f.Unseen != "" {
		t.Errorf("flock refusal %q, unseen %q; want %s refused, its holder (%s) seen", f.Flock, f.Unseen, lock, mine)
	}
	defer func(l func([]string) ([]openFile, error)) { listAsRoot = l }(listAsRoot)
	listAsRoot = func([]string) ([]openFile, error) { return nil, errors.New("sudo: a password is required") }
	f, _ = gatherFacts(t.TempDir(), t.TempDir(), "", []string{"cat", lock}, []string{dir})
	if d := place(placeRequest{Remotes: []string{"server"}, Slots: map[string]slotState{"server": {Total: 1, Free: 1}}, Facts: f}); d.Rule != "unseen-holder" {
		t.Errorf("without root: %+v, want the unseen-holder refusal", d)
	}
}

// A cwd reached through a symlink holds the databases under its real path, the path a
// holder names: one there keeps the job here, not bound read-only beneath it.
func TestWalPlanRefusesADatabaseUnderTheRealPathOfASymlinkedCwd(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	targets := []openFile{{path: filepath.Join(real, "app.db-wal")}}
	if refuse, ro := walPlan(targets, []string{dir}, link, "", "/home/testuser", []string{"make"}, nil); refuse != targets[0].path || ro != nil {
		t.Errorf("refusal %q, read-only %q; want %s refused", refuse, ro, targets[0].path)
	}
}

// An argument naming a held file through a symlinked folder names it: the holder opened it
// by its real path, which the argument's text never spells.
func TestNamesFileFollowsASymlinkedFolder(t *testing.T) {
	cwd, real := t.TempDir(), t.TempDir()
	if err := os.Symlink(real, filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(real, "app.db")
	if err := os.WriteFile(db, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !namesFile([]string{db}, cwd, t.TempDir(), []string{"sqlite3", "link/app.db"}, nil) {
		t.Errorf("link/app.db does not name %s", db)
	}
	// An absolute URL path through the link, percent-encoded.
	if !namesFile([]string{db}, t.TempDir(), t.TempDir(), nil, []string{"DATABASE_URL=sqlite:///" + cwd + "/link/app%2Edb"}) {
		t.Errorf("an absolute sqlite URL through the link does not name %s", db)
	}
	if namesFile([]string{db}, cwd, t.TempDir(), []string{"sqlite3", "other.db"}, nil) {
		t.Errorf("other.db names %s", db)
	}
}

// A script's SQLite URI names its file before the query: `?` ends the path.
func TestNamesFileReadsAScriptsURIUpToItsQuery(t *testing.T) {
	cwd := t.TempDir()
	db := filepath.Join(cwd, "x.db")
	if err := os.WriteFile(db, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !namesFile([]string{db}, filepath.Join(cwd, "sub"), t.TempDir(), []string{"sh", "-c", "python app.py file:../x.db?mode=ro"}, nil) {
		t.Errorf("file:../x.db?mode=ro does not name %s", db)
	}
}

// The built-in watch rules hold with no config/local-only file.
func TestGatherFactsAppliesTheBuiltInWatchRulesWithoutTheFile(t *testing.T) {
	f, _ := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"tsc", "--watch"}, nil)
	if f.WatchServe == "" {
		t.Error("tsc --watch was not matched with no local-only file")
	}
}

// A script's `~` and $HOME are the job's HOME, which a caller may have set away from its
// account's: a FIFO named that way is seen, and keeps the job here.
func TestGatherFactsReadsAScriptsHomeAsTheJobsHome(t *testing.T) {
	home := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(home, "f"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, script := range []string{"cat ~/f", "cat $HOME/f"} {
		f, _ := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"sh", "-c", script}, nil)
		if !strings.Contains(f.Special, "is a FIFO") {
			t.Errorf("%q: special-file refusal %q, want the FIFO under HOME", script, f.Special)
		}
	}
}

// gatherFacts joins the cwd's links into the tmpfs refusal: a link onto /dev/shm, which the
// export does not carry, keeps `make` here.
func TestGatherFactsRefusesACwdLinkOntoAnUnexportedMount(t *testing.T) {
	useTestMounts(t)
	t.Setenv("TMPDIR", "/tmp")
	cwd := t.TempDir()
	link := filepath.Join(cwd, "shm")
	if err := os.Symlink("/dev/shm", link); err != nil {
		t.Fatal(err)
	}
	want := "link " + link + " leads onto dev's /dev/shm mount, which is not exported"
	if f, _ := gatherFacts(t.TempDir(), cwd, "", []string{"make"}, []string{cwd}); f.Tmpfs != want {
		t.Errorf("Tmpfs %q, want %q", f.Tmpfs, want)
	}
}

// /proc/locks may skip a lock held throughout one read of it (a lock released between its
// pages moves the rest up), so a held flock is read from its holder's fdinfo: missed, the
// job would take the lock beside dev's holder.
func TestGatherFactsSeesAFlockProcLocksSkipped(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lockfile")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holdOpen(t, "sh", "-c", `exec 3<"$1"; flock 3; echo up; exec sleep 30`, "sh", lock)
	defer func(r func() string) { readLocks = r }(readLocks)
	readLocks = func() string { return "" }
	if f, _ := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"flock", lock, "true"}, []string{dir}); f.Flock != lock {
		t.Errorf("a job naming the lock: flock refusal %q, want %s", f.Flock, lock)
	}
	if f, ro := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"make"}, []string{dir}); f.Flock != "" || !slices.Contains(ro, lock) {
		t.Errorf("a job not naming it: flock refusal %q, read-only %q, want %s read-only", f.Flock, ro, lock)
	}
}

// A WAL database a connection in exclusive locking mode holds has no -shm, ever: SQLite
// keeps its index in the holder's memory and write-locks the bytes every reader must
// share-lock first, and NFS carries a remote reader's lock to dev, where it fails before the
// reader would create a -shm. So it is bound read-only with no wal-bare refusal; an
// ordinary connection that has not read yet still refuses.
func TestGatherFactsBindsAnExclusiveModeWALDatabaseWithoutItsShm(t *testing.T) {
	const create = `import sqlite3, sys
c = sqlite3.connect(sys.argv[1]); c.execute("pragma journal_mode=wal"); c.execute("create table t(x)"); c.commit(); c.close()`
	const hold = `import sqlite3, sys, time
c = sqlite3.connect(sys.argv[1], isolation_level=None)
if sys.argv[2] == "exclusive":
    c.execute("pragma locking_mode=exclusive"); c.execute("select * from t").fetchall()
print("up", flush=True); time.sleep(30)`
	for _, c := range []struct{ mode, bare string }{{"exclusive", ""}, {"normal", "app.db"}} {
		t.Run(c.mode, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "app.db")
			if out, err := exec.Command("python3", "-I", "-c", create, db).CombinedOutput(); err != nil {
				t.Skipf("python3's sqlite3: %v %s", err, out)
			}
			holdOpen(t, "python3", "-I", "-c", hold, db, c.mode)
			if _, err := os.Lstat(db + "-shm"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the holder made a -shm: %v", err)
			}
			f, ro := gatherFacts(t.TempDir(), t.TempDir(), "", []string{"make"}, []string{dir})
			want := ""
			if c.bare != "" {
				want = filepath.Join(dir, c.bare)
			}
			if f.WAL != "" || f.WALBare != want || !slices.Contains(ro, db) {
				t.Errorf("wal-open %q, wal-bare %q, read-only %q; want wal-bare %q, %s read-only", f.WAL, f.WALBare, ro, want, db)
			}
		})
	}
}

// A job the tmpfs rule keeps here skips the link walk: its cwd's link into a remote-local
// path, which a walk would find, goes unreported.
func TestGatherFactsSkipsTheLinkWalkForAJobTheTmpfsRuleKeepsHere(t *testing.T) {
	home := accountHome()
	if home == "" {
		t.Skip("no account home")
	}
	cwd, err := os.MkdirTemp("/dev/shm", "offload-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(cwd) })
	if err := os.Symlink(home, filepath.Join(cwd, "home")); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "remote-local-paths"), []byte(".\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := gatherFacts(config, cwd, "", []string{"make"}, nil)
	if !strings.Contains(f.Tmpfs, "/dev/shm") || f.RemoteLocal != "" {
		t.Errorf("Tmpfs %q, RemoteLocal %q: want the /dev/shm cwd refused and no link walked", f.Tmpfs, f.RemoteLocal)
	}
}

// A command config/remote-store-commands lists is a remote-local fact, which --where cannot
// override, so `offload --where server podman build .` still builds on dev.
func TestGatherFactsRefusesAStoreCommandAsRemoteLocal(t *testing.T) {
	cwd := t.TempDir()
	f, _ := gatherFacts("../config", cwd, "", []string{"podman", "build", "."}, nil)
	if !strings.Contains(f.RemoteLocal, `"podman"`) {
		t.Errorf("RemoteLocal %q, want podman refused", f.RemoteLocal)
	}
	if f.WatchServe != "" {
		t.Errorf("WatchServe %q, want the store command left to remote-local", f.WatchServe)
	}
}
