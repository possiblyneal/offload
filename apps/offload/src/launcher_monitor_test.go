package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// launchUntilExit runs the launcher's host half up to an early exit, in user, mount and
// pid namespaces of the test's own: temporary directories stand over /run and /var/lib,
// and stand-ins replace systemd, nft and timeout (which cuts at 0.5 s), so nothing on this machine is touched. start is
// the body of systemd-run's stand-in, which the launcher calls to start the job's scope,
// and systemctl is the body of systemctl's stand-in. Once systemd-run's stand-in has
// started, say, when set, is written to the control channel as the wrapper would, and
// signal, when set, is sent to the launcher (a say holding `|` is written in two parts,
// the second 1.5 s after the first, across the monitor's read timeout); the status it then reports is in out. env is
// added to the launcher's environment. It returns what the namespace's shell saw after the launcher exited: whether
// slot 1 was free at once, and the processes still alive a moment later.
func launchUntilExit(t *testing.T, start, signal, systemctl, say string, env ...string) (code int, out string) {
	t.Helper()
	needNamespaces(t, "-rmpf", "mount -t proc proc /proc")
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Fatal(err)
	}
	realTimeout, err := exec.LookPath("timeout")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := filepath.Abs("offload-launch")
	if err != nil {
		t.Fatal(err)
	}
	bin, run, varlib := t.TempDir(), t.TempDir(), t.TempDir()
	// The signal and say wait for $FAKE_DIR/starting; a start that must reach a point
	// first writes it itself.
	const starting = `: >"$FAKE_DIR/starting"`
	if !strings.Contains(start, starting) {
		start = starting + "; " + start
	}
	for name, body := range map[string]string{
		"systemctl":   systemctl,
		"nft":         "exit 0",
		"systemd-run": start,
		// Records the cap the launcher asks for, then cuts at a tenth of it, so a command
		// that hangs ends the test in half a second rather than five.
		"timeout": `echo "$1" >>"$FAKE_DIR/timeout"; shift; exec ` + realTimeout + ` 0.5 "$@"`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const id = "0123abcd"
	cmd := exec.Command("unshare", "-rmpf", "--mount-proc", "sh", "-c", `
mount --bind "$1" /run && mount --bind "$2" /var/lib && mkdir /var/lib/offload || exit 99
"$3" 10.0.0.1 1000 1000 1000 / '' '' '' -- true &
pid=$!
until [ -p /run/offload-ctl/`+id+`/in ] || ! kill -0 $pid 2>/dev/null; do sleep 0.01; done
echo attached >/run/offload-ctl/`+id+`/in
exec 9<>/run/offload-ctl/`+id+`/out
if [ -n "$4$6" ]; then
  until [ -e "$FAKE_DIR/starting" ] || ! kill -0 $pid 2>/dev/null; do sleep 0.01; done
  case $6 in
    '') ;;
    *'|'*) printf %s "${6%%|*}" >/run/offload-ctl/`+id+`/in; sleep 1.5; echo "${6#*|}" >/run/offload-ctl/`+id+`/in ;;
    *) echo "$6" >/run/offload-ctl/`+id+`/in ;;
  esac
  [ -z "$4" ] || kill -"$4" $pid
fi
wait $pid; rc=$?
if [ -n "$4$6" ]; then
  echo "$("$7" 15 sh -c 'while read -r l; do case $l in status*) echo "$l"; exit;; esac; done' <&9)"
fi
if "$5" -n /var/lib/offload/slots/1 true; then echo 'slot free'; else echo 'slot held'; fi
for _ in $(seq 80); do
  left=$(pgrep -af '^bash .*/offload-launch ')
  [ -z "$left" ] && break
  sleep 0.1
done
echo "left: $left"
[ -d /run/offload-ctl/`+id+` ] && echo 'ctl kept'
exit $rc`, "sh", run, varlib, launch, signal, realFlock, say, realTimeout)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DIR="+t.TempDir(),
		"OFFLOAD_CTL="+id, "OFFLOAD_SLOTS=1")
	cmd.Env = append(cmd.Env, env...)
	b, err := cmd.CombinedOutput()
	if code = exitStatus(t, err); code == 99 {
		t.Fatalf("setting up the namespace failed: %s", b)
	}
	return code, string(b)
}

