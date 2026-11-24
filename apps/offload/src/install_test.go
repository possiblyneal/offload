package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkConfig reads conf's remotes with read_remotes, the reader install.sh and
// uninstall.sh both source, which exits before either changes anything.
func checkConfig(t *testing.T, conf string) (int, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "offload.conf")
	if err := os.WriteFile(file, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", `. ../scripts/remotes.sh && read_remotes "$1" "nothing installed"`, "bash", file).CombinedOutput()
	return exitStatus(t, err), string(out)
}

func TestInstallAcceptsRemotesNamedByAddressOrHostName(t *testing.T) {
	conf := "dev 192.0.2.50\n# remote commented out\nremote server 192.0.2.10\nremote\tai   worker.example\nslots server 2\n"
	if code, out := checkConfig(t, conf); code != 0 || out != "" {
		t.Errorf("exit %d, output %q, want a silent 0", code, out)
	}
}

// An export line built from no remote, or from an empty or wildcard address, admits every
// host, so the install refuses before it writes /etc/exports.d.
func TestInstallRefusesAConfigWhoseExportWouldAdmitTheWrongHosts(t *testing.T) {
	for name, conf := range map[string]string{
		"no remote":           "dev 192.0.2.50\n",
		"empty file":          "",
		"no address":          "remote server 192.0.2.10\nremote ai\n",
		"no name or address":  "remote\n",
		"a trailing word":     "remote server 192.0.2.10 (rw)\n",
		"a wildcard":          "remote all *\n",
		"a network":           "remote net 192.0.2.0/24\n",
		"a netgroup":          "remote group @trusted\n",
		"an octet over 255":   "remote server 192.0.2.256\n",
		"a short address":     "remote server 10.10.10\n",
		"a host name with _":  "remote server bad_name.lan\n",
		"options as address":  "remote server rw,no_root_squash\n",
		"an IPv6 address":     "remote server fe80::1\n",
		"last line no return": "remote server 192.0.2.10\nremote ai",
	} {
		if code, out := checkConfig(t, conf); code == 0 || !strings.Contains(out, "offload:") {
			t.Errorf("%s: exit %d, output %q, want a refusal", name, code, out)
		}
	}
}

