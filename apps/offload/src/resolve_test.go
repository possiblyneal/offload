package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const home = "/home/testuser"

// tree builds a directory tree: a name ending in "/" is a directory, anything else a
// file. Parents are implied.
func tree(names ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			fsys[n[1:len(n)-1]] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		} else {
			fsys[n[1:]] = &fstest.MapFile{}
		}
	}
	return fsys
}

func settings(allow, deny []string) []byte {
	q := func(rules []string) string {
		quoted := make([]string, len(rules))
		for i, r := range rules {
			quoted[i] = fmt.Sprintf("%q", r)
		}
		return strings.Join(quoted, ",")
	}
	return []byte(fmt.Sprintf(`{"permissions":{"allow":[%s],"deny":[%s]}}`, q(allow), q(deny)))
}

func TestResolvePathsTurnsEditRulesIntoWritableAndReadOnlySets(t *testing.T) {
	fsys := tree(
		"/home/testuser/code/a/TASKS.md",
		"/home/testuser/code/b/TASKS.md",
		"/home/testuser/code/c/README.md",
		"/home/testuser/code/a/.claude/worktrees/w1/",
		"/home/testuser/code/b/.claude/worktrees/w2/",
		"/home/testuser/code/c/node_modules/p/.claude/worktrees/x/",
		"/home/testuser/code/a/d1/d2/d3/d4/d5/.claude/worktrees/toodeep/",
		"/home/testuser/orca/workspaces/a/sponge/.claude/worktrees/w3/",
		"/srv/data/",
		"/home/testuser/notes/",
		"/home/testuser/code/e/a/b/c/d/e/secret.key",
		"/home/testuser/code/e/a/ok.txt",
		"/home/testuser/code/e/node_modules/k/secret.key",
		"/tmp/held.db",
		"/tmp/x/secret.key",
	)
	cases := []struct {
		name     string
		allow    []string
		deny     []string
		repo     string
		writable []string
		readOnly []string
	}{
		{
			name:     "a home-relative rule, and a repository outside it",
			allow:    []string{"Edit(~/code/**)"},
			repo:     "/home/testuser/orca/workspaces/a/sponge",
			writable: []string{"/home/testuser/code", "/home/testuser/orca/workspaces/a/sponge"},
		},
		{
			name:     "a repository inside a rule is covered by it",
			allow:    []string{"Edit(~/code/**)"},
			repo:     "/home/testuser/code/a",
			writable: []string{"/home/testuser/code"},
		},
		{
			name:     "an absolute double-slash rule",
			allow:    []string{"Edit(//srv/data/**)"},
			writable: []string{"/srv/data"},
		},
		{
			name:     "a bare or dot path is relative to the home the settings live in",
			allow:    []string{"Edit(notes/**)", "Edit(./code/c/**)"},
			writable: []string{"/home/testuser/code/c", "/home/testuser/notes"},
		},
		{
			name:  "tmp rules add nothing: dev's /tmp and /var/tmp are always writable",
			allow: []string{"Edit(//tmp/**)", "Edit(//var/tmp/**)", "Edit(//tmp/x/**)"},
		},
		{
			name:     "a deny inside /tmp is read-only, and a deny's ** is not searched under /tmp",
			deny:     []string{"Edit(//tmp/held.db)", "Edit(//**/secret.key)"},
			readOnly: []string{"/tmp/held.db"},
		},
		{
			name:  "rules for other tools, and a bare Edit, are not path rules",
			allow: []string{"Read(~/code/**)", "Bash(ls:*)", "Edit"},
			deny:  []string{"Read(~/code/a/TASKS.md)"},
		},
		{
			name:     "a leading ** is searched under the repository only, and is already covered by it",
			allow:    []string{"Edit(//**/.claude/worktrees/**)"},
			repo:     "/home/testuser/orca/workspaces/a/sponge",
			writable: []string{"/home/testuser/orca/workspaces/a/sponge"},
		},
		{
			name:  "a wildcard inside a concrete prefix resolves to the directories that exist",
			allow: []string{"Edit(~/code/*/.claude/worktrees/**)"},
			writable: []string{
				"/home/testuser/code/a/.claude/worktrees",
				"/home/testuser/code/b/.claude/worktrees",
			},
		},
		{
			name:  "a ** search is bounded in depth and does not enter node_modules",
			allow: []string{"Edit(~/code/**/.claude/worktrees/**)"},
			writable: []string{
				"/home/testuser/code/a/.claude/worktrees",
				"/home/testuser/code/b/.claude/worktrees",
			},
		},
		{
			name:     "a denied file is read-only where it exists",
			allow:    []string{"Edit(~/code/**)"},
			deny:     []string{"Edit(~/code/*/TASKS.md)"},
			writable: []string{"/home/testuser/code"},
			readOnly: []string{"/home/testuser/code/a/TASKS.md", "/home/testuser/code/b/TASKS.md"},
		},
		{
			name:     "a denied literal path that is missing is left out",
			allow:    []string{"Edit(~/code/**)"},
			deny:     []string{"Edit(~/code/c/TASKS.md)", "Edit(~/code/a/TASKS.md)"},
			writable: []string{"/home/testuser/code"},
			readOnly: []string{"/home/testuser/code/a/TASKS.md"},
		},
		{
			name:     "a denied directory is read-only whole",
			allow:    []string{"Edit(~/code/**)"},
			deny:     []string{"Edit(~/code/b/**)"},
			writable: []string{"/home/testuser/code"},
			readOnly: []string{"/home/testuser/code/b"},
		},
		{
			name:     "a deny rule reaches a file far below its pattern, inside node_modules too",
			allow:    []string{"Edit(~/code/**)"},
			deny:     []string{"Edit(~/code/e/**/secret.key)"},
			writable: []string{"/home/testuser/code"},
			readOnly: []string{"/home/testuser/code/e/a/b/c/d/e/secret.key", "/home/testuser/code/e/node_modules/k/secret.key"},
		},
		{
			name:     "a deny that is an ancestor of a writable path makes it read-only",
			allow:    []string{"Edit(~/code/e/a/**)"},
			deny:     []string{"Edit(~/code/**)"},
			writable: []string{"/home/testuser/code/e/a"},
			readOnly: []string{"/home/testuser/code/e/a"},
		},
		{
			name:     "a denial outside every writable path changes nothing",
			allow:    []string{"Edit(~/code/**)"},
			deny:     []string{"Edit(//srv/data/**)"},
			writable: []string{"/home/testuser/code"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolvePaths(settings(c.allow, c.deny), home, []string{c.repo}, fsys)
			if err != nil {
				t.Fatal(err)
			}
			if want := collapse(append(c.writable, sharedTmp...)); !reflect.DeepEqual(got.writable, want) {
				t.Errorf("writable = %v, want %v", got.writable, want)
			}
			if !reflect.DeepEqual(got.readOnly, c.readOnly) {
				t.Errorf("readOnly = %v, want %v", got.readOnly, c.readOnly)
			}
		})
	}
}

