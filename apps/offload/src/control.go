package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The control channel is a second ssh session, apart from the job's stdout and
// stderr. The launcher writes one record per line on it:
//
//	hb                  the launcher is alive (every heartbeatEvery)
//	ready               the job is set up and waits for `go`; it runs only after one
//	started             the job is about to be exec'd
//	noslot              the remote has no free slot: nothing ran
//	cpu <ms>            CPU the job's processes used, just before the status
//	status exit <n>     the job exited with code n
//	status signal <n>   the job died by signal n (or one arrived before it started)
//	status oom-kill     the job's scope was killed for exceeding MemoryMax
//	status unrun        the job never passed the wait for `go`: it ran nowhere, whatever
//	                    the wrapper sent (a late `go` reaches a launcher that gave up)
//	status watchdog     the launcher stopped the job after hearing nothing from the wrapper
//	status launcher-lost the launcher died while the job ran; the job's scope is stopped
//	status graced       the job outlived the grace period after a forwarded signal and
//	                    was killed
//	offset <out|err> <n> where the job left an output file it wrote itself (stdioPlan)
//
// and the wrapper writes `go` (once, on `ready`), `ping` (its heartbeat),
// `sig <INT|TERM|HUP|QUIT|USR1|USR2|TSTP|CONT>` (TSTP and CONT suspend and resume the job,
// around the wrapper's own Ctrl-Z stop; USR1 and USR2 only reach the job, which keeps
// running, as a local run's command would) and `eow <out|err>` (the caller's reader of
// that stream is gone: offload-wait closes the job's pipe for it, and the job runs on). A launcher that gets no `go` tears the job down unrun, so the
// wrapper may fall back to a local run only while it has not sent `go`: from then on the
// job may be running, whatever it has heard. Fallback and the exit code come from these
// records alone, never from ssh's own exit code.
var (
	heartbeatEvery = 2 * time.Second
	silenceLimit   = 10 * time.Second // peer silent this long after its first record: gone
	// setupLimit bounds the launcher's setup, heartbeats and all: a mount that hangs
	// keeps the launcher beating without ever reaching `ready`. Nothing has run by then,
	// so the job runs here; closing the control channel makes the launcher give up.
	setupLimit = 60 * time.Second
)

const (
	connectLimit = 30 * time.Second // before its first record: gone
	drainWait    = 2 * time.Second  // after the job's ssh ended before `go`, for the records still in flight
	flushWait    = 5 * time.Second  // with no status, for the job's last output
)

// controlState is what the launcher has reported so far.
type controlState struct {
	ready   bool // the launcher asked for `go`
	goSent  bool // the wrapper sent it: the job may run from here
	started bool
	noSlot  bool
	cpuMs   int64
	cpuSeen bool
	// offsets is where the job left each output file it wrote itself (stdout, stderr), as
	// `offset out|err <n>` reports it, where offsetSeen.
	offsets    [2]int64
	offsetSeen [2]bool
	status     string // "exit", "oom-kill", "unrun", "watchdog", "launcher-lost", or "" while none has arrived
	// stdinSpent is whether the caller's stdin began flowing to the remote (--stdin, at
	// `go`): a local run would no longer see all of it.
	stdinSpent bool
	code       int // the exit code, 128+signal for a death by signal, when status is "exit"
	signal     int // the signal the job died by, or 0
}

// wantGo is whether the wrapper owes the launcher its `go` now: never after it forwarded
// a signal (sig != 0), which already cancelled the job.
func (s *controlState) wantGo(sig int) bool { return s.ready && !s.goSent && sig == 0 }

// mayHaveRun is whether the job could be running on the remote: `go` went out, or the
// launcher says it has started (which it does only after hearing `go`).
func (s *controlState) mayHaveRun() bool { return s.goSent || s.started }