// fakeInstallTools puts stand-ins for every command install.sh reaches outside its own
// shell on a PATH of their own, so a test runs the whole install against no real machine.
// Each call is logged, one line per call. ssh answers the job check as FAKE_SSH says
// (idle, busy, stopping, launcher, pgrep-fails, down, or hung: a remote that never answers,
// which ssh gives up on only with ServerAliveInterval set) by running the real check against stand-in
// systemctl and pgrep, and runs the rule reload with the real nft, in whatever
// network namespace the test gave the install. A file push runs as the remote would run
// it, with the remote's paths moved under FAKE_ROOT; FAKE_SSH=damage adds a byte to what
// arrives, FAKE_SSH=damage-wait to offload-wait's alone, and FAKE_SSH=cut-wait drops the
// connection pushing offload-wait. A root file dev's install writes lands under
// FAKE_ROOT/dev. go writes fakeBuild to the file a build names with -o.
func fakeInstallTools(t *testing.T) (bin, log string) {
	t.Helper()
	bin, log = t.TempDir(), filepath.Join(t.TempDir(), "calls")
	for name, body := range map[string]string{
		"ssh": `alive=
while [ $# -gt 0 ]; do case $1 in -o) case $2 in ServerAliveInterval=*) alive=1 ;; esac; shift 2 ;; -F) shift 2 ;; *) break ;; esac; done
addr=$1; shift
[ -n "$addr" ] || { echo "ssh: Could not resolve hostname : Name or service not known" >&2; exit 255; }
printf 'ssh %s %s\n' "$addr" "$(echo "$*" | tr '\n' ' ')" >>"$FAKE_LOG"
case $* in
*offload-job-*)
  [ "$FAKE_SSH" = down ] && exit 255
  if [ "$FAKE_SSH" = hung ]; then
    [ -n "$alive" ] && { echo "Timeout, server not responding." >&2; exit 255; }
    sleep 5; exit 0
  fi
  exec sh -c "systemctl() {
  states=
  for a; do case \$a in --state=*) states=\${a#--state=} ;; esac; done
  case \$FAKE_SSH in busy) s=active ;; stopping) s=deactivating ;; *) return 0 ;; esac
  case ,\$states, in *,\$s,*) echo \"offload-job-7.scope loaded \$s running offload-job-7.scope\" ;; esac; }
pgrep() {
  case \$FAKE_SSH in
  launcher) echo '42 bash /usr/local/libexec/offload-launch 192.0.2.50 1000' ;;
  pgrep-fails) return 2 ;;
  *) return 1 ;;
  esac; }
$*" ;;
*"install -m "*" /dev/stdin "*)
  cmd=$(printf '%s' "$*" | sed "s#\(/usr/local/libexec\|/etc/offload\|/etc/systemd/system\)#$FAKE_ROOT\1#g")
  mkdir -p "$FAKE_ROOT/usr/local/libexec" "$FAKE_ROOT/etc/offload" "$FAKE_ROOT/etc/systemd/system"
  case $FAKE_SSH:$cmd in cut-wait:*offload-wait.new*) cat >/dev/null; exit 255 ;; esac
  if [ "$FAKE_SSH" = damage ] || { [ "$FAKE_SSH" = damage-wait ] && case $cmd in *offload-wait.new*) true ;; *) false ;; esac; }; then
    { cat; echo x; } | sh -c "sudo() { \"\$@\"; }; $cmd"
  else
    sh -c "sudo() { \"\$@\"; }; $cmd"
  fi
  exit ;;
"sudo mv -f "*|"sudo rm -f "*)
  sh -c "sudo() { \"\$@\"; }; $(printf '%s' "$*" | sed "s#\(/usr/local/libexec\|/etc/offload\|/etc/systemd/system\)#$FAKE_ROOT\1#g")"
  exit ;;
'sudo nft -f /dev/stdin') exec /usr/sbin/nft -f /dev/stdin ;;
esac
cat >/dev/null`,
		"sudo": `printf 'sudo %s\n' "$*" >>"$FAKE_LOG"
case $* in
'install -D -m 0644 /dev/stdin '*) mkdir -p "$FAKE_ROOT/dev${6%/*}"; cat >"$FAKE_ROOT/dev$6"; exit ;;
esac
[ -t 0 ] || cat >/dev/null`,
		"go": `printf 'go %s\n' "$*" >>"$FAKE_LOG"
if [ "$1" = version ]; then cat "$3"; exit; fi
while [ $# -gt 0 ]; do [ "$1" = -o ] && printf '%s' "$fakeBuild" >"$2"; shift; done`,
		"getent":    `printf 'probe:x:1000:1000::/home/probe:/bin/sh\n'`,
		"dpkg":      `exit 0`,
		"systemctl": `exit 0`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nfakeBuild='"+fakeBuild+"'\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, log
}

// install runs install.sh with the fake tools, under `unshare -rn` so its nft acts on a
// network namespace of the test's own, which pre (nft input) sets up first and whose
// ruleset is printed after the install, past a "--- ruleset" line.
func install(t *testing.T, mode, pre string) (code int, stdout, stderr string, calls []string) {
	t.Helper()
	code, stdout, stderr, calls, _ = installIn(t, mode, pre)
	return code, stdout, stderr, calls
}

// installIn is install, given install.sh's arguments, also returning the directory the
// pushed remote files landed under.
func installIn(t *testing.T, mode, pre string, args ...string) (code int, stdout, stderr string, calls []string, root string) {
	t.Helper()
	root = t.TempDir()
	needNamespaces(t, "-rn", "/usr/sbin/nft list ruleset")
	bin, log := fakeInstallTools(t)
	script := filepath.Join(installFixture(t), "install.sh")
	cmd := exec.Command("unshare", append([]string{"-rn", "sh", "-c",
		`printf '%s' "$1" | /usr/sbin/nft -f /dev/stdin || exit 99; shift; "$@"; rc=$?; echo '--- ruleset'; /usr/sbin/nft list ruleset; exit $rc`,
		"sh", pre, script}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+t.TempDir(), "FAKE_SSH="+mode, "FAKE_LOG="+log, "FAKE_ROOT="+root)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code = exitStatus(t, cmd.Run())
	if code == 99 {
		t.Fatalf("setting up the namespace's rules failed: %s", errOut.String())
	}
	logged, _ := os.ReadFile(log)
	return code, out.String(), errOut.String(), strings.Split(strings.TrimSpace(string(logged)), "\n"), root
}

// needNamespaces skips the test, saying why, where `unshare <flags>` cannot run probe: an
// unprivileged user namespace is refused on some hosts (Ubuntu's AppArmor default).
func needNamespaces(t *testing.T, flags, probe string) {
	t.Helper()
	if out, err := exec.Command("unshare", flags, "sh", "-c", probe).CombinedOutput(); err != nil {
		t.Skipf("needs `unshare %s` able to run %q: %v %s", flags, probe, err, out)
	}
}

const fixtureRemote = "192.0.2.10"

// installFixture supplies a synthetic deployment to the real scripts without
// using or writing the operator's config.
func installFixture(t *testing.T) string {
	t.Helper()
	unit := t.TempDir()
	if err := os.Mkdir(filepath.Join(unit, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"install.sh", "uninstall.sh", "src", "scripts", "deploy", "config/offload-net.nft", "config/remote-local-paths"} {
		source, err := filepath.Abs(filepath.Join("..", path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(unit, path)); err != nil {
			t.Fatal(err)
		}
	}
	conf := "dev 192.0.2.50\nremote server " + fixtureRemote + "\n"
	if err := os.WriteFile(filepath.Join(unit, "config/offload.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	return unit
}

// A job that runs while the launcher and the firewall are replaced reads a half-written
// launcher or runs unfirewalled, so the install refuses, before it changes anything on
// dev or any remote, while a remote has a job or a launcher running, or cannot say.
func TestInstallRefusesWhileARemoteRunsAJobOrCannotSay(t *testing.T) {
	addr := fixtureRemote
	for mode, want := range map[string]string{
		"busy":        "offload: jobs are running on " + addr + "; nothing installed",
		"stopping":    "offload: jobs are running on " + addr + "; nothing installed",
		"launcher":    "offload: jobs are running on " + addr + "; nothing installed",
		"down":        "offload: cannot list jobs on " + addr + "; nothing installed",
		"pgrep-fails": "offload: cannot list jobs on " + addr + "; nothing installed",
		"hung":        "offload: cannot list jobs on " + addr + "; nothing installed",
	} {
		code, _, stderr, calls := install(t, mode, "")
		if code == 0 || !strings.Contains(stderr, want) {
			t.Errorf("%s: exit %d, stderr %q, want non-zero and %q", mode, code, stderr, want)
		}
		for _, c := range calls {
			if !strings.Contains(c, "offload-job-") {
				t.Errorf("%s: the refused install still ran %q", mode, c)
			}
		}
	}
}

// uninstall.sh refuses as install.sh does, before it removes anything, while a remote has
// a job or a launcher running, or cannot say.
func TestUninstallRefusesWhileARemoteRunsAJobOrCannotSay(t *testing.T) {
	addr := fixtureRemote
	script := filepath.Join(installFixture(t), "uninstall.sh")
	for mode, want := range map[string]string{
		"busy":        "offload: jobs are running on " + addr + "; not uninstalling",
		"stopping":    "offload: jobs are running on " + addr + "; not uninstalling",
		"launcher":    "offload: jobs are running on " + addr + "; not uninstalling",
		"down":        "offload: cannot list jobs on " + addr + "; not uninstalling",
		"pgrep-fails": "offload: cannot list jobs on " + addr + "; not uninstalling",
		"hung":        "offload: cannot list jobs on " + addr + "; not uninstalling",
	} {
		bin, log := fakeInstallTools(t)
		cmd := exec.Command(script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+t.TempDir(), "FAKE_SSH="+mode, "FAKE_LOG="+log, "FAKE_ROOT="+t.TempDir())
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if code := exitStatus(t, cmd.Run()); code == 0 || !strings.Contains(errOut.String(), want) {
			t.Errorf("%s: exit %d, stderr %q, want non-zero and %q", mode, code, errOut.String(), want)
		}
		logged, _ := os.ReadFile(log)
		for _, c := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
			if !strings.Contains(c, "offload-job-") {
				t.Errorf("%s: the refused uninstall still ran %q", mode, c)
			}
		}
	}
}

func TestExportOnlyRefusesLegacyWrapperBeforeChanges(t *testing.T) {
	bin, log := fakeInstallTools(t)
	root, home := t.TempDir(), t.TempDir()
	wrapper := filepath.Join(home, ".local/bin/offload")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("legacy wrapper"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(installFixture(t), "install.sh"), "--export-only")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home, "FAKE_SSH=idle", "FAKE_LOG="+log, "FAKE_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "nothing installed") {
		t.Errorf("exit %v, output %q, want a refusal", err, out)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "sudo ") {
		t.Errorf("refused install made changes: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(root, "dev")); !os.IsNotExist(err) {
		t.Errorf("refused install wrote dev files: %v", err)
	}
}

// The synthetic root is the only fsid=0 export; each intended child is explicit.
func TestInstallExportsSyntheticView(t *testing.T) {
	bin, log := fakeInstallTools(t)
	root, home := t.TempDir(), t.TempDir()
	wrapper := filepath.Join(home, ".local/bin/offload")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := "build\t-ldflags=\"-X main.configDir=/fixture/config -X main.scopedExportRoot=/run/offload-export\"\n"
	if err := os.WriteFile(wrapper, []byte(metadata), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(installFixture(t), "install.sh"), "--export-only")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home, "FAKE_SSH=idle", "FAKE_LOG="+log, "FAKE_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	got, err := os.ReadFile(root + "/dev/etc/exports.d/offload.exports")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if !strings.HasPrefix(line, "/run/offload-export") || strings.Contains(line, "crossmnt") {
			t.Errorf("unsafe export %q", line)
		}
	}
	want := []string{
		"/run/offload-export " + fixtureRemote + "(ro,sync,root_squash,no_subtree_check,fsid=0)",
		"/run/offload-export/usr " + fixtureRemote + "(ro,sync,root_squash,subtree_check,fsid=1)",
		"/run/offload-export/etc " + fixtureRemote + "(ro,sync,root_squash,subtree_check,fsid=2)",
		"/run/offload-export/tmp " + fixtureRemote + "(rw,sync,root_squash,subtree_check,fsid=3)",
		"/run/offload-export/var/tmp " + fixtureRemote + "(rw,sync,root_squash,subtree_check,fsid=4)",
		"/run/offload-export/home/probe/code " + fixtureRemote + "(rw,sync,root_squash,subtree_check,fsid=5)",
		"/run/offload-export/home/probe/orca/workspaces " + fixtureRemote + "(rw,sync,root_squash,subtree_check,fsid=6)",
	}
	if string(got) != strings.Join(want, "\n")+"\n" {
		t.Errorf("unexpected exports: %s", got)
	}
	unit, err := os.ReadFile(root + "/dev/etc/systemd/system/nfs-server.service.d/offload.conf")
	if err != nil || !strings.Contains(string(unit), "Requires=offload-export-view.service\nAfter=offload-export-view.service") {
		t.Errorf("NFS ordering: %q, %v", unit, err)
	}
}

// fakeBuild is what the fake go writes as a build's output.
const fakeBuild = "built by the fake go\n"

// remoteFiles is each file install.sh pushes to a remote, by its source under the unit; ""
// is one install.sh builds, which arrives as fakeBuild.
var remoteFiles = map[string]string{
	"/usr/local/libexec/offload-launch":       "offload-launch",
	"/usr/local/libexec/offload-bind":         "",
	"/usr/local/libexec/offload-forward":      "",
	"/usr/local/libexec/offload-wait":         "",
	"/etc/offload/remote-local-paths":         "../config/remote-local-paths",
	"/etc/offload/net.nft":                    "../config/offload-net.nft",
	"/etc/systemd/system/offload-net.service": "../deploy/systemd/offload-net.service",
	"/etc/systemd/system/offload-cut.service": "../deploy/systemd/offload-cut.service",
}

// --export-only changes nfsd under a running job too, so it refuses as a full install does.
func TestExportOnlyInstallRefusesWhileARemoteRunsAJob(t *testing.T) {
	code, _, stderr, calls, _ := installIn(t, "busy", "", "--export-only")
	if code == 0 || !strings.Contains(stderr, "jobs are running on") {
		t.Errorf("exit %d, stderr %q, want a refusal", code, stderr)
	}
	for _, c := range calls {
		if !strings.Contains(c, "offload-job-") {
			t.Errorf("the refused install still ran %q", c)
		}
	}
}

// An argument install.sh does not know (a typo of --export-only, the old --check-config)
// stops it before it reads the config or asks any remote, rather than running a full install.
func TestInstallRefusesAnUnknownArgument(t *testing.T) {
	for _, args := range [][]string{{"--export_only"}, {"--check-config", "x"}, {"--export-only", "x"}, {""}} {
		code, _, stderr, calls, _ := installIn(t, "idle", "", args...)
		if code != 2 || !strings.Contains(stderr, "usage: install.sh [--export-only]") {
			t.Errorf("%q: exit %d, stderr %q, want 2 and the usage", args, code, stderr)
		}
		if len(calls) != 1 || calls[0] != "" {
			t.Errorf("%q: the refused install still ran %q", args, calls)
		}
	}
}

// Each file reaches a remote under a name of its own, is checked against dev's copy and
// renamed over the old one, so a launcher starting mid-install reads the old file or the
// new one whole, never a mix.
func TestInstallReplacesEachRemoteFileByRename(t *testing.T) {
	code, _, stderr, calls, root := installIn(t, "idle", "")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for dest, src := range remoteFiles {
		want := []byte(fakeBuild)
		if src != "" {
			var err error
			if want, err = os.ReadFile(src); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := os.ReadFile(root + dest); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: not pushed whole (%v)", dest, err)
		}
		if _, err := os.Stat(root + dest + ".new"); err == nil {
			t.Errorf("%s.new left behind", dest)
		}
		staged, renamed := false, false
		for _, c := range calls {
			c = strings.Join(strings.Fields(c), " ")
			staged = staged || strings.Contains(c, "/dev/stdin '"+dest+".new' &&")
			renamed = renamed || strings.Contains(c, "sudo mv -f '"+dest+".new' '"+dest+"'")
		}
		if !staged || !renamed {
			t.Errorf("%s was not pushed by rename (calls %q)", dest, calls)
		}
	}
}

// A file that arrives changed in transit is removed, never renamed over the old one, and
// the install fails saying so.
func TestInstallRefusesAFileThatArrivesDamaged(t *testing.T) {
	code, _, stderr, _, root := installIn(t, "damage", "")
	if code == 0 || !strings.Contains(stderr, "/usr/local/libexec/offload-launch.new did not arrive whole; nothing replaced") {
		t.Errorf("exit %d, stderr %q, want a failure naming the damaged file", code, stderr)
	}
	for dest := range remoteFiles {
		for _, p := range []string{dest, dest + ".new"} {
			if _, err := os.Stat(root + p); err == nil {
				t.Errorf("%s exists after a damaged push", p)
			}
		}
	}
}

// A file damaged after others arrived whole replaces none of them: the remote keeps its
// old set, never a new launcher beside an old offload-wait.
func TestInstallReplacesNoFileWhenALaterOneArrivesDamaged(t *testing.T) {
	for _, mode := range []string{"damage-wait", "cut-wait"} {
		t.Run(mode, func(t *testing.T) { replacesNoFile(t, mode) })
	}
}

func replacesNoFile(t *testing.T, mode string) {
	code, _, stderr, _, root := installIn(t, mode, "")
	if code == 0 || !strings.Contains(stderr, "/usr/local/libexec/offload-wait.new did not arrive whole; nothing replaced") {
		t.Errorf("exit %d, stderr %q, want a failure naming the damaged file", code, stderr)
	}
	for dest := range remoteFiles {
		for _, p := range []string{dest, dest + ".new"} {
			if _, err := os.Stat(root + p); err == nil {
				t.Errorf("%s exists after a damaged push", p)
			}
		}
	}
}

// The job firewall is replaced in one nft transaction, never by restarting
// offload-net.service, which deletes the tables before it loads them: on a first install
// and over rules an older install left, the result is exactly the shipped ruleset.
func TestInstallReplacesTheJobFirewallInOneTransaction(t *testing.T) {
	needNamespaces(t, "-rn", "/usr/sbin/nft list ruleset")
	shipped, err := exec.Command("unshare", "-rn", "sh", "-c", "/usr/sbin/nft -f ../config/offload-net.nft && /usr/sbin/nft list ruleset").CombinedOutput()
	if err != nil {
		t.Fatalf("loading the shipped ruleset: %v %s", err, shipped)
	}
	older := "table inet offload_job {\n chain input { type filter hook input priority -10; policy accept; tcp dport 9 drop; }\n chain gone { }\n}\n"
	for name, pre := range map[string]string{"first install": "", "over older rules": older} {
		code, stdout, stderr, calls := install(t, "idle", pre)
		_, ruleset, _ := strings.Cut(stdout, "--- ruleset\n")
		if code != 0 || ruleset != string(shipped) {
			t.Errorf("%s: exit %d (%s), ruleset\n%s\nwant\n%s", name, code, stderr, ruleset, shipped)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "ssh ") && (strings.Contains(c, "restart") || strings.Contains(c, "nft delete")) {
				t.Errorf("%s: %q leaves the jobs without a firewall for a moment", name, c)
			}
		}
	}
}

// A config with its remotes removed still uninstalls dev's export, which would otherwise go
// on serving / to the old addresses.
func TestUninstallRemovesTheExportWhenTheConfigListsNoRemote(t *testing.T) {
	// A remote line with no address is no remote to ask about jobs.
	for _, conf := range []string{"dev 192.0.2.50\n", "dev 192.0.2.50\nremote server\n"} {
		t.Run(conf, func(t *testing.T) { uninstallsWith(t, conf) })
	}
}

func uninstallsWith(t *testing.T, conf string) {
	unit := t.TempDir()
	for _, f := range []string{"uninstall.sh", "scripts/remotes.sh"} {
		body, err := os.ReadFile("../" + f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(unit, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unit, f), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(unit+"/config", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit+"/config/offload.conf", []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, log := fakeInstallTools(t)
	cmd := exec.Command(unit + "/uninstall.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+t.TempDir(), "FAKE_SSH=idle", "FAKE_LOG="+log, "FAKE_ROOT="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "sudo rm -f /etc/exports.d/offload.exports") {
		t.Errorf("the export was not removed; calls:\n%s", logged)
	}
}

// exitStatus is the exit code err reports for a command that ran.
func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatal(err)
	return 0
}

func TestExportOnlyChecksUnambiguousScopedMetadata(t *testing.T) {
	for _, c := range []struct {
		name, flags string
		allow       bool
	}{
		{"first", "-X main.scopedExportRoot=/run/offload-export -X main.configDir=/fixture", true},
		{"duplicate empty", "-X main.configDir=/fixture -X main.scopedExportRoot=/run/offload-export -X main.scopedExportRoot=", false},
		{"duplicate same", "-X main.scopedExportRoot=/run/offload-export -X main.scopedExportRoot=/run/offload-export", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, log := fakeInstallTools(t)
			root, home := t.TempDir(), t.TempDir()
			wrapper := filepath.Join(home, ".local/bin/offload")
			if err := os.MkdirAll(filepath.Dir(wrapper), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(wrapper, []byte("build\t-ldflags=\""+c.flags+"\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(installFixture(t), "install.sh"), "--export-only")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home, "FAKE_SSH=idle", "FAKE_LOG="+log, "FAKE_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.allow {
				t.Errorf("exit %v output %s, want allow %v", err, out, c.allow)
			}
		})
	}
}