// The background monitor must neither hold the job's slot nor outlive the launcher: when the
// job's scope fails to start, and on a signal while the launcher waits for the job, the launcher
// exits with the slot free at once and no monitor left pinging. A launcher stopped by a
// signal tells the wrapper what became of the job before it goes: never run while the job
// had not passed its wait for `go`, else the launcher lost. A launcher killed outright
// runs no cleanup: its monitor notices, does the cleanup, tells the wrapper the job never
// ran, and ends; if the job's scope will not die, the job may have passed its wait for
// `go`, so the wrapper is told the launcher was lost rather than that the job never ran.
func TestLauncherExitFreesItsSlotAndEndsItsMonitor(t *testing.T) {
	const released = ": >/run/offload-ctl/0123abcd/released; "
	for name, c := range map[string]struct {
		start, signal, systemctl string
		code                     int
		status                   string
	}{
		"scope start fails":     {"exit 1", "", "exit 1", 1, ""},
		"signal":                {"sleep 1; exit 0", "TERM", "exit 1", 143, "status unrun\n"},
		"hangup, released":      {released + "sleep 1; exit 0", "HUP", "exit 1", 143, "status launcher-lost\n"},
		"killed":                {"sleep 1; exit 0", "KILL", "exit 1", 137, "status unrun\n"},
		"killed, scope undying": {"sleep 1; exit 0", "KILL", `[ "$1" = is-active ]`, 137, "status launcher-lost\n"},
	} {
		code, out := launchUntilExit(t, c.start, c.signal, c.systemctl, "")
		if code != c.code || !strings.Contains(out, "slot free") || !strings.Contains(out, "left: \n") ||
			c.status != "" && (!strings.Contains(out, c.status) || strings.Contains(out, "ctl kept")) {
			t.Errorf("%s: exit %d (want %d), output:\n%s", name, code, c.code, out)
		}
	}
}

// The cleanup kills the scope, waits on its stop at most 5 s (dev down can hold a dying
// process for good), then resets it, whether the stop returns or hangs.
func TestTheCleanupCapsTheScopesStop(t *testing.T) {
	for name, stop := range map[string]string{"returns": "exit 0", "hangs": "exec sleep 30"} {
		calls := filepath.Join(t.TempDir(), "calls")
		systemctl := `echo "$1" >>"$CALLS"; case $1 in stop) [ -e "$FAKE_DIR/timeout" ] && cat "$FAKE_DIR/timeout" >>"$CALLS"; ` + stop + ` ;; esac; exit 1`
		code, out := launchUntilExit(t, "exit 1", "", systemctl, "", "CALLS="+calls)
		b, _ := os.ReadFile(calls)
		if code != 1 || !strings.Contains(out, "slot free") || !strings.Contains(string(b), "kill\nstop\n5\nreset-failed\n") {
			t.Errorf("stop %s: exit %d, systemctl calls %q, output:\n%s", name, code, b, out)
		}
	}
}

// A monitor that dies alone, without marking an abort, leaves the job with no watchdog and
// no channel: the launcher stops the job and, once the job may have passed its wait for
// `go`, tells the wrapper the launcher was lost, never what the stopped job exited with;
// before that, that the job never ran, so the wrapper may run it elsewhere. The stand-in
// systemd-run kills the launcher's other child, the monitor. A launcher signalled while it
// stops that job (here by the stand-in systemctl's kill) tells the wrapper the same,
// though its monitor is already gone.
func TestAMonitorLostAloneIsReportedAsTheLauncherLost(t *testing.T) {
	const ctl = "/run/offload-ctl/0123abcd/"
	const killMonitor = `l=$(pgrep -P 1 -f offload-launch); a=" $$ "; p=$$
while [ "$p" != 1 ]; do p=$(cut -d' ' -f4 /proc/$p/stat); a="$a$p "; done
for c in $(pgrep -P "$l"); do case $a in *" $c "*) ;; *) kill -KILL "$c" ;; esac; done
sleep 1; `
	const undying = `[ "$1" = is-active ]`
	const signalOnStop = `case $1 in kill) [ -e "$FAKE_DIR/sent" ] || { : >"$FAKE_DIR/sent"; kill -TERM $(pgrep -P 1 -f offload-launch); } ;; esac; exit 1`
	for name, c := range map[string]struct {
		start, systemctl, status string
		code                     int // 0: any
	}{
		"released":     {": >" + ctl + "released; echo 1 >" + ctl + "started; " + killMonitor + "exit 0", undying, "status launcher-lost\n", 0},
		"not released": {killMonitor + "exit 0", undying, "status unrun\n", 0},
		"released, launcher signalled": {": >" + ctl + "released; echo 1 >" + ctl + "started; " + killMonitor + "sleep 2; exit 0",
			signalOnStop, "status launcher-lost\n", 143},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out := launchUntilExit(t, c.start, "", c.systemctl, "ping")
			if !strings.Contains(out, c.status) || c.code != 0 && code != c.code {
				t.Errorf("want %q and exit %d; exit %d, output:\n%s", c.status, c.code, code, out)
			}
		})
	}
}