// feed reads one record. Unknown lines, and a status before the job may have run, are
// ignored.
func (s *controlState) feed(line string) {
	f := strings.Fields(line)
	switch {
	case len(f) == 1 && f[0] == "ready":
		s.ready = true
	case len(f) == 1 && f[0] == "started":
		s.started = true
	case len(f) == 1 && f[0] == "noslot":
		s.noSlot = true
	case len(f) == 2 && f[0] == "status" && f[1] == "unrun":
		s.status = "unrun"
	case !s.mayHaveRun():
	case len(f) == 3 && f[0] == "offset" && (f[1] == "out" || f[1] == "err"):
		if n, err := strconv.ParseInt(f[2], 10, 64); err == nil && n >= 0 {
			i := map[string]int{"out": 0, "err": 1}[f[1]]
			s.offsets[i], s.offsetSeen[i] = n, true
		}
	case len(f) == 2 && f[0] == "cpu":
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			s.cpuMs, s.cpuSeen = n, true
		}
	case len(f) == 3 && f[0] == "status" && f[1] == "exit":
		if n, err := strconv.Atoi(f[2]); err == nil {
			s.status, s.code = "exit", n
		}
	case len(f) == 3 && f[0] == "status" && f[1] == "signal":
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 && n < 65 {
			s.status, s.code, s.signal = "exit", 128+n, n
		}
	case len(f) == 2 && f[0] == "status" && (f[1] == "oom-kill" || f[1] == "watchdog" || f[1] == "launcher-lost" || f[1] == "graced"):
		s.status = f[1]
	}
}

// outcome is what the wrapper does once the control channel has said all it will.
type outcome struct {
	fallback bool   // `go` was never sent: run the command here
	code     int    // the wrapper's exit code
	signal   int    // the signal the wrapper dies by instead of exiting with code, or 0
	message  string // for stderr; empty when the job simply ended
	noSlot   bool   // the fallback is because the remote had no free slot
	cpuMs    int64  // CPU the job used on the remote, when cpuKnown
	cpuKnown bool
	// offsets is where the job left each output file it wrote itself, by fd-1, where
	// offsetSeen; ran is whether it may have run (stdioPlan.settle).
	offsets    [2]int64
	offsetSeen [2]bool
	ran        bool
	// sweep is whether the job may have been killed with files open, its scope now
	// stopped: dev may hold silly-rename files nothing will remove (see sweep.go).
	sweep bool
}

// decide turns the control records into the wrapper's behaviour. sig is the signal
// number the wrapper forwarded, or 0.
func decide(s controlState, sig int) outcome {
	switch {
	case (!s.mayHaveRun() || s.status == "unrun") && sig != 0:
		return outcome{code: 128 + sig, signal: sig}
	case s.status == "unrun" && s.stdinSpent:
		return outcome{code: 255, message: "the remote gave up on the job before it ran, but part of --stdin was " +
			"already sent to it, so it cannot run here on the rest; nothing ran: run it again"}
	case s.status == "unrun":
		return outcome{fallback: true}
	case s.noSlot:
		return outcome{fallback: true, noSlot: true}
	case !s.mayHaveRun():
		return outcome{fallback: true}
	case s.status == "exit":
		return outcome{code: s.code, signal: s.signal, cpuMs: s.cpuMs, cpuKnown: s.cpuSeen, sweep: s.code >= 128}
	case s.status == "oom-kill":
		return outcome{code: 128 + int(syscall.SIGKILL), signal: int(syscall.SIGKILL), sweep: true, message: "the job was killed by the remote's out-of-memory killer " +
			"(scope Result=oom-kill, MemoryMax); its scope is stopped, so no more writes will land on dev. Run it locally."}
	case s.status == "graced":
		// The remote killed the job grace seconds after the forwarded signal; it dies by
		// that signal here, as it would have locally had it obeyed.
		if sig == 0 {
			sig = int(syscall.SIGKILL)
		}
		return outcome{code: 128 + sig, signal: sig, cpuMs: s.cpuMs, cpuKnown: s.cpuSeen, sweep: true, message: "the job was still running " +
			"after the grace period that followed the signal, so the remote killed it; it may have written part of its output to dev"}
	case s.status == "watchdog":
		return outcome{code: 255, sweep: true, message: "the remote stopped the job after hearing nothing from this wrapper for " +
			strconv.Itoa(int(silenceLimit/time.Second)) + " s (was the wrapper stopped?); it may have written part of its output to dev"}
	case s.status == "launcher-lost":
		return outcome{code: 255, sweep: true, message: "the remote's launcher died while the job ran; the job is stopped, " +
			"and may have written part of its output to dev"}
	}
	code := 255
	if sig != 0 {
		code = 128 + sig
	}
	return outcome{code: code, signal: sig, message: "lost contact with the remote after the job started; writes may still land on dev " +
		"until the remote's watchdog stops the job (within " + strconv.Itoa(int(silenceLimit/time.Second)) +
		" s) and its NFS client flushes"}
}

