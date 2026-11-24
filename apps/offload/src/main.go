// Command offload runs a command on another machine as if it ran here: the
// remote mounts dev's filesystem in a namespace of its own for the one job.
package main

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// configDir is set at build time by install.sh to the unit's config/ folder.
var configDir string

// scopedExportRoot is set by the restricted-view installer. An absent view
// keeps jobs local rather than assuming the dev filesystem is exported.
var scopedExportRoot string

const usage = "usage: offload [--where local|<remote>] [--explain] [--stdin] -- <command> [args...]"

func main() {
	if v := os.Getenv(contWatchEnv); v != "" {
		os.Exit(watchContinue(v))
	}
	keepInheritedIgnores()
	quitQuietly()
	if len(os.Args) > 1 && os.Args[1] == "report" {
		os.Exit(reportCmd(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(healthCmd(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == openFilesCmd {
		os.Exit(openFilesMain(os.Args[2:], os.Stdout))
	}
	exitAs(run(os.Args[1:]))
}

// diedBy is the signal the command died by, when it did, set by the run that saw it end.
var diedBy syscall.Signal

// exitAs ends the wrapper as its command ended: by diedBy, when it is set, since a shell
// ends a script's loop on Ctrl-C only when the child was killed by SIGINT, not when it
// exited 130; otherwise with code.
func exitAs(code int) {
	followAppended()
	if diedBy != 0 {
		dieBy(diedBy)
	}
	os.Exit(code)
}

// sayOutcome prints the run's closing message, if any.
func sayOutcome(msg string) {
	if msg == "" {
		return
	}
	fmt.Fprintln(os.Stderr, "offload:", msg)
}

func run(args []string) int {
	// Caught for the wrapper's whole life, unless the caller ignores it: with the caller's
	// reader of stderr gone (`2>&1 | head -1`), a message (`running locally`, the outcome)
	// fails rather than killing the wrapper before it runs the job or ends as the job did.
	// A command it execs or starts gets SIGPIPE back at its default.
	if !signal.Ignored(syscall.SIGPIPE) {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	}
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "offload:", err)
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if os.Getenv(insideJob) != "" {
		return runInPlace(opts.argv)
	}
	if err := checkStdin(os.Stdin, opts.stdin); err != nil {
		fmt.Fprintln(os.Stderr, "offload:", err)
		return 2
	}
	code, err := runRemote(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "offload:", err)
		return 2
	}
	return code
}

// insideJob is set in every offloaded job's environment by the launcher.
const insideJob = "OFFLOAD_JOB"

// runInPlace runs a job's own offload call's command in its place, by exec: the job
// already runs where offload put it, and has neither a remote to reach nor the user bus
// a local run's batch scope needs. It goes through env as every run does (viaEnv), and
// returns only if env itself cannot start, with 126.
func runInPlace(argv []string) int {
	fmt.Fprintln(os.Stderr, "offload: running in place: already inside an offloaded job")
	env := viaEnv()
	for fd := range 3 {
		if stdioClosed(fd) {
			syscall.Close(fd) // the runtime's /dev/null: the command gets it closed, as the caller passed it
		}
	}
	err := syscall.Exec(env[0], slices.Concat(env, commandWords(argv)), os.Environ())
	fmt.Fprintln(os.Stderr, "offload:", env[0]+":", err)
	return 126
}

// options is the parsed command line.
type options struct {
	where   string // "", local, or a remote's name
	explain bool
	stdin   bool // forward the caller's stdin to the job
	argv    []string
}

func parseArgs(args []string) (options, error) {
	var o options
	for len(args) > 0 {
		switch {
		case args[0] == "--where":
			if len(args) < 2 || args[1] == "--" {
				return options{}, errors.New("--where needs a value: local or a remote's name")
			}
			o.where, args = args[1], args[2:]
		case args[0] == "--explain":
			o.explain, args = true, args[1:]
		case args[0] == "--stdin":
			o.stdin, args = true, args[1:]
		case args[0] == "--":
			o.argv = args[1:]
			args = nil
		default:
			return options{}, fmt.Errorf("unexpected %q", args[0])
		}
	}
	if len(o.argv) == 0 {
		return options{}, errors.New("no command")
	}
	return o, nil
}

func runRemote(opts options) (int, error) {
	where, explain, argv := opts.where, opts.explain, opts.argv
	f, err := os.Open(filepath.Join(configDir, "offload.conf"))
	if err != nil {
		return 0, fmt.Errorf("read config (was it built by install.sh?): %w", err)
	}
	defer f.Close()
	cfg, err := parseConfig(f)
	if err != nil {
		return 0, err
	}
	if _, ok := cfg.remotes[where]; where != "" && where != local && !ok {
		return 0, fmt.Errorf("unknown remote %q", where)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	start := time.Now()
	// newRecord is the decisions-log entry for d, ending now with the given exit code.
	newRecord := func(d decision, code int) record {
		return record{Time: start, Argv: argv, Cwd: cwd, Placement: d.Placement, Rule: d.Rule, Reason: d.Reason,
			WallSeconds: time.Since(start).Seconds(), ExitCode: code}
	}
	// finishLocal runs the command here, in the batch scope, and logs the decision.
	finishLocal := func(d decision) (int, error) {
		code, cpu, err := runBatch(argv, opts.stdin)
		if err != nil {
			return 0, err
		}
		rec := newRecord(d, code)
		rec.CPUSeconds, rec.CPUPressure = &cpu, cpuPressure()
		appendRecord(rec)
		return code, nil
	}
	if where == local {
		// Nothing is gathered for a forced local run: no remote's slots or paths apply.
		req := placeRequest{Where: where}
		d := place(req)
		if explain {
			explainPlacement(os.Stderr, req, d)
		}
		return finishLocal(d)
	}

	groups, err := os.Getgroups()
	if err != nil {
		return 0, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	repo := gitToplevel(cwd)
	paths, err := jobPathsFor(home, jobRepos(cwd))
	if err != nil {
		return 0, err
	}
	stdio, problem := readStdio()
	// Each output file is one the job writes, so the checks of its writable paths cover it.
	mounts := devMounts()
	view := scopedExport(scopedExportRoot, accountHome(), configDir)
	env := view.environment(os.Environ(), mounts)
	if view.root != "" {
		paths.writable = slices.DeleteFunc(paths.writable, func(p string) bool {
			return view.missing(mounts, p) != "" || view.missing(mounts, realPath(p)) != "" ||
				within([]string{"/etc", "/usr"}, realPath(p)) != ""
		})
	}
	facts, readOnly := gatherJobFacts(configDir, cwd, repo, argv, slices.Concat(paths.writable, stdio.paths()), mounts, env, view)
	req := placeRequest{
		Where:   where,
		Remotes: cfg.order,
		Slots:   map[string]slotState{},
		Facts:   facts,
	}
	// Before the launcher claims one, a slot is assumed free: the claim, made on the
	// remote's own disk, is the only authority. --explain reads the real state.
	for _, name := range cfg.order {
		req.Slots[name] = slotState{Total: cfg.slotCount(name), Free: cfg.slotCount(name)}
		if explain {
			req.Slots[name] = probeSlots(cfg, name)
		}
	}
	j := job{
		devAddr:  cfg.dev,
		uid:      os.Getuid(),
		gid:      os.Getgid(),
		groups:   groups,
		repo:     repo,
		cwd:      cwd,
		argv:     argv,
		env:      env,
		writable: paths.writable,
		readOnly: append(paths.readOnly, readOnly...),
		grace:    cfg.grace,
	}
	j.stdio = stdio
	req.Facts.OutputFile = cmp.Or(problem, outputRefusal(stdio.files, mounts,
		remoteLocalDirs(remoteLocalPaths(configDir, accountHome())), j.readOnly, view))
	// The job opens its output files itself, so each must be writable there; one already
	// under a writable path is (a bind of it would make a rename of it fail with EBUSY).
	for _, p := range stdio.paths() {
		if within(withRealPaths(slices.Concat(j.writable, sharedTmp)), p) == "" {
			j.writable = append(j.writable, p)
		}
	}
	req.Facts = withJobFacts(req.Facts, j)
	d := place(req)
	if explain {
		explainPlacement(os.Stderr, req, d)
		explainPaths(os.Stderr, j, remoteLocalPaths(configDir, accountHome()))
	}
	if d.Placement == local && !explain {
		fmt.Fprintln(os.Stderr, "offload: running locally:", d.Reason)
	}
	if d.Placement == local {
		return finishLocal(d)
	}
	// Without the lock another wrapper's sweep could take this job's open files.
	lock, err := holdJobsLock(jobsLockPath)
	if err != nil {
		d = decision{local, "jobs-lock", fmt.Sprintf("the jobs lock could not be taken (%v)", err)}
		fmt.Fprintln(os.Stderr, "offload: running locally:", d.Reason)
		return finishLocal(d)
	}
	defer lock.Close()
	d, out, err := runPlaced(os.Stderr, cfg, req, d, j, opts.stdin)
	if err != nil {
		return 0, err
	}
	if d.Placement == local {
		lock.Close() // a local run leaves no silly-rename files, so it never holds off a sweep
		return finishLocal(d)
	}
	if out.sweep {
		sweepSillyRenames(lock, "/tmp", "/var/tmp", j.repo)
	}
	sayOutcome(out.message)
	diedBy = syscall.Signal(out.signal)
	rec := newRecord(d, out.code)
	rec.Fault = out.message
	if out.cpuKnown {
		cpu := float64(out.cpuMs) / 1000
		rec.CPUSeconds = &cpu
	}
	appendRecord(rec)
	return out.code, nil
}

// runPlaced runs the job on d's remote and returns the decision and the remote's outcome.
// A remote that has no free slot after all (another call took it first) hands the job to
// the next remote with one; when none has, or a remote never let the job go, the
// returned decision is local and stderr has said why. The caller then runs it here.
func runPlaced(stderr io.Writer, cfg config, req placeRequest, d decision, j job, stdin bool) (decision, outcome, error) {
	for d.Placement != local {
		out, err := runOn(cfg, d.Placement, j, stdin)
		if err != nil {
			return d, outcome{}, err
		}
		if !out.fallback {
			return d, out, nil
		}
		if out.noSlot {
			req.Slots[d.Placement] = slotState{Total: req.Slots[d.Placement].Total}
			d = place(req)
		} else {
			d = decision{local, "remote-unreachable", d.Placement + " never let the job go, so nothing ran there"}
		}
		if d.Placement == local {
			fmt.Fprintln(stderr, "offload: running locally:", d.Reason)
		}
	}
	return d, outcome{}, nil
}

// runOn runs the job on the named remote and returns what its control channel reported.
// A var so a test can script the outcomes runPlaced acts on.
var runOn = runOnRemote

func runOnRemote(cfg config, where string, j job, stdin bool) (outcome, error) {
	addr := cfg.remotes[where]
	j.ctlID = newControlID()
	j.cpuQuota, j.memoryMax = cfg.cpuQuota[where], cfg.memoryMax[where]
	j.slots = cfg.slotCount(where)
	var err error
	j.stdio = j.stdio.at()
	if j.umask, j.nofile, err = callerLimits(); err != nil {
		return outcome{}, err
	}
	j.rlimits = callerRlimits()
	j.ignore, j.block = inheritedIgnoreList(), inheritedBlockList()

	// Dev's loopback services reach the job over this connection, as -R unix sockets.
	j.id = jobID()
	if out, err := exec.Command("ss", "-ltnH").Output(); err == nil {
		j.forwards = parseListeners(string(out))
	}
	sshOpts := []string{"-F", "/dev/null", "-T",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"}
	jobOpts := append([]string{}, sshOpts...)
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		jobOpts = append(jobOpts, "-o", "ForwardAgent=yes")
	}
	for _, f := range j.forwards {
		jobOpts = append(jobOpts, "-R", f.sshSpec(j.id))
	}
	ssh := exec.Command("ssh", append(jobOpts, addr, launcherCommand(j))...)
	// The environment block comes first; the caller's stdin follows only with --stdin, and
	// only once `go` is sent, so a fallback still finds it unread.
	var follow io.Reader
	if stdin {
		follow = os.Stdin
	}
	in, onGo, err := jobStdin(envBlock(j.env), follow)
	if err != nil {
		return outcome{}, err
	}
	defer in.Close()
	ssh.Stdin = in
	if !stdin {
		onGo = nil // nothing follows the environment, so nothing is spent at `go`
	}
	ssh.Stdout, ssh.Stderr = os.Stdout, os.Stderr
	// What reaches an output file the job writes itself over ssh is held until the job is over.
	var held [2]*heldOutput
	for _, f := range j.stdio.files {
		h := &heldOutput{f: []*os.File{os.Stdout, os.Stderr}[f.fd-1]}
		held[f.fd-1] = h
		if f.fd == 1 {
			ssh.Stdout = h
		} else {
			ssh.Stderr = h
		}
	}
	if j.stdio.merged && held[0] != nil {
		// fd 2 is stdout's open file: one hold keeps their order.
		ssh.Stderr = held[0]
	}
	ctl := exec.Command("ssh", append(append([]string{}, sshOpts...), addr, controlCommand(j.ctlID))...)
	out, err := supervise(ssh, ctl, onGo)
	// Whether the job may have written its files; supervise errs only before sending anything.
	ran := out.ran
	if !out.fallback {
		j.stdio.settle(out.offsets, out.offsetSeen, ran)
	}
	// What the wrapper writes to an output file from here (held messages, then its own) goes
	// at the file's end once the job may have run, leaving the caller's offset where the job
	// left it; else where the caller stands, as a local run's message would.
	to := [2]*os.File{os.Stdout, os.Stderr}
	if ran && !out.fallback {
		for _, f := range j.stdio.files {
			a, aerr := appendTo(f.fd)
			if aerr != nil {
				firstMessage(&out, "appending to the job's output file failed: "+aerr.Error())
				continue
			}
			to[f.fd-1] = a
			trackAppended(f.fd, a)
			if f.fd == 2 || j.stdio.merged {
				os.Stderr = a
			}
		}
	}
	for i, h := range held {
		if ferr := h.flush(to[i]); ferr != nil {
			firstMessage(&out, "writing the launcher's messages to the job's output file failed: "+ferr.Error())
		}
	}
	return out, err
}

// firstMessage sets out's message to why the wrapper could not write an output file, unless it has one.
func firstMessage(out *outcome, message string) {
	if out.message == "" {
		out.message = message
	}
}

// jobID is 8 random hex digits, unique to one job.
func jobID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func gitToplevel(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(out), "\n") // only git's newline: a path may end in space
}

// jobRepos is what a job run in cwd writes to: its repository's toplevel and the git dir
// shared across its worktrees, which a linked worktree's commits write outside it.
func jobRepos(cwd string) []string { return []string{gitToplevel(cwd), gitCommonDir(cwd)} }

// gitCommonDir is the git dir that dir's repository shares across its worktrees, which a
// linked worktree's commits write outside its toplevel; "" outside a repository.
func gitCommonDir(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(out), "\n")
}

// withJobFacts is f with the refusal facts read from the job itself.
func withJobFacts(f refusalFacts, j job) refusalFacts {
	f.TooLong = commandTooLong(j)
	f.Root = rootSquashed(j.uid, j.gid, j.groups)
	f.Newline = newlinePath(j)
	f.Groups = groupsOverNFS(j.groups)
	f.ReadOnlyCwd = readOnlyCwd(j.cwd, j.writable, j.readOnly)
	return f
}

// rootSquashed reports whether dev's export (root_squash) would squash the caller: nfsd
// maps uid 0, and gid 0 as the primary or a supplementary group, to nobody.
func rootSquashed(uid, gid int, groups []int) bool {
	return uid == 0 || gid == 0 || slices.Contains(groups, 0)
}

// newlinePath is the first path the launcher would get that holds a newline, or "": its
// writable and read-only lists are newline-separated, so such a path would split in two
// and, skipped as missing, a deny entry among them would be left writable.
func newlinePath(j job) string {
	for _, p := range slices.Concat([]string{j.cwd, j.repo}, j.writable, j.readOnly, j.stdio.paths()) {
		if strings.Contains(p, "\n") {
			return p
		}
	}
	return ""
}

// readOnlyCwd is cwd when the job could write nothing there: with its symlinks followed,
// as the job's binds follow them, no writable path holds it or a read-only path does.
// Else "".
func readOnlyCwd(cwd string, writable, readOnly []string) string {
	c := realPath(cwd)
	if within(withRealPaths(writable), c) != "" && within(withRealPaths(readOnly), c) == "" {
		return ""
	}
	return cwd
}