// Dev's /tmp and /var/tmp are writable to every job, whatever the settings allow.
func TestResolvePathsAlwaysMakesTmpAndVarTmpWritable(t *testing.T) {
	got, err := resolvePaths(settings(nil, nil), home, nil, tree())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/tmp", "/var/tmp"} {
		if !slices.Contains(got.writable, d) {
			t.Errorf("writable = %v, want it to hold %s", got.writable, d)
		}
	}
}

func TestResolvePathsRejectsSettingsThatAreNotJSON(t *testing.T) {
	if _, err := resolvePaths([]byte("{"), home, nil, tree()); err == nil {
		t.Error("resolvePaths accepted broken JSON")
	}
}

// countingFS counts the directories read through it.
type countingFS struct {
	fs.FS
	reads *int
}

func (c countingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	*c.reads++
	return fs.ReadDir(c.FS, name)
}

// An allow rule with a leading `**` could only find paths inside the roots it is searched
// under, which are writable already, so resolving it reads no directory at all.
func TestResolvePathsReadsNoDirectoryForALeadingWildcardAllowRule(t *testing.T) {
	reads := 0
	fsys := countingFS{tree("/home/testuser/code/a/.claude/worktrees/w1/"), &reads}
	got, err := resolvePaths(settings([]string{"Edit(~/code/**)", "Edit(//**/.claude/worktrees/**)"}, nil), home, nil, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := collapse(append([]string{"/home/testuser/code"}, sharedTmp...)); !reflect.DeepEqual(got.writable, want) {
		t.Errorf("writable = %v, want %v", got.writable, want)
	}
	if reads != 0 {
		t.Errorf("read %d directories, want 0", reads)
	}
}

// A leading `**` descends exactly maxWildcardDepth directories below a root, no further.
func TestExpandSearchesALeadingWildcardToItsDepthExactly(t *testing.T) {
	fsys := tree(
		"/r/a/b/c/d/.claude/worktrees/",
		"/r/a/b/c/d/e/.claude/worktrees/",
	)
	got := expand(fsys, "/**/.claude/worktrees/**", []string{"/r"}, maxWildcardDepth)
	want := []string{"/r/a/b/c/d/.claude/worktrees"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A rule that makes all of / writable would have every deny rule searched over the whole
// disk on every launch, so it is refused loudly instead.
func TestResolvePathsRefusesARuleThatMakesTheRootWritable(t *testing.T) {
	_, err := resolvePaths(settings([]string{"Edit(//**)"}, []string{"Edit(//**/.env)"}), home, nil, tree("/x/.env"))
	if err == nil {
		t.Error("resolvePaths accepted a writable /")
	}
}

func TestUnderAndCollapseTreatTheRootAsHoldingEverything(t *testing.T) {
	if !under("/home/x", []string{"/"}) {
		t.Error(`under("/home/x", ["/"]) = false`)
	}
	if got := collapse([]string{"/tmp", "/", "/var/tmp"}); !reflect.DeepEqual(got, []string{"/"}) {
		t.Errorf("collapse = %v, want [/]", got)
	}
}

func TestScopedFilteringKeepsRepositoryAndDenyUnderUnsharedAncestor(t *testing.T) {
	repo := home + "/code/repo"
	denied := repo + "/secret"
	fsys := tree(denied)
	view := exportView{root: "/synthetic", shared: []string{home + "/code", "/tmp", "/var/tmp"}}
	excluded := func(p string) bool { return view.excludesWritable(nil, p) }
	paths, err := resolvePathsFiltered(settings([]string{"Edit(~/**)"}, []string{"Edit(~/code/repo/secret)"}), home, []string{repo}, fsys, excluded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePathsFiltered(settings([]string{"Edit(//**)"}, nil), home, []string{repo}, fsys, excluded); err == nil {
		t.Error("root guard lost")
	}
	if !slices.Contains(paths.writable, repo) {
		t.Errorf("repository not writable: %v", paths.writable)
	}
	if !slices.Contains(paths.readOnly, denied) {
		t.Errorf("deny lost: %v", paths.readOnly)
	}
}

func TestJobPathsForScopedRepositoryAndDeny(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "code", "repo")
	denied := filepath.Join(repo, "secret")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(denied, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), settings([]string{"Edit(~/**)"}, []string{"Edit(~/code/repo/secret)"}), 0600); err != nil {
		t.Fatal(err)
	}
	view := exportView{root: "/synthetic", shared: []string{filepath.Join(home, "code"), "/var/tmp"}}
	// Model the unshared home mount hiding the /tmp share except for the verified code bind.
	mounts := []string{home}
	paths, err := jobPathsFor(home, []string{repo}, mounts, view)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths.writable, repo) {
		t.Errorf("repository not writable: %v", paths.writable)
	}
	if !slices.Contains(paths.readOnly, denied) {
		t.Errorf("deny lost: %v", paths.readOnly)
	}
}