// supervise runs the job's ssh session and its control session, forwards signals and
// heartbeats over the control channel, and returns what happened. It never runs the
// job's command itself; the caller falls back when outcome.fallback is set. onGo, nil
// unless the caller's stdin follows `go` (--stdin), runs once, as `go` is sent.
//
// Both ssh clients get process groups of their own, so a Ctrl-C or a group kill aimed at
// the wrapper reaches only the wrapper, which forwards it over the control channel.
func supervise(jobSSH, ctlSSH *exec.Cmd, onGo func()) (outcome, error) {
	for _, c := range []*exec.Cmd{jobSSH, ctlSSH} {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// The caller's reader gone (`| head -1`) shows only here: ssh swallows its own EPIPE,
	// and the remote's writes land in sshd's buffer, so the job would never get the
	// SIGPIPE it would have locally. Caught, SIGPIPE leaves the wrapper alive to say so:
	// `eow out` or `eow err` has offload-wait close that stream's relay, so the job's next
	// write to it fails as it would have here, and the job runs on as a local one would.
	// A pipe is watched for its reader's end as well, so the job's next write fails rather
	// than the one after the wrapper's next write of the job's output.
	pipes := make(chan os.Signal, 1)
	stopPipes := notifyUnignored(pipes, syscall.SIGPIPE)
	defer stopPipes()
	readerGone := make(chan string, 2)
	var readerLeft atomic.Bool
	var writers []*pipeWriter
	for _, w := range []struct {
		w      *io.Writer
		stream string
	}{{&jobSSH.Stdout, "out"}, {&jobSSH.Stderr, "err"}} {
		if *w.w != nil {
			p := &pipeWriter{w: *w.w, gone: func() { readerGone <- w.stream }, left: func() { readerLeft.Store(true) }}
			writers = append(writers, p)
			if f, ok := p.w.(*os.File); ok {
				stop := watchReader(f, p.close)
				defer stop()
			}
			*w.w = p
		}
	}
	ctlIn, err := ctlSSH.StdinPipe()
	if err != nil {
		return outcome{}, err
	}
	ctlOut, err := ctlSSH.StdoutPipe()
	if err != nil {
		return outcome{}, err
	}
	if err := ctlSSH.Start(); err != nil {
		return outcome{}, err
	}
	if err := jobSSH.Start(); err != nil {
		ctlSSH.Process.Kill()
		ctlSSH.Wait()
		return outcome{}, err
	}

	sigs := make(chan os.Signal, 4)
	stop := notifyUnignored(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTSTP)
	defer stop()

	lines := make(chan string)
	go func() {
		// A record the session ended inside, with no newline, is dropped: `status exit 13` cut
		// from `status exit 137` would be a wrong exit, where none is lost contact.
		r := bufio.NewReader(ctlOut)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				break
			}
			lines <- strings.TrimSuffix(l, "\n")
		}
		close(lines)
	}()
	jobDone := make(chan struct{})
	jobEnded := jobDone // jobDone is niled once seen; this stays
	var jobErr error    // jobSSH.Wait's result, valid once jobEnded is closed
	go func() { jobErr = jobSSH.Wait(); close(jobDone) }()

	send := func(s string) { io.WriteString(ctlIn, s+"\n") }
	// suspend is Ctrl-Z: the remote job stops, then the wrapper does, as the shell expects.
	// The launcher pauses its silence watchdog from `sig TSTP` until `sig CONT`, which goes
	// out when the shell resumes the wrapper. It reports whether s was Ctrl-Z, and adds the
	// time stopped to stopped, which is not the launcher's setup time.
	var stopped time.Duration
	suspend := func(s os.Signal) bool {
		if s != syscall.SIGTSTP {
			return false
		}
		if groupOrphaned() {
			return true
		}
		send("sig TSTP")
		at := time.Now()
		suspendSelf()
		stopped += time.Since(at)
		send("sig CONT")
		return true
	}
	var (
		st  controlState
		sig int // the first ending signal: forwarded to the job, or ending a wait with none to run
		// cut is a signal that ended the wait for the job's output after its status: the job
		// is over and nothing hears it, so it ends the wrapper, which stands for a local
		// command still writing that output. decide reads sig as forwarded, which a job's own
		// status answers, so it never sees this one. A death the run already has came first
		// (a signal or kill in the status, SIGXFSZ at a relayed write), and stands.
		cut       int
		heard     bool
		silent    bool // the remote went quiet: nothing more will arrive, so do not wait for it
		lastHeard = time.Now()
		began     = lastHeard
		ping      = time.NewTicker(heartbeatEvery)
		deadline  <-chan time.Time // set once the job's ssh has ended
	)
	defer ping.Stop()

