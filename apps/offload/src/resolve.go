package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

// maxWildcardDepth bounds how many directories an allow rule's `**` descends, so launch
// stays fast.
const maxWildcardDepth = 4

// unlimitedDepth is the `**` bound for deny rules: a protected file the search misses
// would be writable, so a deny rule is never cut short.
const unlimitedDepth = 1 << 30

// pruned directories are never entered by a bounded `**` search; a deny rule's, being
// unbounded, enters them too.
var pruned = map[string]bool{".git": true, "node_modules": true}

// jobPaths is what a job may write (every path bound read-write from dev) and the
// paths inside those that stay read-only (denied files and directories that exist).
// sharedTmp are dev's temporary directories, writable to every job: install.sh exports
// dev's /tmp (a tmpfs) beside /, and /var/tmp is on the root filesystem.
var sharedTmp = []string{"/tmp", "/var/tmp"}

type jobPaths struct {
	writable []string
	readOnly []string
}

// resolvePaths turns the `Edit(...)` rules of Claude Code settings into the job's
// paths. repos are the git toplevel of the cwd and its common git dir, which a linked
// worktree keeps outside it; none outside a repository, and "" stands for none. fsys is dev's
// filesystem rooted at "/", used only to find what a wildcard names and what exists.
//
// Rule forms: `~/x` is relative to home, `//x` is absolute, and a bare, `./` or `/x`
// path is relative to home, the folder holding the settings file. Dev's /tmp and /var/tmp
// are always writable (sharedTmp), as they are to a local run.
func resolvePaths(settingsJSON []byte, home string, repos []string, fsys fs.FS) (jobPaths, error) {
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(settingsJSON, &s); err != nil {
		return jobPaths{}, err
	}

	var writable []string
	for _, r := range repos {
		if r != "" {
			writable = append(writable, r)
		}
	}
	for _, rule := range s.Permissions.Allow {
		// A rule with a leading ** (`//**/.claude/worktrees/**`) would be searched under the
		// other writable roots only, so all it finds is inside one of them and collapse drops
		// it: it is not searched at all (path.Clean leaves no `..` that could lead out).
		if pat, ok := editPattern(rule, home); ok && !strings.HasPrefix(pat, "/**/") {
			writable = append(writable, expand(fsys, pat, nil, maxWildcardDepth)...)
		}
	}
	// A deny rule's `**` is searched under the settings' own paths, not under /tmp.
	denyRoots := collapse(writable)
	if slices.Contains(denyRoots, "/") {
		// Every deny rule's `**` would then be searched over the whole disk, unbounded,
		// on every launch.
		return jobPaths{}, errors.New("an Edit rule makes / writable; offload cannot bound its deny search, so narrow the rule")
	}
	writable = collapse(append(writable, sharedTmp...))

	var readOnly []string
	for _, rule := range s.Permissions.Deny {
		pat, ok := editPattern(rule, home)
		if !ok {
			continue
		}
		for _, p := range expand(fsys, pat, denyRoots, unlimitedDepth) {
			if under(p, writable) {
				readOnly = append(readOnly, p)
				continue
			}
			// A deny on an ancestor of a writable path covers it too.
			for _, w := range writable {
				if under(w, []string{p}) {
					readOnly = append(readOnly, w)
				}
			}
		}
	}
	return jobPaths{writable: writable, readOnly: collapse(readOnly)}, nil
}

// editPattern returns the absolute glob an `Edit(<glob>)` rule names.
func editPattern(rule, home string) (string, bool) {
	glob, ok := strings.CutPrefix(rule, "Edit(")
	if !ok {
		return "", false
	}
	glob, ok = strings.CutSuffix(glob, ")")
	if !ok || glob == "" {
		return "", false
	}
	switch {
	case strings.HasPrefix(glob, "//"):
		return path.Clean(glob[1:]), true
	case strings.HasPrefix(glob, "~/"):
		return path.Join(home, glob[2:]), true
	default: // bare, ./x and /x: relative to the folder holding the settings file
		return path.Join(home, glob), true
	}
}

// expand lists the existing paths a glob names. A trailing `/**` names the directory
// itself. A glob that starts with `**` is searched under each of roots instead of "/".
func expand(fsys fs.FS, pat string, roots []string, limit int) []string {
	segs := strings.Split(strings.Trim(pat, "/"), "/")
	if segs[len(segs)-1] == "**" {
		segs = segs[:len(segs)-1]
	}
	if len(segs) > 0 && segs[0] == "**" {
		var out []string
		for _, r := range roots {
			out = append(out, walk(fsys, r, segs, 0, limit)...)
		}
		return out
	}
	return walk(fsys, "/", segs, 0, limit)
}

// walk matches segs below base. depth counts the directories a `**` has entered.
func walk(fsys fs.FS, base string, segs []string, depth, limit int) []string {
	if len(segs) == 0 {
		if _, err := fs.Stat(fsys, fsName(base)); err != nil {
			return nil
		}
		return []string{base}
	}
	seg, rest := segs[0], segs[1:]
	switch {
	case seg == "**":
		out := walk(fsys, base, rest, depth, limit)
		if depth < limit {
			for _, e := range readDirs(fsys, base) {
				if e.IsDir() && (limit == unlimitedDepth || !pruned[e.Name()]) {
					out = append(out, walk(fsys, path.Join(base, e.Name()), segs, depth+1, limit)...)
				}
			}
		}
		return out
	case strings.ContainsAny(seg, "*?["):
		var out []string
		for _, e := range readDirs(fsys, base) {
			if ok, _ := path.Match(seg, e.Name()); ok {
				out = append(out, walk(fsys, path.Join(base, e.Name()), rest, depth, limit)...)
			}
		}
		return out
	default:
		return walk(fsys, path.Join(base, seg), rest, depth, limit)
	}
}

func readDirs(fsys fs.FS, dir string) []fs.DirEntry {
	entries, _ := fs.ReadDir(fsys, fsName(dir))
	return entries
}

// fsName is the io/fs spelling of an absolute path: no leading slash, "." for the root.
func fsName(p string) string {
	if p = strings.TrimPrefix(p, "/"); p != "" {
		return p
	}
	return "."
}

// collapse sorts and dedupes paths, dropping any inside another in the list.
func collapse(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if !under(p, out) {
			out = append(out, p)
		}
	}
	return out
}

// under reports whether p is one of roots or inside one.
func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}
