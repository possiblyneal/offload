package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// jobLinks lists the symlinks a job may follow without naming them: every one under the
// cwd (walkLinks), and the ones the repository tracks at any depth (git's index). It says
// why when it could not see every link under the cwd.
func jobLinks(cwd, repo string) ([]string, string) {
	links, err := walkLinks(cwd)
	problem := ""
	if err != nil {
		problem = fmt.Sprintf("offload could not see every link under the cwd, which the job may follow (%v)", err)
	}
	if repo == "" {
		// git also fails in a repository it refuses (dubious ownership): only "not a git
		// repository" means there are no tracked links to follow.
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		cmd.Dir = cwd
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil && !strings.Contains(stderr.String(), "not a git repository") {
			problem = strings.TrimPrefix(problem+"; ", "; ") + fmt.Sprintf("offload could not tell whether the cwd is in a repository, whose tracked links the job may follow (%v: %s)", err, firstLine(stderr.String()))
		}
		return links, problem
	}
	out, err := exec.Command("git", "-C", repo, "ls-files", "-s", "-z").Output()
	if err != nil {
		problem = strings.TrimPrefix(problem+"; ", "; ") + fmt.Sprintf("offload could not list the links the repository tracks, which the job may follow (%v)", err)
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		if meta, path, ok := strings.Cut(rec, "\t"); ok && strings.HasPrefix(meta, "120000 ") {
			if p := filepath.Join(repo, path); !slices.Contains(links, p) {
				links = append(links, p)
			}
		}
	}
	return links, problem
}

// linkWalkLimit bounds the entries walkLinks reads; a var so a test can lower it.
var linkWalkLimit = 10_000_000

// linkWalkWorkers is how many directories walkLinks reads at once: NanoKVM-OS's six
// million entries, a build's output, take about 3 s this way, and 50 s one at a time.
const linkWalkWorkers = 64

// walkLinks lists the symlinks under root, sorted, on root's own filesystem: a mount
// below it is one the export does not carry, judged as such. It skips a directory the job
// cannot enter, which holds no link the job could follow, and one removed meanwhile, and
// fails on any other directory it cannot read and past linkWalkLimit entries.
func walkLinks(root string) ([]string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(root, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", root, err)
	}
	var (
		mu      sync.Mutex
		ready   = sync.NewCond(&mu)
		queue   = []string{root}
		busy    int
		entries int
		links   []string
		failed  error
		wg      sync.WaitGroup
	)
	for range linkWalkWorkers {
		wg.Go(func() {
			mu.Lock()
			defer mu.Unlock()
			for {
				for len(queue) == 0 && busy > 0 && failed == nil {
					ready.Wait()
				}
				if len(queue) == 0 || failed != nil {
					ready.Broadcast()
					return
				}
				dir := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				busy++
				mu.Unlock()
				found, subdirs, n, err := readLinkDir(dir, st.Dev)
				mu.Lock()
				busy--
				entries += n
				links, queue = append(links, found...), append(queue, subdirs...)
				switch {
				case failed != nil:
				case err != nil:
					failed = err
				case entries > linkWalkLimit:
					failed = fmt.Errorf("more than %d entries under %s", linkWalkLimit, root)
				}
				ready.Broadcast()
			}
		})
	}
	wg.Wait()
	slices.Sort(links)
	return links, failed
}

// readLinkDir reads one directory of walkLinks: the links in it, its subdirectories, and
// how many entries it holds; none when it is on another device than dev.
func readLinkDir(dir string, dev uint64) (links, subdirs []string, n int, err error) {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) && syscall.Access(dir, 1) != nil {
			return nil, nil, 0, nil // removed meanwhile, or one the job cannot enter (X_OK)
		}
		return nil, nil, 0, err
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return nil, nil, 0, fmt.Errorf("%s: %w", dir, err)
	}
	if st.Dev != dev {
		return nil, nil, 0, nil
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%s: %w", dir, err)
	}
	for _, e := range entries {
		switch p := filepath.Join(dir, e.Name()); {
		case e.Type()&fs.ModeSymlink != 0:
			links = append(links, p)
		case e.IsDir():
			subdirs = append(subdirs, p)
		}
	}
	return links, subdirs, len(entries), nil
}

// cwdLinkRefusal names a link (jobLinks) onto, or holding, a mount the export does not
// carry, or into a private directory or onto dev's own machine (hostOnly), or "". A build
// or a script follows it without naming it (`make`, `sh -c 'cat conf/x'`), and the remote
// would read what the mount hides on dev's root, or the remote's own of the rest. Each is
// judged by the link's target as written and by its real path. A link into a private
// directory whose target's directory dev lacks, as build output keeps for the system it
// builds (`kvm/state -> /run/nanokvm/state`), leads nowhere on dev or in the job's empty
// private directory, and a job that makes the directory and writes there fails as it ends.
func cwdLinkRefusal(mounts, links []string) string {
	for _, p := range links {
		q := realPath(p)
		if within(privateDirs, q) != "" {
			if _, err := os.Lstat(filepath.Dir(q)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		if m := within(mounts, q); m != "" {
			return fmt.Sprintf("link %s leads onto dev's %s mount, which is not exported", p, m)
		}
		if m := holding(mounts, q); m != "" {
			return fmt.Sprintf("link %s holds dev's %s mount, which is not exported", p, m)
		}
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		target = filepath.Clean(target)
		for _, r := range []string{target, q} {
			if m := within(privateDirs, r); m != "" {
				return fmt.Sprintf("link %s leads under %s, which the job sees as a private directory removed when it ends", p, m)
			}
			if h := hostOnlyAs(target, r); h != "" {
				return fmt.Sprintf("link %s leads to %s, which a remote has its own of", p, h)
			}
		}
	}
	return ""
}

// firstLine is s up to its first newline: git's error, without its advice.
func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}