loop:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			heard, lastHeard = true, time.Now()
			st.feed(l)
			if st.wantGo(sig) {
				send("go")
				st.goSent = true
				if onGo != nil {
					onGo()
					st.stdinSpent = true
				}
			}
			if st.status != "" || st.noSlot {
				break loop
			}
		case <-jobDone:
			jobDone = nil
			if !st.mayHaveRun() {
				// Nothing ran, but a `noslot` or `status unrun` may still be in flight, and
				// it decides where the job goes next. No `go` is sent from here.
				drain := time.After(drainWait)
			rest:
				for st.status == "" && !st.noSlot {
					select {
					case l, ok := <-lines:
						if !ok {
							break rest
						}
						st.feed(l)
					case s := <-sigs:
						if !suspend(s) {
							sig = cmp.Or(sig, int(s.(syscall.Signal)))
							break rest
						}
					case <-drain:
						break rest
					}
				}
				break loop
			}
			// The status follows the job's end at once, but a loaded control session can
			// lag behind: wait as long as for any record before calling contact lost.
			deadline = time.After(silenceLimit)
		case <-deadline:
			break loop
		case s := <-sigs:
			if suspend(s) {
				// Time stopped here is not the remote's silence. A deadline left running
				// would be ready on resume beside a status that arrived meanwhile, and
				// select could take it first.
				lastHeard = time.Now()
				if deadline != nil {
					deadline = time.After(silenceLimit)
				}
				continue
			}
			if informational(s) && st.goSent {
				send("sig " + signalName(s.(syscall.Signal))) // the job runs on
				continue
			}
			// Any other signal ends the run. Before `go`, USR1 or USR2 does too: the
			// command has not started, and the wrapper itself would die of it. The first
			// ending signal is the one the run dies by, as a local command would.
			sig = cmp.Or(sig, int(s.(syscall.Signal)))
			if informational(s) {
				send("sig TERM")
			} else {
				send("sig " + signalName(s.(syscall.Signal)))
			}
		case stream := <-readerGone:
			send("eow " + stream)
		case <-ping.C:
			send("ping")
			limit := connectLimit
			if heard {
				limit = silenceLimit
			}
			if time.Since(lastHeard) > limit {
				silent = true
				break loop
			}
			if !st.mayHaveRun() && !st.ready && time.Since(began)-stopped > setupLimit {
				break loop
			}
		}
	}

	// The job's ssh may still hold the last of its output. Once the status has arrived the
	// job is over and its output is on the way, so wait for the stream to end however long
	// a slow reader takes: ssh's own keepalive ends a dead connection, and a signal ends
	// the wait. Without a status, give it a moment.
	if jobDone != nil && !silent {
		var limit <-chan time.Time
		if st.status == "" {
			limit = time.After(flushWait)
		}
	flush:
		for {
			select {
			case <-jobDone:
				break flush
			case <-limit:
				break flush
			case s := <-sigs:
				if suspend(s) {
					continue
				}
				// With the job never run (`noslot`, `status unrun`), USR1 or USR2 ends the
				// run, as before `go`: there is no job to inform, and nothing may now run.
				if informational(s) && st.mayHaveRun() && st.status != "unrun" {
					if st.status == "" {
						send("sig " + signalName(s.(syscall.Signal)))
					}
					continue
				}
				// After the status of a job that may have run nothing hears it, so it is no
				// forwarded signal (sig); with none run, it ends a wait with nothing to run.
				if st.status != "" && st.mayHaveRun() && st.status != "unrun" {
					cut = int(s.(syscall.Signal))
				} else {
					sig = cmp.Or(sig, int(s.(syscall.Signal)))
				}
				break flush
			}
		}
	}
	// Whether the job's ssh ended on its own, read before the kill below ends it too.
	var ended bool
	select {
	case <-jobEnded:
		ended = true
	default:
	}
	ctlIn.Close()
	jobSSH.Process.Kill()
	ctlSSH.Process.Kill()
	ctlSSH.Wait()
	out := decide(st, sig)
	out.offsets, out.offsetSeen = st.offsets, st.offsetSeen
	out.ran = st.mayHaveRun() && st.status != "unrun"
	// A write the caller's destination refused (ENOSPC on a full disk) lost output. The
	// job's next write to that stream failed too (with EPIPE: a pipe cannot give ENOSPC), so
	// its own status stands; the error is said, as a local run's command would have printed it. A
	// write past the caller's file-size limit would have killed a local job by SIGXFSZ,
	// whatever it did next, unless the caller ignores that signal.
	var lost error
	for _, p := range writers {
		if err := p.err.Load(); err != nil && !out.fallback {
			lost = *err
			break
		}
	}
	if errors.Is(lost, syscall.EFBIG) && !inheritedIgnore(syscall.SIGXFSZ) && out.signal == 0 {
		out.code, out.signal = 128+int(syscall.SIGXFSZ), int(syscall.SIGXFSZ)
	}
	if cut != 0 && out.signal == 0 {
		out.code, out.signal = 128+cut, cut
	}
	if lost != nil {
		if out.message == "" {
			out.message = "writing the job's output failed: " + lost.Error() + "; its output is incomplete"
		}
		return out, nil
	}
	// Output the caller's reader left unread is not output lost.
	if readerLeft.Load() {
		return out, nil
	}
	if st.status == "exit" && !out.fallback {
		switch {
		case ended:
			if code := sshExitCode(jobErr); code != st.code {
				out.message = "the job's output stream ended abnormally (ssh exited " + strconv.Itoa(code) +
					", the job's own status was " + strconv.Itoa(st.code) + "); its output may be incomplete"
			}
		default:
			out.message = "the job's output stream was cut before its end; its output may be incomplete"
		}
		if out.message != "" && out.code == 0 {
			out.code = 255
		}
	}
	return out, nil
}