// The grace period's end marks the job graced while its scope is active, and the scope
// outlives the job's own end by the launcher's helpers in it. A job that ended by itself
// before the grace kill took it keeps its own exit; only one the kill took (SIGKILL) is
// reported graced. Here the grace is 0, so the monitor marks the job graced at once, and
// the scope never dies of it.
func TestAJobThatEndsByItselfIsNotReportedGraced(t *testing.T) {
	const released = ": >/run/offload-ctl/0123abcd/released; echo 1 >/run/offload-ctl/0123abcd/started; "
	for name, c := range map[string]struct {
		start, status string
	}{
		"exits 0":      {released + "sleep 1; exit 0", "status exit 0\n"},
		"exits 3":      {released + "sleep 1; exit 3", "status exit 3\n"},
		"killed by it": {released + "sleep 1; kill -KILL $$", "status graced\n"},
	} {
		_, out := launchUntilExit(t, c.start, "", `[ "$1" = is-active ]`, "sig TERM", "OFFLOAD_GRACE=0")
		if !strings.Contains(out, c.status) {
			t.Errorf("%s: want %q, output:\n%s", name, c.status, out)
		}
	}
}

// The watchdog's mark, like the grace's, stands only when its stop's SIGKILL ended the job:
// one that outlived the silence and then ended by itself keeps its own exit. The stand-in
// systemctl stops nothing, so each job runs past the 10 s silence to its own end.
func TestAJobThatEndsByItselfIsNotReportedWatchdog(t *testing.T) {
	const released = ": >/run/offload-ctl/0123abcd/released; echo 1 >/run/offload-ctl/0123abcd/started; "
	for name, c := range map[string]struct {
		start, status string
	}{
		"exits 0":      {released + "sleep 13; exit 0", "status exit 0\n"},
		"killed by it": {released + "sleep 13; kill -KILL $$", "status watchdog\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, out := launchUntilExit(t, c.start, "", `[ "$1" = is-active ]`, "ping")
			if !strings.Contains(out, c.status) {
				t.Errorf("want %q, output:\n%s", c.status, out)
			}
		})
	}
}

