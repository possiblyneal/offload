package main

import "fmt"

// slotState is how many jobs a remote accepts at once and how many of those are free.
type slotState struct {
	Total, Free int
	Unknown     string // why --explain's probe could not read the state, which is then assumed
}

// refusalFacts are what dev observed about the job. A non-zero field is a refusal to
// offload; WatchServe, Tmpfs and WAL carry the evidence the output names.
type refusalFacts struct {
	Tty         bool   // stdin is a terminal
	Sudo        bool   // the command runs sudo
	WatchServe  string // the config/local-only entry the command matches
	Tmpfs       string // why the cwd, TMPDIR or an argument is on a tmpfs or FUSE mount
	RemoteLocal string // why the cwd or an argument names a path under a remote-local path
	WAL         string // a -wal or -shm file a dev process holds open, of a database the job may use
	WALBare     string // a WAL database a dev process holds by its main file only, under a writable path
	Flock       string // a file a dev process holds a flock on, that the job may lock
	Fd          string // a descriptor above 2 the caller passed, which a remote job would not have
	ClosedStdio string // stdin, stdout or stderr, when the caller passed it closed
	OutputFile  string // why a remote job could not write the caller's output file as a local run would
	TooLong     int    // the remote command line's length, when ssh cannot carry it; else 0
	Root        bool   // the caller is root or in group 0, which dev's export squashes to nobody
	Special     string // why an argument names a FIFO, socket or device node on dev
	Unseen      string // why a lock on a writable path's filesystem has no holder offload sees
	Foreign     string // a -wal, -shm or flocked file held from another mount namespace, on a writable path's filesystem
	Newline     string // a path the launcher would get that holds a newline, which its lists cannot carry
	Groups      int    // the caller's supplementary group count, when more than AUTH_SYS carries; else 0
	ReadOnlyCwd string // the cwd, when no writable path holds it
}

// placeRequest is everything place decides from. Where is "", "local" or a remote's
// name; Remotes is in the order of config/offload.conf.
type placeRequest struct {
	Where   string
	Remotes []string
	Slots   map[string]slotState
	Facts   refusalFacts
}

// decision is where the job runs ("local" or a remote's name) and the rule that decided it.
type decision struct {
	Placement string
	Rule      string
	Reason    string
}

const local = "local"

// maxNFSGroups is how many supplementary groups an AUTH_SYS credential carries; dev's nfsd
// sees no more, so a caller in more would be refused files a local run may read.
const maxNFSGroups = 16

// groupsOverNFS is how many supplementary groups the caller is in when NFS cannot carry
// them all, else 0.
func groupsOverNFS(groups []int) int {
	if len(groups) > maxNFSGroups {
		return len(groups)
	}
	return 0
}

// place decides where a job runs. Tmpfs, RemoteLocal, WAL, Flock, Foreign, Unseen,
// Special, TooLong, Fd, ClosedStdio, OutputFile, Root, Newline and Groups refusals apply
// under every --where (dev's export does not carry a tmpfs, a remote-local path the job
// names would be the remote's copy, a WAL database or a flock the job uses cannot be shared
// over NFS, nor one whose place on dev is unknown or whose holder offload cannot see, a
// FIFO, socket or device node would be the remote's own, ssh cannot run a command line it
// cannot pass, a descriptor does not cross machines, nor does a closed one stay closed, an
// output file the remote cannot open as dev's would be another file, root and group 0 are
// nobody on dev's export, the launcher's path lists end at a newline, and NFS carries at
// most 16 groups); the other refusals yield to a named host. A host still needs a free slot.
func place(r placeRequest) decision {
	if r.Where == local {
		return decision{local, "where-local", "--where local"}
	}
	hard, soft := refusals(r.Facts)
	if len(hard) > 0 {
		return hard[0]
	}
	if r.Where != "" {
		return takeSlot(r, []string{r.Where}, "where-remote")
	}
	if len(soft) > 0 {
		return soft[0]
	}
	return takeSlot(r, r.Remotes, "free-slot")
}

// refusals are the refusals that apply to f, each as the local decision it makes, in the
// order place weighs them: hard ones hold under every --where, soft ones yield to a named
// host. --explain prints them all.
func refusals(f refusalFacts) (hard, soft []decision) {
	add := func(to *[]decision, applies bool, rule, reason string) {
		if applies {
			*to = append(*to, decision{local, rule, reason})
		}
	}
	add(&hard, f.Tmpfs != "", "tmpfs", f.Tmpfs)
	add(&hard, f.RemoteLocal != "", "remote-local", f.RemoteLocal)
	add(&hard, f.WAL != "", "wal-open", "a dev process holds "+f.WAL+" open, and the job's directory, an argument or a variable holds or names its database")
	add(&hard, f.WALBare != "", "wal-bare", "a dev process holds the WAL database "+f.WALBare+" open before its first read, under a writable path; a remote reader would create its -shm file over NFS, which dev's process would then map with no coherence")
	add(&hard, f.Flock != "", "flock-held", "a dev process holds a flock on "+f.Flock+", which the job's directory holds or an argument or a variable names; a remote job's flock would not see it")
	add(&hard, f.Foreign != "", "foreign-holder", "a process in another mount namespace (a container) holds "+f.Foreign+" open or locked, under a writable path; a remote job would use it beside that holder")
	add(&hard, f.Unseen != "", "unseen-holder", "offload cannot see every holder of a lock on a writable path's filesystem ("+f.Unseen+"): the lock may be on a database or a file the job uses")
	add(&hard, f.Special != "", "special-file", f.Special)
	add(&hard, f.TooLong != 0, "too-long", fmt.Sprintf("the remote command line would be %d bytes, more than ssh carries (%d)", f.TooLong, maxCommandLine))
	add(&hard, f.Fd != "", "inherited-fd", f.Fd+" is open, and a remote job would not have it")
	add(&hard, f.ClosedStdio != "", "closed-stdio", "the caller passed "+f.ClosedStdio+" closed, and a remote job's would be open")
	add(&hard, f.OutputFile != "", "output-file", f.OutputFile)
	add(&hard, f.Root, "root", "the caller is root or in group 0, which dev's export (root_squash) treats as nobody")
	add(&hard, f.Newline != "", "newline-path", fmt.Sprintf("the path %q holds a newline, which the launcher's path lists cannot carry", f.Newline))
	add(&hard, f.Groups != 0, "groups", fmt.Sprintf("the caller is in %d supplementary groups, and NFS (AUTH_SYS) carries only %d", f.Groups, maxNFSGroups))
	add(&soft, f.Tty, "interactive-tty", "stdin is an interactive terminal")
	add(&soft, f.Sudo, "sudo", "the command runs sudo")
	add(&soft, f.WatchServe != "", "watch-serve", fmt.Sprintf("the command runs %q, which config/local-only keeps on dev", f.WatchServe))
	add(&soft, f.ReadOnlyCwd != "", "read-only-cwd", "the job could write nothing in "+f.ReadOnlyCwd+": no writable path holds it")
	return hard, soft
}

// takeSlot picks the first of the remotes with a free slot, or runs the job here.
func takeSlot(r placeRequest, remotes []string, rule string) decision {
	for _, name := range remotes {
		if r.Slots[name].Free > 0 {
			return decision{name, rule, fmt.Sprintf("%s has a free slot", name)}
		}
	}
	if len(remotes) == 1 {
		return decision{local, "no-free-slot", remotes[0] + " has no free slot"}
	}
	return decision{local, "no-free-slot", "no remote has a free slot"}
}