// pipeWriter passes the job's output on to w until a write to w fails, then calls gone
// once and drops the rest, so ssh drains to the job's end instead of failing its own write.
// A reader gone (EPIPE, or watchReader) also calls left; any other error is kept in err.
type pipeWriter struct {
	w      io.Writer
	gone   func()
	left   func()
	broken atomic.Bool
	once   sync.Once
	err    atomic.Pointer[error]
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	if p.broken.Load() {
		return len(b), nil
	}
	if _, err := p.w.Write(b); err != nil {
		if !errors.Is(err, syscall.EPIPE) {
			p.err.CompareAndSwap(nil, &err)
			p.stop()
			return len(b), nil
		}
		p.close()
	}
	return len(b), nil
}

// close marks w's reader gone, from a failed write or from watchReader.
func (p *pipeWriter) close() {
	p.left()
	p.stop()
}

// stop ends the stream for the job: the job's next write to it fails, as w's did.
func (p *pipeWriter) stop() {
	p.once.Do(func() {
		p.broken.Store(true)
		p.gone()
	})
}

// watchReader calls gone when the reader of f, a pipe, closes its end, which poll reports
// as POLLERR on the write end with no write needed. Anything but a pipe is not watched.
// stop ends the watch.
func watchReader(f *os.File, gone func()) (stop func()) {
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		return func() {}
	}
	var fd int32
	conn, err := f.SyscallConn()
	if err != nil || conn.Control(func(raw uintptr) { fd = int32(raw) }) != nil {
		return func() {}
	}
	wake := make([]int, 2)
	if syscall.Pipe2(wake, syscall.O_CLOEXEC) != nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		const pollErr, pollIn = 0x8, 0x1
		fds := [2]struct {
			fd              int32
			events, revents int16
		}{{fd: fd}, {fd: int32(wake[0]), events: pollIn}}
		for {
			_, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&fds[0])), 2, 0, 0, 0, 0)
			if errno == syscall.EINTR {
				continue
			}
			if errno == 0 && fds[0].revents&pollErr != 0 && fds[1].revents == 0 {
				gone()
			}
			return
		}
	}()
	return func() {
		syscall.Close(wake[1])
		<-done
		syscall.Close(wake[0])
	}
}

// suspendSelf stops this process until something continues it. kill returns before the
// stop takes hold, whichever thread the kernel stops last, so it waits for the SIGCONT
// that ends the stop: returning at once would let the caller act (send `sig CONT`, end a
// watcher) as if already continued. A var so a test need not stop itself.
var suspendSelf = func() {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	<-cont
}

// sshExitCode is the code ssh exited with: the remote command's, or 128+n if a signal
// killed ssh, or -1 when Wait failed some other way.
func sshExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

// signalName is the name the launcher accepts for a forwarded signal.
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGUSR1:
		return "USR1"
	case syscall.SIGUSR2:
		return "USR2"
	}
	return "TERM"
}

// informational reports whether s is one a command may handle and run on (dd's USR1
// progress report), so it is forwarded to a running job without ending the run.
func informational(s os.Signal) bool {
	return s == syscall.SIGUSR1 || s == syscall.SIGUSR2
}

// newControlID names one job's control channel on the remote.
func newControlID() string {
	return fmt.Sprintf("%d-%x", os.Getpid(), time.Now().UnixNano())
}
