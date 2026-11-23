## Purpose

`offload [--where local|<remote>] -- <cmd>` runs a command on another machine as if it ran on
dev: the remote mounts dev's filesystem inside a mount namespace of the one job and runs
the command there, borrowing only CPU and RAM. Nothing is copied. When offloading is unsafe
or no slot is free it runs locally in a low-priority scope, and `offload report` measures
whether it pays.

## Ownership

- `src/` — the Go module `devex/offload`, one file per concern:
  - `main.go` (arguments and the run), `config.go` (`offload.conf` and the shared
    config-line reader), `job.go` (the job and the launcher's command line).
  - `resolve.go` and `paths.go` (path classes), `shell.go` (how an argument or `-c` script
    names paths, `readArg`'s `argPaths`, and the program a command word runs through its
    wrappers, `findCommand`), `links.go` (the symlinks a job may follow).
  - `nopt.go` (npm's and npx's command lines as npm 10.9.9 reads them: npx-cli.js's
    rewrite, then nopt; see `docs/third-party-notices.md`), `localonly.go` (`config/local-only`, package-manager scripts and
    `config/remote-store-commands`).

  - `openfiles.go` and `holders.go` (dev's processes' open files and locks, a container's
    among them), `mounts.go` (dev's mounts the export does not carry), `listeners.go`
    (dev's listeners to forward).
  - `local.go` (the local run), `stdio.go` (`--stdin` and the stdio plan), `control.go`
    (control channel and exit decision).
  - `place.go` and `facts.go` (placement and its refusal facts), `batch.go` (local batch
    scope), `sweep.go` (silly-rename sweep), `record.go` and `report.go` (decisions log,
    `offload report`), `health.go` (`offload health`), `explain.go` (`--explain`), `sigign.go` (the cgo constructor that
    records inherited signal state).

  The remote side, also in `src/`: `offload-launch` (bash, runs as root) and three static Go
  commands: `cmd/offload-bind/` (bind-mount helper), `cmd/offload-forward/` (loopback relay)
  and `cmd/offload-wait/` (the job's parent and its pid namespace's init, which checks its
  writes reached dev); `internal/strerror/` words their errnos as the C library does.

  Each command's `main_test.go` tests it, `install_test.go` the install and
  uninstall scripts with stand-in tools, `launcher_monitor_test.go` the launcher's early
  exits in namespaces of its own (skipping, saying why, where `unshare` is refused)

- `config/offload.conf` — `dev <addr>` and `remote <name> <addr>`; the export admits
  exactly the listed remotes, tried in file order. Copy `config/offload.conf.example` to the
  gitignored `config/offload.conf` and fill in the deployment addresses before installing.

  `slots <remote> <n>` is how many jobs a remote takes at once (default 1). Optional `grace <seconds>`, and `cpu-quota <remote> <v>`
  and `memory-max <remote> <v>` (systemd values such as `150%`, `512M`)

- `config/remote-local-paths` — paths under the job user's home (its passwd entry's on dev,
  never `HOME`) kept on the remote's disk; an entry written `ro <path>` is bound read-only

- `config/local-only` — the watch/serve list: argv[0] basenames, or a program followed by
  the arguments that must come next (`cargo watch`), one per line. Package-manager
  `dev`/`start`/`serve`/`watch`/`preview` scripts, task-runner tasks (`taskNames`) and watch
  flags match without an entry (`packageCommand`, `watchFlag`, `localonly.go`; the list's
  header has every form).

  The program an `npx`/`bunx`/`exec` form or `python -m` (`pythonModule`) runs, and the one
  `uvx`, `uv tool run` and `pipx run` start, is matched against this list and
  `config/remote-store-commands` alike (`listedRun`, `findCommand`)

- `config/remote-store-commands` — commands that use a store kept in a remote-local path,
  read as `config/local-only` is: the container tools, whose images live there, and the
  commands that clean a remote-local cache (`go clean`, `uv cache`, `pre-commit clean|gc`)

- `config/offload-net.nft` — the remote's job firewall and NAT

- `deploy/systemd/offload-net.service` — loads that ruleset on each remote at boot;
  `install.sh` starts it and never restarts it

- `deploy/systemd/offload-cut.service` — runs `offload-launch --cut` when it stops;
  `install.sh` starts it and never restarts it

- `deploy/systemd/offload-health.{service,timer}` — dev's `--user` units running
  `offload health --notify` at `:05` and `:35`; `install.sh` copies them into
  `~/.config/systemd/user/` and enables the timer, `uninstall.sh` removes them

- `install.sh`, `uninstall.sh` — one command each, idempotent; `install.sh --export-only`
  applies dev's export and nfsd tuning without touching the remotes or the command, and
  `scripts/remotes.sh` (sourced) reads and validates an `offload.conf`, refuses while a
  remote runs a job (`refuse_while_jobs`), and holds `remote_ssh`, every remote call's ssh,
  which gives up on a remote that stops answering rather than hanging

- `scripts/benchmark` — the benchmark gate: reproduces every run and prints the tables

- `tests/integration/` — its own Go module, `devex/offload/tests/integration`, run by
  `scripts/integration`; needs the server. The gate leaves out import paths with a
  `/tests/integration` segment, so the module path must keep one

## Local Contracts

### Install and uninstall

- `install.sh` takes no argument but `--export-only`; any other exits 2 with its usage
  before it reads anything.

- `install.sh` then checks `offload.conf` and, before writing anything, refuses (exit 1,
  naming the line, "nothing installed") a config with no `remote` line or one whose
  address is not a single IPv4 address or host name: a wildcard, CIDR, `@netgroup`, IPv6
  address, option string or trailing word would make the export admit other hosts.

- `install.sh`, `--export-only` included, then refuses, exit 1 with "nothing installed"
  and before it changes anything, while any remote has an `offload-job-*.scope` that is
  not yet stopped (a deactivating one still holds processes) or a running `offload-launch`
  (it lists them), or cannot be asked.

  Each remote file is written to `<path>.new` and its sha256 checked against dev's copy; a
  file that fails either removes every `.new` staged so far ("did not arrive whole; nothing
  replaced", exit 1; uninstall removes any a cut connection left), and only once all arrive
  whole is each renamed over the old one, all in one ssh call (a cut in that instant can
  leave old and new files side by side; the next install replaces them).

  The job firewall is replaced in one `nft -f` transaction, never by restarting
  `offload-net.service`, whose stop deletes the tables before its start loads them.

- `install.sh` writes `/etc/exports.d/offload.exports` (`/` over NFSv4.2 to each remote
  only, `rw,sync,root_squash,no_subtree_check,fsid=0`, and `/tmp` beside it, `fsid=1`),
  `/etc/nfs.conf.d/offload.conf` (8 nfsd threads, v4 only) and a drop-in
  `/etc/systemd/system/nfs-server.service.d/offload.conf` that renices the nfsd threads to
  -10 after each start (kernel threads sit in the root cgroup, so `CPUWeight` cannot reach
  them).

  It installs `/usr/local/libexec/offload-launch`, `offload-bind`, `offload-forward`,
  `offload-wait` and `/etc/offload/` on each remote, with the `nftables` package and
  `offload-net.service` and `offload-cut.service`; and builds `~/.local/bin/offload` with
  `config/` baked in through `-X main.configDir`. The three Go helpers are built static
  for linux/amd64, the remotes' one arch, and pushed from a temporary directory. Last, it
  copies the health units (copied, not linked, as the command is) and enables the timer.

- `uninstall.sh` removes all of it and leaves the packages. It refuses, changing nothing
  and exiting non-zero with the remote's address, while any remote lists an
  `offload-job-*.scope` not yet stopped or a running `offload-launch` (or cannot be asked; a failed scope
  does not count); it disables `nfs-server` only when no other export remains. It reads
  `offload.conf` leniently rather than through `scripts/remotes.sh`, skipping a `remote`
  line with no address and validating none, so a config with its remotes removed still
  removes dev's export.

### Path classes

- **Writable**: the git toplevel of the cwd and its common git dir, plus every `Edit(...)`
  allow rule of `~/.claude/settings.json`, read at each launch, less the deny rules.
  Dev's `/tmp` and `/var/tmp` are always writable (`sharedTmp`). `resolvePaths`
  (`resolve.go`) owns the rule syntax and the `**` search limits; a rule that makes `/`
  itself writable fails every launch loudly.

- **Read-only**, each listed by `--explain` and bound over the writable mount so a write
  fails naming the path:
  - each existing deny-rule path (a deny on an ancestor binds the writable path whole);
  - each mount the export does not carry inside or under a writable path
    (`readOnlyMounts`, `mounts.go`);
  - each directory of dev's root filesystem bound read-only onto itself under a writable
    path (`readOnlySelfBinds`);
  - each database whose writers share a memory map (`mappedDatabase`: SQLite WAL, LMDB)
    that a dev process holds open, when `wal-open` does not keep the job here, and each
    data directory a live `*.pid` file guards (`walPlan`, `pidDataDir`, `holders.go`);
  - each file a dev process holds a `flock` on, when `flock-held` does not keep the job
    here (`flockPlan`).

- **Remote-local**: `config/remote-local-paths`, bound from the remote's disk. The
  container stores (`~/.local/share/containers`, `~/.cache/containers`) are bound
  read-only, so a job that would pull, build or remove an image fails with EROFS rather
  than change a store dev never sees.

- **Dev's `/tmp`**: exported beside `/`, so the job's `/tmp` and `/var/tmp` are dev's. The
  launcher refuses a job when the export lacks `/tmp`. A caller whose `/tmp` is not dev's
  exported one (PrivateTmp, its own tmpfs; `exportedTmpDevice`, `mounts.go`) is kept here
  by `tmpfs`.

- **Private**: `/run` is an empty tmpfs. `/dev/shm` and `/dev/mqueue` are the job's own, in
  an IPC namespace of its own, and end with it; `offload-wait` fails the run, naming them,
  when the job leaves files in either, since a local run would have kept them. Podman's
  lock table (`libpod_*lock*`) is exempt.

- A remote that looked up a mount pins it on dev: `umount` says it is busy until
  `sudo exportfs -f` flushes nfsd's export cache.

### Placement

`offload -- <cmd>` decides for itself; `--where local|<remote>` forces it. `place`
(`place.go`, pure, table-tested) maps each remote's slot state, the refusal facts and
`--where` to a placement and the rule that decided it. `gatherFacts` (`facts.go`) collects
the facts; `runPlaced` (`main.go`) walks the placement over the remotes' outcomes
(`noslot` to the next remote, unreachable to local). `refusals` (`place.go`) weighs the
rules in this order and `TestRefusalsAreInTheDocumentedOrder` (`explain_test.go`) pins it;
change both together.

1. `where-local` — `--where local` was given.

Hard refusals, which `--where <host>` cannot override:

2. `tmpfs` — a path the job names or follows reaches an unexported mount, a private
   directory or dev's own machine (`/dev`, `/sys`, a dev pid), or holds an unexported
   mount. `tmpRefusal`, `cwdLinkRefusal`, `hostOnly` (`paths.go`, `links.go`); words
   become paths in `readArg` and `scriptPaths`.
3. `remote-local` — a path the job names or follows is under a remote-local path or holds
   one; environment values are not judged. `remoteLocalRefusal` (`paths.go`).

   Or the job runs a `config/remote-store-commands` entry, also behind sudo, su, doas,
   pkexec or run0 (`storeCommandRefusal`, `localonly.go`).

4. `wal-open` — a dev process holds a `mappedDatabase` file the job is likely to use, or
   one with hard links. `walPlan` (`holders.go`).

5. `wal-bare` — a held WAL-mode SQLite file has no `-shm`, which a remote reader would
   create over NFS. `bareWAL`.

6. `flock-held` — a dev process holds a `flock` on a file the job is likely to use (NFS
   turns the job's into a POSIX lock that never conflicts). `flockPlan`.

7. `foreign-holder` — a container holds such a file on a writable path's filesystem,
   under a path dev cannot place. `foreignPlan`.

8. `unseen-holder` — offload could not read every holder: the `sudo -n` read (the hidden
   `open-files <pid>…` mode) failed, a held lock matches no open file, or a writable
   path's device is not its mount's. Needs passwordless sudo. `hiddenHolders`,
   `lockDeviceMismatch` (`openfiles.go`).

9. `special-file` — an argument names a FIFO, socket or device node outside `/dev`,
   `/proc`, `/sys`. `specialFileRefusal`.

10. `too-long` — the remote command line would exceed 120 KiB (`maxCommandLine`).

11. `inherited-fd` — a descriptor past stderr is inherited without close-on-exec.

12. `closed-stdio` — stdin, stdout or stderr was passed closed; a local run passes it on
    closed (`childStdio`).

13. `output-file` — stdout or stderr is a file the remote job could not open as dev's.
    `readStdio`, `outputRefusal` (`stdio.go`).

14. `root` — uid 0 or gid 0, which root_squash maps to `nobody`. `rootSquashed`.
15. `newline-path` — a path the launcher's newline-separated lists must carry holds a
    newline.
16. `groups` — more than 16 supplementary groups (`maxNFSGroups`), all AUTH_SYS carries.

Soft refusals, only with no `--where`:

17. `interactive-tty` — fd 0 is a terminal.
18. `sudo` — the job runs `sudo`, `sudoedit`, `su`, `doas`, `pkexec` or `run0` (`findCommand`).
19. `watch-serve` — a `config/local-only` entry or built-in rule matches; a best effort,
    so a watcher it misses runs remotely. `npx <pkg>` is read as the program named by the package's
    last path part, so a package whose bin is named otherwise (`npx @angular/cli serve`
    runs `ng`) is missed: the bin is known only from the package's manifest.
20. `read-only-cwd` — the job could write nothing in the cwd (`readOnlyCwd`).

Last the slot: `free-slot` or `where-remote` when a remote has one, else `no-free-slot`.
`--where <host>` still needs a free slot there; with none it runs locally and says so.
Two more rules come from the run, not `place`: `remote-unreachable` (a remote that never
got `go`) and `jobs-lock` (the sweep lock could not be shared in time).

- A set `DISPLAY` or `WAYLAND_DISPLAY` refuses nothing; the job's environment on the
  remote has neither (`envBlock`).

- A script that reaches `/run` or an unexported mount by a relative path or a variable is
  not seen. A lock on no descriptor (mmap only, in flight over a socket) shows only in
  `/proc/locks` and may be missed. A word matched wrongly only keeps the job here.

- An `offload` call inside an offloaded job (`OFFLOAD_JOB`, which the launcher sets)
  places nothing: it prints `offload: running in place` and execs the command where the
  job runs.

### Slots

- The launcher takes an exclusive `flock` on `/var/lib/offload/slots/<n>` on the remote's
  own disk after the control channel attaches, and holds it until it exits; neither the
  job nor the launcher's monitor holds it, so a watchdog stop or a launcher killed
  outright frees the slot. With none free it writes `noslot`, starts nothing, and the
  wrapper runs locally or on the next remote. The wrapper does not probe first: the claim
  is the authority.

- `--explain` and `offload-launch --slots N` read the real state; that probe briefly takes
  each free slot's lock, so a claim at that instant may answer `noslot`. `offload.conf`
  records how the slot count was sized.

### Local runs, `--explain`, log and report

- Every local run (a refusal, a fallback, `--where local`) goes in a
  `systemd-run --user --scope -p CPUWeight=10` scope; local slots are unlimited. With no
  user manager answering (as root under sudo) it runs outside a scope at normal priority,
  saying so on stderr (`batch.go`). The scope requires `systemd-run` with
  `--expand-environment=no` so it passes command arguments literally.

- Every placement starts the command through `/usr/bin/env --` (`viaEnv`), so a leading `NAME=value` word sets a variable and a missing command
  exits 127 (126 when it cannot run) with env's message.

- A local run that is not `--where local` prints one `offload: running locally: <reason>`
  line on stderr; a remote placement prints nothing. Stdin is `/dev/null` unless it is a
  terminal, which the command keeps (`runLocal`).

- `--explain` prints on stderr `offload: slots`, one `offload: refusal <rule>: <reason>`
  line per refusal that applies (hard ones first), the
  `offload: placement <where> by <rule>: <reason>` line and the path classes
  (`offload: <class> <path>`), then runs the job as decided; under `--where local` it
  prints only the placement line.

- Decisions log: one JSON object per line in
  `${XDG_STATE_HOME:-~/.local/state}/offload/decisions.log`: `time`, `argv`, `cwd`,
  `placement`, `rule`, `reason`, `wall_seconds`, `cpu_seconds`, `exit_code`,
  `cpu_pressure_avg10` (dev's `some avg10` from `/proc/pressure/cpu` at the end of a local
  run, null on a remote run or when unreadable). `cpu_seconds` is the children's rusage
  for a local run and, for a remote one, the launcher's `cpu <ms>` record (null when the
  remote was lost before it).

- `offload report [--since <dur>]` (default `7d`; `Nd` or a Go duration; `report.go`)
  prints jobs remote versus local, local counted by deciding rule, CPU-seconds run
  remotely, failures (non-zero exits, 255 being lost contact) by placement, and the mean
  `cpu_pressure_avg10` of local runs. `summarise` is pure; `--since` is the only flag.

- `apps/claude-user-level/src/rules/offload.md` tells agents to prefix `offload --` to
  builds, tests and replays over about 30 s of CPU that no runner already offloads. Heavy
  runners call `offload` themselves; the gate (`scripts/check`, `scripts/ci`) does not, so
  it never depends on the server. Adopted runner: `apps/session-review/src/review.py` runs
  `scan.py` through `offload` when `~/.local/bin/offload` exists. Other repositories'
  runners are tracked as `offload: adopt in <repo> <runner>` issues.

### Health

- `offload health [--notify]` (`health.go`) runs `hostname` through `--where <remote>` on
  each remote in file order. Each probe uses a private directory on the `/tmp` export,
  a clean environment with the SSH agent and nested-job guard preserved, and its own decisions log.
  A relative agent socket uses a short symlink beside the probe cwd, so OpenSSH can reach the
  original socket without the placement checks walking that link. Health then reads the jobs
  that ended since its last run (30 min with no state) and prints the verdict:

  - `fail`: a remote did not take the test job (`judgeProbe`: it ran on dev for any rule
    but `no-free-slot` or `jobs-lock`, carried a fault, failed, or left no record);
  - `warn`: a job in the window lost contact with its remote (exit 255 with a `fault`, the
    wrapper's own account in the record) or ran locally by `remote-unreachable`;
  - `ok` otherwise. A job's own exit, a bare 255 or 125 included, is not offload's fault;
    the launcher's 125 reaches the wrapper as a plain exit, so it is not counted.

- `--notify` sends the verdict with `homelab-notify --stream errors --report offload`, so it
  uses the optional external `homelab-notify` command, and saves `health.json` (`last_run`,
  `status`) beside the decisions log. An unchanged status goes as a `--heartbeat`, so a
  fault alerts once and the panel still sees the check run. A full event that fails to send
  exits 1 with the state unsaved, so the next run sends it again.

- `tests/integration/` sets `XDG_STATE_HOME` to a folder of its own, so the faults the tests
  cause never reach dev's real log.

### The remote job

- Nothing is mounted in the remote's host namespace: every mount is made after
  `unshare --mount --propagation private`, and the job runs in a transient scope killed
  when the command exits. Mount points live under `/run/offload-job.XXXXXX/`, removed with
  `rmdir`, never `rm -r`.

- The launcher mounts dev's `/` read-only, binds each writable path and read-only override
  from two further mounts of the export, the remote-local paths, an empty `/run` and the
  remote's `/proc`, `/sys`, `/dev`, then `pivot_root`s (never `chroot`: it blocks user
  namespaces) and drops to the caller's uid, gid and supplementary groups (none gets
  `--clear-groups`) with `setpriv --no-new-privs`, so a setuid program fails loudly. The
  job runs in a pid namespace of its own, so `ps` and `pkill` see only its processes.

  The cwd is entered only after the drop: dev's home is 0700 and root is squashed.

- `offload-bind` resolves each path inside the job root as if it were `/`, so no entry
  reaches the remote's own disk; exits 2 for a failed mount whose destination does not
  exist, else 1. The launcher makes every bind in one `offload-bind` process, in list
  order, the first failure stopping the rest. Its resolution and `--skip-missing` rules
  are `cmd/offload-bind/main.go`'s.

- The job gets the caller's umask and soft limits (`OFFLOAD_UMASK`, `OFFLOAD_NOFILE`,
  `OFFLOAD_RLIMITS`; `callerLimits`, `callerRlimits`), capped at the remote's hard ones.

- The caller's environment never reaches the launcher's root shell: it travels on stdin
  (NUL-terminated entries ended by an empty one; the command line is world-readable on the
  remote) and is applied by the final `env -i` after `setpriv`. The job's environment is
  the caller's less `DISPLAY`, `WAYLAND_DISPLAY`, `DBUS_*` and the rebound
  `SSH_AUTH_SOCK`, plus `OFFLOAD_JOB=1`.

- Stdin: without `--stdin` the job reads EOF and the caller's stdin is never read, so a
  `while read` loop over `offload` calls keeps its input. `--stdin` appends the caller's
  stdin to the block, read only once `go` is sent, so a fallback runs on it unread; it reads
  to EOF, leaving nothing for a reader after the wrapper, and is refused (exit 2) unless fd
  0 is a pipe or a regular file (`checkStdin`).

- Every launcher step after `go` that fails before the job runs (the pivot, entering the
  cwd) prints `offload-launch: ...; the job did not run` and exits 125, which the wrapper
  reports as the run's code; it is not re-run locally. `offload-wait` writes `1` to fd 13
  (`$ctl/started`) once the job's command runs and `2` once it and its stragglers have
  ended; a launcher that ends with that file empty reports
  `starting the job failed (exit N)` and `status exit 125` (`status signal`, 128+n, after a
  forwarded signal).

- The launcher's arguments are the dev address, uid, gid, groups, cwd, the writable and
  read-only lists (newline-separated), the forward sockets, then `--` and the command.

- Caps: the job's scope has `CPUQuota` (default threads x 100% less one core) and
  `MemoryMax` (default 75% of RAM), overridable per remote in `offload.conf`.

Accepted limits of a remote job:

- A per-user rlimit count (`nproc`, `sigpending`, `msgqueue`) counts the caller's
  processes on the remote, not on dev.

- The hostname is the remote's, deliberately: a lock file that records a hostname and pid
  (git's `gc.pid`, Emacs and Vim locks) then names the machine that pid is on.

- Each writable path is a mount of its own, so a rename or hard link between two of them
  fails with EXDEV, and moving or removing a writable root with EBUSY; `mv` copies on
  EXDEV and is unaffected.

- Processes the job leaves running are killed as it ends, since the pid namespace's end
  kills them; `offload-wait` names them on stderr (`offload: the job left processes
running, stopped as it ends ...`) and keeps the exit code.

- `XDG_RUNTIME_DIR` keeps dev's value and names nothing on the remote, so
  `systemctl --user` fails with "Failed to connect".

- A caller whose own `/` or `/tmp` is read-only does not pass that to the job, which can
  still write its writable paths there.

### Writeback

- `offload-wait`, the job's parent (run as the job's user; opened on fd 8 before the pivot and run
  as `/proc/self/fd/8`, so it loads nothing over NFS), opens a
  descriptor on each filesystem of the writable paths before the job starts and calls
  `syncfs` on each once it ends. A write that failed at writeback (ENOSPC, EDQUOT, EIO, a
  file made immutable on dev) is reported there and nowhere else: `offload: writing to dev
failed under <paths>: <error>; files the job wrote there may be incomplete`, turning exit
  0 into 1 and keeping a job's own non-zero code.

  Accepted limit: the NFS client reports ENOSPC as itself and every other writeback error
  (EDQUOT, EPERM) as EIO.

- It writes n to fd 9 (`$ctl/signal`) for a job killed by signal n, so the launcher
  reports `status signal <n>`. Its init duties (reaping, stragglers) are
  `cmd/offload-wait/main.go`'s.

### Silly-rename sweep

- After a remote job whose scope stopped on an end it did not choose (exit 128 or more,
  grace kill, OOM, watchdog, lost launcher; `outcome.sweep`), the wrapper deletes the
  kernel's `.nfs` plus 24-hex-digit files from the job's repository and dev's `/tmp` and
  `/var/tmp` with a local `find -xdev` (`sweep.go`, 30 s bound). No sweep follows lost
  contact, and other writable roots are not swept (walking `~/code` costs about 10 s).

- Every wrapper whose job may run on a remote holds `/run/lock/offload-jobs.lock` shared
  until it exits; the sweep runs only when it can take it exclusively at once. A wrapper
  waits for the shared hold 40 s at most (`jobsLockWait`), then runs the job locally (rule
  `jobs-lock`). Lock-file creation and the sweep's signal handling are `sweep.go`'s.

- `apps/claude-user-level/install.sh` puts `.nfs*` in git's global ignore file.

- Accepted limit: while a file is open its `.nfs` name keeps the directory non-empty, so a
  job that removes a directory holding a file it still has open fails there (`rm -r`:
  Directory not empty) where a local run succeeds.

### Network

- Each job runs in a network namespace of its own, created after the NFS mounts (so file
  traffic never crosses the NAT), joined to the remote by a veth pair (`offv<N>` on the
  host, `eth0` in the job, a /30 inside `10.200.0.0/16`). Veth plus NAT, not pasta: run as
  root, pasta drops to `nobody` and cannot attach to the namespace.

- The only persistent state is `offload-net.service`: it sets `ip_forward`, masquerades
  `10.200.0.0/16`, and loads `inet offload_job` and `ip offload_nat`, which reject what a
  job sends to the remote, its containers or another job. Its forward chain resets every forwarded TCP connection to port
  2049, since masqueraded containers keep privileged source ports that dev's `sec=sys`
  export trusts; a container on the host network is not covered. The launcher checks
  `nft list table inet offload_job` before `ready` and exits when it fails.

- Accepted limits: IPv4 only; DNS is dev's `/etc/resolv.conf` through the NAT, so a
  loopback stub resolver on dev would not answer.

- Dev's loopback: the wrapper adds an `ssh -R /tmp/offload-<id>-<port>.sock:<dev
address>:<port>` for each dev TCP listener (`parseListeners`), and `offload-forward`
  relays the job's `127.0.0.1:<port>` to it. A port with no dev listener, and a port only
  the remote listens on, is refused; a listener that starts on dev after launch is not
  forwarded.

- ssh-agent: the wrapper adds `ForwardAgent=yes` when `SSH_AUTH_SOCK` is set; the launcher
  binds the agent socket at `/run/ssh-agent.sock` and sets `SSH_AUTH_SOCK` there.

### Shutdown with dev down

- A job process exiting with a file open on dev blocks in the NFS CLOSE call, which
  neither SIGKILL nor a `soft` mount ends. Every job scope is ordered
  `Before=offload-cut.service`, whose stop runs `offload-launch --cut`: it writes `1` to
  `/sys/fs/nfs/<maj:min>/shutdown` (Linux 6.4+) for each NFS superblock a job scope has
  mounted that the host's mount namespace does not hold, and exits non-zero after naming
  any it could not cut.

- Accepted limit: a job scope stopped on its own, as the watchdog does, still waits for
  dev to answer.

### Output

- The wrapper reads fd 1 and fd 2 once into a stdio plan (`readStdio`), passed as
  `OFFLOAD_STDIO`, one line per stream that is not a plain relay:
  - `err merged` — fd 1 and fd 2 are one file (`sameFile`) and, for a regular file, one
    open file description (`sameOpenFile`); the job runs with stderr on stdout.
  - `<out|err> file <[r]a|[r]w> <offset> <path>` — the stream is a regular file the remote
    job writes itself, so a job that seeks it, reopens `/dev/stdout` or reads back what it
    wrote gets the file a local run leaves; the wrapper then seeks the caller's descriptor
    to the reported `offset` (`settle`). A file another process also holds
    (`heldElsewhere`) is relayed instead; wrapper messages are ordered after the job's
    output (`heldOutput`, `appendTo`). One that will not open on the remote fails the run
    with 125 before the job.

- Any other stream is relayed over its ssh channel through `offload-wait`'s per-stream
  pipes. A caller's reader gone (`| head -1`) or any refused write sends `eow out|err`
  (`watchReader`, `control.go`); the launcher records it in `$ctl/eow` and sends
  signal 63 to the job half leader's children (not SIGPIPE, which Go drops on a thread
  running no goroutine). The leader ignores 63, so a setup child (`sleep`, `mount`) lives
  on; `offload-wait` catches it and closes that relay, so the job's next write fails with EPIPE and
  nothing in the job is signalled.

  A refused write other than EPIPE prints `writing the job's output failed: <error>` and
  keeps the job's exit code. A write past the caller's file-size limit ends the run by
  SIGXFSZ unless the caller ignores it.

- After the `status` record the wrapper waits for the job's ssh stream to end with no time
  limit; with no status, 5 s. If ssh's exit code differs from the status's, or the stream
  never ended, it prints that the output may be incomplete and exits non-zero (255 when
  the job's own code was 0); a signal that ends the wait makes it die by that signal
  (128+n) unless a death already stands, as Exit decision says.

Accepted limits of output:

- Under dash, which applies a simple command's redirection before forking, an output file
  is relayed unless offload is exec'd; bash does not. A holder whose `/proc/<pid>/fd` the
  caller may not read goes unseen by `heldElsewhere`.

- An `a` file is appended over NFS from the remote's cached size: a write dev makes to the
  same file during the job may be overwritten, which a local append never is.

- Writes the job makes in the round trip before a relay closes still reach the remote's
  pipe, which a local run would not have allowed.

- A refused relay write ends the job's stream with EPIPE (141 for a job SIGPIPE ends), not
  the error a local run would see: a job that tells them apart (`yes` exits 1 on ENOSPC)
  ends otherwise than locally.

### Control channel

- Besides the job's ssh session the wrapper opens a second one running
  `offload-launch --control <id>`, which bridges the root-only fifos
  `/run/offload-ctl/<id>/{in,out}` to its stdin and stdout; the launcher gets the id as
  `OFFLOAD_CTL`. The bridge writes `attached`, and the launcher never starts the job
  without it.

- Launcher to wrapper, one newline-ended line each (a last line the session cut is
  dropped): `hb` (every 2 s), `noslot` (no free slot, nothing
  ran), `ready` (set up, before `pivot_root`), `started` (just before the job's exec),
  `cpu <ms>`, `offset out|err <n>` per output file the job wrote itself, then one status:
  - `exit <n>`;
  - `signal <n>` — the job died by signal n;
  - `oom-kill` — the job half ended by an unexplained SIGKILL and systemd's `Result` says
    OOM;
  - `graced` — the monitor killed the job `grace` seconds after a forwarded signal, and
    that SIGKILL ended it;
  - `unrun` — the job never passed its wait for `go` (`$ctl/released`);
  - `watchdog` — the monitor stopped a running job after 10 s with no `ping`;
  - `launcher-lost` — the host-side launcher, or its monitor, died while the job ran.

- Wrapper to launcher: `go` (once, on `ready`), `ping` (every 2 s),
  `sig INT|TERM|HUP|QUIT|USR1|USR2|TSTP|CONT`, `eow out|err`, and `eof` (the bridge writes
  it when the wrapper's stdin closes). The job runs only after `go`; with no `go` (`eof` or
  the watchdog) the scope is stopped and nothing ran.

- Exit decision (`decide`, `control.go`, pure and table-tested): the records, not ssh's
  exit code, decide. Before `go` is sent nothing ran, and the wrapper runs the command
  locally. Once `go` is sent the job may be running, so lost contact exits 255 with a
  "writes may still land on dev" warning and is never re-run. `unrun` alone allows a local
  run after `go` (128+n when a signal was forwarded; 255 when `--stdin` had begun flowing).

  `watchdog` and `launcher-lost` exit 255 with a message. `graced` kills the wrapper by the
  forwarded signal and says the output on dev may be partial. Otherwise the exit code is
  the job's; a job killed by signal n (an OOM kill as SIGKILL) kills the wrapper by n too
  (`statusCode`, `exitAs`), without a core or Go stack trace (`die_by`, `quitQuietly`).

  A signal that ends the wait for the output after a status other than `unrun` reaches no
  job, so the wrapper dies by it (128+n), as a local command cut mid-write would, unless
  the run already dies by a signal: the status's, or SIGXFSZ at a relayed write.

- Accepted limit: `graced` is reported also when the job's own process had exited and only
  processes it left outlived the grace, where a local run would have returned its code
  and left them running.

### Signals and grace

- The job half runs under `setsid`, so its group is the job's own; its leader is outside
  the job's pid namespace, so `kill -- -$$` names no group, as under a shell without job
  control. A forwarded SIGINT, SIGTERM, SIGHUP or SIGQUIT goes to that group,
  and the scope is killed `grace` seconds later (default 10) if the job is still there; a
  job whose end mark is written is spared, so the grace never cuts the output drain.

- SIGUSR1 and SIGUSR2 once `go` is out reach the group and the run goes on; before `go`
  the wrapper sends `sig TERM` and exits 128+n, nothing run, never falling back locally. Of several ending signals the
  first sets 128+n; each is still forwarded. The wrapper sends no `go` once it has
  forwarded a signal.

- A signal before the pid exists makes the job exit 130 before `pivot_root`; one that
  reaches `offload-wait` before it starts the job ends the run as 128+n with the job unrun
  (SIGUSR1 and SIGUSR2 are dropped there), and each that lands after that check, while it
  forks the job, is passed on to it.

  Accepted limit: a signal that lands in the instant after the job's exec but before its
  fork returns reaches the job twice, from its group and again from `offload-wait`'s
  re-send, since a group signal before the fork misses the child and one after the exec does
  not, and the two cannot be told apart.

- The monitor appends each ending signal's name to `$ctl/abort-sig`, which `offload-wait`
  reads on fd 10 in send order. A job that leaves the group (`timeout`, `setsid`) gets
  each signal from `offload-wait` directly (`passOn`). Both ssh clients run in groups of
  their own, so a terminal Ctrl-C reaches only the wrapper.

- A local run with a controlling terminal keeps the command in the wrapper's process
  group, so a `/dev/tty` read works and the terminal's signals reach it directly; the
  wrapper follows its stops (`waitStopping`, `local.go`). Otherwise the command leads a
  group of its own and the wrapper forwards to it.

- A signal the caller ignores (`nohup offload`) or blocks stays so for the command, local
  or remote (`sigign.go`, `keepInheritedIgnores`; remotely `OFFLOAD_IGNORE`,
  `OFFLOAD_BLOCK`). The wrapper needs cgo and its default external link.

- The command has `Pdeathsig` SIGKILL (`local.go`), so a wrapper killed outright takes it
  with it.

- Ctrl-Z: the wrapper sends `sig TSTP`, stops itself and on `fg` sends `sig CONT`; the
  launcher stops the job's group with SIGSTOP (an orphan group discards TSTP), resumes it
  with SIGCONT, and pauses its silence watchdog between the two.

Accepted limits of signals:

- A Ctrl-C while a remote's setup hangs takes effect only once `setupLimit` (60 s) gives
  up on that remote.

- On a local run with a terminal, a SIGINT, SIGHUP or SIGQUIT sent to the wrapper's pid
  alone is dropped: the wrapper cannot tell it from the terminal's, and forwarding would
  deliver every Ctrl-C twice.

- `Pdeathsig` does not reach the command's own children; `review.py` gives up on its pipes
  10 s after its SIGKILL and fails loudly.

- A caller that dies while stopped sends no `eof`; the remote's sshd ends the channel
  instead (`ClientAliveInterval` on the server, 60 s × 3, else TCP keepalive after about
  2 h), which ends the job: the paused launcher watches no silence.

### Liveness

- ssh runs with `ServerAliveInterval=5`, `ServerAliveCountMax=3` and `ConnectTimeout=5`.
  The wrapper treats 10 s of silence on the channel (30 s before the first record) as a
  lost remote, and waits as long for the status once the job's ssh has ended. A job's ssh
  that ends before `go` gets 2 s (`drainWait`) for a `noslot` or `unrun` in flight. A
  launcher that reaches no `ready` within 60 s (`setupLimit`, stopped time excluded) is
  given up on and the job runs here.

- The launcher kills the scope after 10 s without a `ping` or on `eof`, and gives up 30 s
  after `ready` with no `go` while not paused; a dev hard-stop removes the job within 15 s,
  and the launcher's cleanup waits at most 5 s on `systemctl stop` of a scope a dead
  export holds.
  The monitor and the launcher watch each other: either dying stops the scope and reports
  `launcher-lost` (`unrun` before `released`). Once the job half is reaped, the launcher
  ignores HUP, INT and TERM, so it still reports the job's end and exits with its code.

- A lost remote is reported with "writes may still land on dev": NFS writes already sent
  can still arrive until the scope is gone and the client flushes.

### Benchmark

- `scripts/benchmark [--reps N] [--label TEXT] [--skip-small-files]` runs the cold
  `go test -count=1 ./...` of the Offload module, interleaved: dev lightly loaded, local in a
  `CPUWeight=10` user scope under a synthetic load (36 busy-loop processes), and offloaded
  under the same load, each with an empty `GOCACHE` per run. It then
  times `npm ci` (offline, warmed cache) and `git status` local versus offloaded.

  The table has wall and CPU time, dev's runnable count and load average, CPU pressure (mean of
  `some avg10`) and the nfsd CPU time; the verdict is PASS when the offloaded median beats
  the batch-scope median by 30%. Records go to the repository's `tmp/offload-benchmark/`.

  Dev is shared, so compare medians and read `bg_run`.

## Work Guidance

- A job sees dev's `HTTP(S)_PROXY` work as on dev: do not clear it in tests or scripts.

- **Review stop rule**: a finding blocks sign-off only if it can lose or corrupt data, break
  or wrongly fail a job that works on dev, or let a hard refusal fail to refuse. A
  command-reader miss blocks only for a form an AI agent would plausibly type; any other
  spelling (`npm --no-no-registry`, an option buried in a contrived shell shape) is a
  non-blocking suspicion, batched into one issue. A watch-serve or build-on-dev misread
  never blocks.

## Verification

- `scripts/check` runs the unit tests in `src/`. `protocol_test.go` reads the control
  records, signal names and stdio plan each of the wrapper, `offload-launch` and
  `cmd/offload-wait/main.go` uses from their source, so a word added on one side alone fails there;
  `signals_test.go` runs the launcher's own command-start block against `commandWords`'
  table.
- Unit tests that need to reach the symlink or lock checks can use `testUnexportedMounts`
  to model an exported workspace and `/tmp` with a private `/dev/shm`. They still exercise
  real links and locks; `mounts_test.go` tests the mount classification separately.

- `scripts/integration` runs `tests/integration/`: each case runs one command locally
  and through `offload --where server`, then compares exit code, stdout, stderr and the
  files written (contents, owners). It needs `install.sh` run and the server reachable.

  Set `OFFLOAD_TEST_DEV_ADDR` and `OFFLOAD_TEST_SERVER_ADDR` to the IPv4 addresses in
  `config/offload.conf`. The container isolation tests use a running `node-exporter`
  container; the cache-isolation test needs `OFFLOAD_TEST_GO_WORKLOAD` set to a Go module
  directory with dependencies.
  The setup tests run `install.sh` and `uninstall.sh` from the checkout under test, and stop
  with a message when a job is already running on the server. Setup
  relinks `~/.local/bin/offload` and replaces the server's launcher with that checkout's.

  `OFFLOAD_TEST_LAUNCHER=<path on the server>` builds the test binaries against another
  launcher, so a branch can be tried without replacing the one other branches use (push the
  branch's `offload-launch` to a path of its own with `sudo install`). The
  link-drop test blackholes dev with `iptables` on the server from a detached server-side
  script that lifts the rules itself.

  Test builds look at no process's open files (`-X main.fdGlob=/nonexistent/*`) so a WAL
  database some other dev process holds under `~/code` cannot refuse a test job; the WAL
  tests point the glob at their own holder.

  `gatherFacts` runs the `unseen-holder` check only on the full scan (`allFds`), since a
  narrowed scan cannot tell a hidden holder from one it skipped. The
  mount test mounts a tmpfs under a scratch repository with `sudo mount` and flushes nfsd's
  export cache (`exportfs -f`) before its `umount`.