// A launcher refused leaves no job dir in /run behind: one refused before the job dir is
// made, and one whose control channel cannot be made after it (a file already holds the
// channel's path), which the cleanup trap must remove.
func TestALauncherRefusedLeavesNoJobDir(t *testing.T) {
	needNamespaces(t, "-rm", "true")
	launch, err := filepath.Abs("offload-launch")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, ctl, block, want string }{
		{"before the job dir", "", "", "OFFLOAD_CTL is not set"},
		{"after the job dir", "0123abcd", "0123abcd", "mkdir"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, varlib := t.TempDir(), t.TempDir()
			if c.block != "" {
				if err := os.Mkdir(filepath.Join(run, "offload-ctl"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(run, "offload-ctl", c.block), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("unshare", "-rm", "sh", "-c",
				`mount --bind "$1" /run && mount --bind "$2" /var/lib || exit 99; exec "$3" 10.0.0.1 1000 1000 1000 / '' '' '' -- true`,
				"sh", run, varlib, launch)
			cmd.Env = append(os.Environ(), "OFFLOAD_CTL="+c.ctl)
			out, err := cmd.CombinedOutput()
			left, _ := filepath.Glob(filepath.Join(run, "offload-job.*"))
			if code := exitStatus(t, err); code == 0 || code == 99 || !strings.Contains(string(out), c.want) || len(left) > 0 {
				t.Errorf("exit %d (want a refusal), left %q, output: %s", code, left, out)
			}
		})
	}
}

// A step of the job half's last exec chain (nsenter, unshare, setpriv, offload-wait) that fails
// past the ERR trap exits with its own code: with no record that the job started, the
// wrapper is told the job did not run, never that the job exited so.
func TestAnExitBeforeTheJobStartsIsNotTheJobs(t *testing.T) {
	const released = ": >/run/offload-ctl/0123abcd/released; "
	code, out := launchUntilExit(t, released+"sleep 1; exit 127", "", `[ "$1" = is-active ]`, "ping")
	// The launcher exits with the code it reported: another reads to the wrapper as a cut stream.
	if code != 125 || !strings.Contains(out, "status exit 125\n") || !strings.Contains(out, "starting the job failed (exit 127); the job did not run") {
		t.Errorf("want exit and status 125 and the reason; exit %d, output:\n%s", code, out)
	}
}

// A job signalled before it started ends by that signal, as a local run stopped then would,
// whatever code the exec chain into it ended with.
func TestASignalBeforeTheJobStartsEndsItBySignal(t *testing.T) {
	const ctl = "/run/offload-ctl/0123abcd/"
	start := ": >" + ctl + "released; echo INT >>" + ctl + "abort-sig; : >" + ctl + "abort; sleep 1; exit 1"
	code, out := launchUntilExit(t, start, "", `[ "$1" = is-active ]`, "ping")
	// The launcher exits with the code it reported: another reads to the wrapper as a cut stream.
	if code != 130 || !strings.Contains(out, "status signal 2\n") {
		t.Errorf("want exit 130 and status signal 2; exit %d, output:\n%s", code, out)
	}
}

// Once the job and all it left have ended (offload-wait writes 2 after the 1 of its start),
// the grace's end kills nothing: what remains is the drain of its output to a slow reader,
// which a local run waits out. The stand-in systemctl records a kill, which the job here
// dies of, as offload-wait would mid-drain.
func TestTheGraceSparesTheDrainOfAJobThatEnded(t *testing.T) {
	const ctl = "/run/offload-ctl/0123abcd/"
	start := ": >" + ctl + "released; printf 12 >" + ctl + `started; : >"$FAKE_DIR/starting"; sleep 1.5; [ -e "$FAKE_DIR/killed" ] && kill -KILL $$; exit 0`
	systemctl := `case $1 in is-active) exit 0 ;; kill) : >"$FAKE_DIR/killed" ;; esac; exit 0`
	_, out := launchUntilExit(t, start, "", systemctl, "sig TERM", "OFFLOAD_GRACE=0")
	if !strings.Contains(out, "status exit 0\n") {
		t.Errorf("want status exit 0, output:\n%s", out)
	}
}

// An OOM kill systemd records only after the job half has exited is still reported as one.
func TestAnOOMKillRecordedLateIsReported(t *testing.T) {
	const ctl = "/run/offload-ctl/0123abcd/"
	start := ": >" + ctl + "released; echo 1 >" + ctl + "started; sleep 1; echo 9 >" + ctl + `signal; (sleep 0.3; : >"$FAKE_DIR/oom") & exit 137`
	systemctl := `case $1 in is-active) exit 0 ;; show) [ -e "$FAKE_DIR/oom" ] && echo oom-kill ;; esac; exit 0`
	_, out := launchUntilExit(t, start, "", systemctl, "ping")
	if !strings.Contains(out, "status oom-kill\n") {
		t.Errorf("want status oom-kill, output:\n%s", out)
	}
}

// Once the job half is reaped its end is known: a HUP, INT or TERM reaching the launcher
// then (sshd going, a shutdown) is not the launcher lost while the job ran. The launcher
// reports the job's own status, finishes its cleanup and exits with the job's code, which
// the wrapper checks against that status. The stand-in systemctl signals the launcher from
// inside the report (`show`, as final_status asks whether the job was OOM-killed) or the
// cleanup (`kill`, whose wait for the scope to go then runs to its 5 s bound).
func TestASignalAfterTheJobEndedKeepsItsStatus(t *testing.T) {
	const ctl = "/run/offload-ctl/0123abcd/"
	const launcher = `$(pgrep -P 1 -f offload-launch)`
	for name, systemctl := range map[string]string{
		"reporting":   `case $1 in show) kill -TERM ` + launcher + `; sleep 0.2 ;; esac; exit 1`,
		"cleaning up": `case $1 in kill) kill -HUP ` + launcher + `; sleep 0.2 ;; is-active) exit 0 ;; esac; exit 1`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out := launchUntilExit(t, ": >"+ctl+"released; echo 1 >"+ctl+"started; sleep 1; exit 3", "", systemctl, "ping")
			if code != 3 || !strings.Contains(out, "status exit 3\n") || strings.Contains(out, "ctl kept") || !strings.Contains(out, "left: \n") {
				t.Errorf("want exit and status 3 and the cleanup done; exit %d, output:\n%s", code, out)
			}
		})
	}
}

// Other programs create and remove /tmp entries while the launcher sweeps /tmp for stale
// forward sockets; an entry vanishing mid-sweep must not end the launcher before it starts
// the job's scope. Each launch here reaches the scope's start (which then fails).
func TestTheSocketSweepSurvivesTmpChurn(t *testing.T) {
	stop := make(chan struct{})
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A socket the sweep's glob matches, gone a moment later.
			if l, err := net.Listen("unix", filepath.Join("/tmp", fmt.Sprintf("offload-churn-%d.sock", os.Getpid()))); err == nil {
				l.Close()
			}
		}
	}()
	defer func() { close(stop); <-churned }()
	for i := range 5 {
		code, out := launchUntilExit(t, "echo reached-scope >&2; exit 1", "", "exit 1", "")
		if code != 1 || !strings.Contains(out, "reached-scope") {
			t.Fatalf("launch %d: exit %d, output:\n%s", i, code, out)
		}
	}
}

// The sweep removes a forward socket two minutes old that nothing listens on, and keeps a
// younger one (sshd may not have listened on it yet) and an old one something listens on.
func TestTheSocketSweepRemovesOnlyOldUnlistenedSockets(t *testing.T) {
	sock := func(name string) string {
		return filepath.Join("/tmp", fmt.Sprintf("offload-sweep-%d-%s.sock", os.Getpid(), name))
	}
	old := time.Now().Add(-3 * time.Minute)
	made := map[string]net.Listener{}
	for _, name := range []string{"stale", "young", "live"} {
		l, err := net.Listen("unix", sock(name))
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		made[name] = l
		t.Cleanup(func() { l.Close(); os.Remove(sock(name)) })
	}
	made["stale"].Close()
	made["young"].Close()
	for _, name := range []string{"stale", "live"} {
		if err := os.Chtimes(sock(name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := launchUntilExit(t, "echo reached-scope >&2; exit 1", "", "exit 1", ""); code != 1 || !strings.Contains(out, "reached-scope") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	for name, want := range map[string]bool{"stale": false, "young": true, "live": true} {
		if _, err := os.Lstat(sock(name)); (err == nil) != want {
			t.Errorf("%s socket: present %v, want %v", name, err == nil, want)
		}
	}
}

// A record the wrapper's write splits across the monitor's 1 s read timeout is still acted
// on: the part read before the timeout is carried over, not dropped. Here a `sig TERM`
// arrives as `sig T` then `ERM`; acted on, it marks the job graced before the job kills
// itself 3 s in; dropped, the job's own SIGKILL would be reported instead.
func TestARecordTornAcrossTheMonitorsTimeoutIsActedOn(t *testing.T) {
	const released = ": >/run/offload-ctl/0123abcd/released; "
	_, out := launchUntilExit(t, released+"sleep 3; kill -KILL $$", "", `[ "$1" = is-active ]`, "sig T|ERM", "OFFLOAD_GRACE=0")
	if !strings.Contains(out, "status graced\n") {
		t.Errorf("want status graced, output:\n%s", out)
	}
}

// The bridge relays the launcher's records to the wrapper with a 5 s read timeout; a
// status written in two parts across it reaches the wrapper whole, and ends the bridge.
func TestTheBridgeCarriesARecordTornAcrossItsTimeout(t *testing.T) {
	needNamespaces(t, "-rm", "mount -t tmpfs tmpfs /run")
	launch, err := filepath.Abs("offload-launch")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rm", "sh", "-c", `
mount -t tmpfs tmpfs /run && mkdir -p /run/offload-ctl/ab && cd /run/offload-ctl/ab &&
  mkfifo in out || exit 99
exec 8<>in 9<>out
sleep 8 | timeout 20 "$1" --control ab &
pid=$!
printf 'hb\nstatus ex' >&9; sleep 6; echo 'it 0' >&9
wait $pid`, "sh", launch)
	b, err := cmd.Output()
	if code := exitStatus(t, err); code != 0 || string(b) != "hb\nstatus exit 0\n" {
		t.Errorf("exit %d, output %q; want 0 and hb then status exit 0", code, b)
	}
}
