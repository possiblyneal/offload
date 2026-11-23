package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// An LMDB environment opened with MDB_NOSUBDIR is a file and its -lock; a held -lock with no
// such file beside it is no database.
func TestMappedDatabaseFindsAnLMDBFileBesideItsLock(t *testing.T) {
	dir := t.TempDir()
	db := plant(t, dir, "store")
	if got := mappedDatabase(db + "-lock"); strings.Join(got, "|") != db+"|"+db+"-lock" {
		t.Errorf("mappedDatabase(%s-lock) = %q", db, got)
	}
	if got := mappedDatabase(filepath.Join(dir, "none-lock")); got != nil {
		t.Errorf("a -lock with no database beside it read as %q", got)
	}
	if got := mappedDatabase(filepath.Join(dir, "app.db")); got != nil {
		t.Errorf("a plain file read as %q", got)
	}
}

// sqliteDB writes a database header at p: journal mode WAL when wal, else rollback.
func sqliteDB(t *testing.T, p string, wal bool) {
	t.Helper()
	head := make([]byte, 100)
	copy(head, "SQLite format 3\x00")
	head[16], head[18], head[19] = 0x10, 1, 1
	if wal {
		head[18], head[19] = 2, 2
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, head, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A connection to a WAL database holds only the main file until its first read, so a held
// main file in WAL mode counts as the database; one in rollback mode does not.
func TestWALPlanProtectsAWALDatabaseHeldByItsMainFileOnly(t *testing.T) {
	root := t.TempDir()
	repo, other := filepath.Join(root, "repo"), filepath.Join(root, "other")
	mine, theirs, rollback := filepath.Join(repo, "app.db"), filepath.Join(other, "app.db"), filepath.Join(other, "old.db")
	sqliteDB(t, mine, true)
	sqliteDB(t, theirs, true)
	sqliteDB(t, rollback, false)
	held := func(ps ...string) []openFile {
		var out []openFile
		for _, p := range ps {
			out = append(out, openFile{path: p})
		}
		return out
	}
	if refuse, _ := walPlan(held(mine), []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil); refuse != mine {
		t.Errorf("a WAL database in the repository held by its main file: refusal %q", refuse)
	}
	refuse, ro := walPlan(held(theirs, rollback), []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil)
	if want := []string{theirs, theirs + "-wal", theirs + "-shm"}; refuse != "" || !slices.Equal(ro, want) {
		t.Errorf("another repository's WAL database: refusal %q, read-only %q, want %q", refuse, ro, want)
	}
}

// A database dev holds that the job would see read-only is kept here instead when one of
// its files has other names (hard links): the bind covers the name it is given only.
func TestWALPlanKeepsAHardLinkedDatabaseHere(t *testing.T) {
	root := t.TempDir()
	repo, other := filepath.Join(root, "repo"), filepath.Join(root, "other")
	db := filepath.Join(other, "app.db")
	sqliteDB(t, db, true)
	if err := os.WriteFile(db+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := []openFile{{path: db + "-wal"}}
	if refuse, ro := walPlan(held, []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil); refuse != "" || len(ro) != 3 {
		t.Fatalf("one name each: refusal %q, read-only %q, want none and the three files", refuse, ro)
	}
	if err := os.Link(db+"-wal", filepath.Join(root, "copy")); err != nil {
		t.Fatal(err)
	}
	want := db + "-wal (with 1 other names, hard links offload cannot find to make read-only)"
	if refuse, ro := walPlan(held, []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil); refuse != want || ro != nil {
		t.Errorf("a hard-linked -wal: refusal %q, read-only %q, want %q", refuse, ro, want)
	}
}

// A data directory guarded by a pid file that names a live dev process (PostgreSQL's
// postmaster.pid) counts as a database: a remote server would see no such process and take
// the data over.
func TestWALPlanProtectsADataDirectoryGuardedByALivePidFile(t *testing.T) {
	root := t.TempDir()
	repo, other := filepath.Join(root, "repo"), filepath.Join(root, "other")
	live := []byte(strconv.Itoa(os.Getpid()) + "\n/x/pgdata\n")
	write := func(p string, b []byte) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(filepath.Join(repo, "pgdata/postmaster.pid"), live)
	write(filepath.Join(other, "pgdata/postmaster.pid"), live)
	write(filepath.Join(other, "stale/server.pid"), []byte("2147483646\n"))
	held := []openFile{
		{path: write(filepath.Join(repo, "pgdata/base/1/1259"), nil)},
	}
	if refuse, _ := walPlan(held, []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil); !strings.Contains(refuse, "pgdata/base/1/1259") {
		t.Errorf("a held data directory in the repository: refusal %q", refuse)
	}
	theirs := []openFile{
		{path: write(filepath.Join(other, "pgdata/base/1/1259"), nil)},
		{path: write(filepath.Join(other, "pgdata/pg_wal/0001"), nil)},
		{path: write(filepath.Join(other, "stale/log"), nil)},
	}
	refuse, ro := walPlan(theirs, []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil)
	if want := []string{filepath.Join(other, "pgdata")}; refuse != "" || !slices.Equal(ro, want) {
		t.Errorf("another data directory: refusal %q, read-only %q, want %q", refuse, ro, want)
	}
	if refuse, _ := walPlan(theirs[:1], []string{root}, repo, repo, "/nonexistent", []string{"pg_ctl", "-D", filepath.Join(other, "pgdata"), "start"}, nil); refuse == "" {
		t.Error("a job naming another held data directory was not refused")
	}
	// The writable root is one too, and it holds the repository, so the job stays here.
	write(filepath.Join(root, "top.pid"), live)
	loose := []openFile{{path: write(filepath.Join(root, "loose.log"), nil)}}
	if refuse, _ := walPlan(loose, []string{root}, repo, repo, "/nonexistent", []string{"make"}, nil); refuse == "" {
		t.Error("a held data directory at the writable root, holding the repository, was not refused")
	}
	// A shared temporary directory never is: a stray pid file there must not take all of it.
	if got := pidDataDir("/tmp/offload-absent/f", "/tmp", map[string]bool{"/tmp": true}, devAlive); got != nil {
		t.Errorf("/tmp counted as a data directory: %q", got)
	}
}

// A pid file counts while its process is alive, another account's included (kill gives
// EPERM: a postgres server run by its own user), and only as a regular *.pid file.
func TestHoldsLivePidFile(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          bool
	}{
		{"own.pid", strconv.Itoa(os.Getpid()) + "\n", true},
		{"init.pid", "1\n", true}, // another account's: EPERM
		{"gone.pid", "2147483646\n", false},
		{"own.txt", strconv.Itoa(os.Getpid()) + "\n", false},
		{"zero.pid", "0\n", false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tt.name), []byte(tt.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := holdsLivePidFile(dir, devAlive); got != tt.want {
			t.Errorf("%s holding %q: %v, want %v", tt.name, tt.content, got, tt.want)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "x.pid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if holdsLivePidFile(dir, devAlive) {
		t.Error("a directory named x.pid counted as a pid file")
	}
}

// A WAL database a dev process holds open keeps the job here only when the job is
// likely to use it: under its cwd or repository, or named by an argument or a variable.
// Any other is bound read-only with its -wal and -shm, so a write fails instead of
// corrupting it over NFS.
func TestWALPlan(t *testing.T) {
	writable := []string{"/home/testuser/code", "/home/testuser/.cache/x"}
	other := []string{
		"/home/testuser/code/other/.claude/worktrees/w/agentmemory.sqlite",
		"/home/testuser/code/other/.claude/worktrees/w/agentmemory.sqlite-wal",
		"/home/testuser/code/other/.claude/worktrees/w/agentmemory.sqlite-shm",
	}
	tests := []struct {
		name      string
		targets   []string
		cwd, repo string
		argv, env []string
		refuse    string // substring of the refusal, "" for none
		readOnly  []string
	}{
		{"no open WAL", []string{"/dev/null", "/home/testuser/code/repo/app.db"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", nil},
		{"a WAL in the repository", []string{"/home/testuser/code/repo/data/app.db-wal"}, "/home/testuser/code/repo/sub", "/home/testuser/code/repo", []string{"make"}, nil, "/home/testuser/code/repo/data/app.db-wal", nil},
		{"a -shm in the repository", []string{"/home/testuser/code/repo/app.db-shm"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "app.db-shm", nil},
		{"a deleted -shm in the repository", []string{"/home/testuser/code/repo/x.db-shm (deleted)"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "x.db-shm", nil},
		{"a WAL under the cwd outside a repository", []string{"/home/testuser/code/scratch/a/app.db-wal"}, "/home/testuser/code/scratch", "", []string{"make"}, nil, "app.db-wal", nil},
		{"a WAL in another repository is read-only", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", other},
		{"its -shm names the same database", []string{other[1], other[2]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", other},
		{"a sibling of the repository is another repository", []string{"/home/testuser/code/repository/x.db-wal"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", []string{"/home/testuser/code/repository/x.db", "/home/testuser/code/repository/x.db-wal", "/home/testuser/code/repository/x.db-shm"}},
		{"a WAL outside the writable paths is read-only already", []string{"/home/testuser/notes/n.db-wal"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", nil},
		{"an argument names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sqlite3", other[0], "vacuum"}, nil, "agentmemory.sqlite-wal", nil},
		{"an argument names its -wal", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"cp", other[1], "x"}, nil, "agentmemory.sqlite-wal", nil},
		{"a relative argument names its -wal", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"cp", "../other/.claude/worktrees/w/agentmemory.sqlite-wal", "x"}, nil, "agentmemory.sqlite-wal", nil},
		{"a flag value names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"tool", "--db=" + other[0]}, nil, "agentmemory.sqlite-wal", nil},
		{"a relative argument names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sqlite3", "../other/.claude/worktrees/w/agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "sqlite3 '" + other[0] + "' .dump > out"}, nil, "agentmemory.sqlite-wal", nil},
		{"a variable names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, []string{"DB=" + other[0]}, "agentmemory.sqlite-wal", nil},
		{"an argument names the directory holding the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "--data-dir", "/home/testuser/code/other/.claude/worktrees/w"}, nil, "agentmemory.sqlite-wal", nil},
		{"a flag value names a directory above the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "--data-dir=../other"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script enters the directory holding the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"bash", "-c", "cd /home/testuser/code/other/.claude/worktrees/w && sqlite3 agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names the database by ~", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"bash", "-c", "app --db ~/code/other/.claude/worktrees/w/agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names its directory by $HOME", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"bash", "-c", `cd "$HOME"/code/other && ls`}, nil, "agentmemory.sqlite-wal", nil},
		{"a script enters the database's directory by a relative path", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "cd ../other/.claude/worktrees/w && sqlite3 agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names a directory above the database by $PWD", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", `cd "$PWD/../other" && make`}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names a directory above the database by ${PWD}", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "pushd ${PWD}/../other; make"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names a directory above the database by $(pwd)", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "cd $(pwd)/../other && make"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names a directory above the database by `pwd`", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "cd `pwd`/../other && make"}, nil, "agentmemory.sqlite-wal", nil},
		{"a Python script opens the database by a relative path", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"python3", "-c", "import sqlite3; sqlite3.connect('../other/.claude/worktrees/w/agentmemory.sqlite')"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names the database by a quoted \"$PWD\"", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", `sqlite3 "$PWD"/../other/.claude/worktrees/w/agentmemory.sqlite`}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names the database by a quoted \"$(pwd)\"", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", `sqlite3 "$(pwd)"/../other/.claude/worktrees/w/agentmemory.sqlite`}, nil, "agentmemory.sqlite-wal", nil},
		{"a script reaches the database by chained cds", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "cd /home/testuser/code/third/deep && cd ../../other/.claude/worktrees/w && sqlite3 agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"$PWDX is not $PWD", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "ls $PWDX/../other"}, nil, "", other},
		{"a relative script word that holds no database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sh", "-c", "cd ../third && make"}, nil, "", other},
		{"a variable naming a directory above the database is not a reference", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, []string{"HOME=/home/testuser", "DATA=/home/testuser/code/other"}, "", other},
		{"a bare / names no database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"cut", "-d/", "-f1", "/"}, nil, "", other},
		{"a sibling directory does not hold the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"ls", "/home/testuser/code/oth"}, nil, "", other},
		{"a SQLAlchemy URL names the database by a relative path", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "--url", "sqlite:///../other/.claude/worktrees/w/agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a driver's URL in a flag names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "--url=sqlite+aiosqlite:///../other/.claude/worktrees/w/agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"an absolute SQLAlchemy URL names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "sqlite:///" + other[0]}, nil, "agentmemory.sqlite-wal", nil},
		{"a variable's URL names the database by a relative path", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app"}, []string{"DATABASE_URL=sqlite:///../other/.claude/worktrees/w/agentmemory.sqlite"}, "agentmemory.sqlite-wal", nil},
		{"a file URI with a query names the database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"sqlite3", "file:../other/.claude/worktrees/w/agentmemory.sqlite?mode=rw"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script names the database from HOME after a bare cd", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"bash", "-c", "cd && sqlite3 code/other/.claude/worktrees/w/agentmemory.sqlite"}, nil, "agentmemory.sqlite-wal", nil},
		{"a script enters the database's directory from HOME after a bare cd", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"bash", "-c", "cd; cd code/other; make"}, nil, "agentmemory.sqlite-wal", nil},
		{"an https URL names no database", []string{other[1]}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"curl", "https://example.com/"}, nil, "", other},
		{"an LMDB environment in another repository is read-only", []string{"/home/testuser/code/other/env/lock.mdb", "/home/testuser/code/other/env/data.mdb"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "", []string{"/home/testuser/code/other/env/data.mdb", "/home/testuser/code/other/env/lock.mdb"}},
		{"an LMDB environment in the repository", []string{"/home/testuser/code/repo/db/data.mdb"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "/home/testuser/code/repo/db/data.mdb", nil},
		{"an argument names an LMDB environment's directory", []string{"/home/testuser/code/other/env/lock.mdb"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"app", "--db", "/home/testuser/code/other/env"}, nil, "lock.mdb", nil},
		{"the repository refusal wins over another read-only database", []string{other[1], "/home/testuser/code/repo/app.db-wal"}, "/home/testuser/code/repo", "/home/testuser/code/repo", []string{"make"}, nil, "/home/testuser/code/repo/app.db-wal", other},
	}
	for _, tt := range tests {
		var targets []openFile
		for _, p := range tt.targets {
			targets = append(targets, openFile{path: p})
		}
		refuse, ro := walPlan(targets, writable, tt.cwd, tt.repo, "/home/testuser", tt.argv, tt.env)
		if tt.refuse == "" && refuse != "" || tt.refuse != "" && !strings.Contains(refuse, tt.refuse) {
			t.Errorf("%s: refusal %q, want substring %q", tt.name, refuse, tt.refuse)
		}
		if tt.refuse == "" && strings.Join(ro, "|") != strings.Join(tt.readOnly, "|") {
			t.Errorf("%s: read-only %q, want %q", tt.name, ro, tt.readOnly)
		}
	}
}

func TestParseLocksKeepsHeldFlocks(t *testing.T) {
	got := parseLocks(`1: FLOCK  ADVISORY  WRITE 1203 103:02:5509 0 EOF
1: -> FLOCK  ADVISORY  WRITE 1300 103:02:5509 0 EOF
2: POSIX  ADVISORY  WRITE 99 00:1f:42 0 EOF
3: OFDLCK ADVISORY  READ  -1 00:1f:43 0 EOF
4: FLOCK  ADVISORY  READ  77 fd:01:12 0 EOF
`)
	want := map[fileID]bool{{0x103, 0x02, 5509}: true, {0xfd, 0x01, 12}: true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing %v in %v", id, got)
		}
	}
}

// A file a dev process holds a flock on keeps the job here when the job is likely to lock
// it, as a WAL database does, or when it has other names (hard links) that a read-only bind
// of this one leaves writable; any other under a writable path is read-only to the job.
func TestFlockPlan(t *testing.T) {
	writable := []string{"/home/testuser/code"}
	held := map[fileID]bool{{1, 2, 3}: true}
	lockAt := func(p string) openFile { return openFile{path: p, id: fileID{1, 2, 3}} }
	tests := []struct {
		name     string
		targets  []openFile
		cwd      string
		argv     []string
		env      []string
		refuse   string
		readOnly []string
	}{
		{"no lock", []openFile{{path: "/home/testuser/code/repo/f", id: fileID{9, 9, 9}}}, "/home/testuser/code/repo", []string{"make"}, nil, "", nil},
		{"a lock in the repository", []openFile{lockAt("/home/testuser/code/repo/target/.cargo-lock")}, "/home/testuser/code/repo", []string{"cargo", "test"}, nil, "/home/testuser/code/repo/target/.cargo-lock", nil},
		{"a lock in another repository is read-only", []openFile{lockAt("/home/testuser/code/other/l"), lockAt("/home/testuser/code/other/l")}, "/home/testuser/code/repo", []string{"make"}, nil, "", []string{"/home/testuser/code/other/l"}},
		{"an argument names the lock", []openFile{lockAt("/home/testuser/code/other/l")}, "/home/testuser/code/repo", []string{"flock", "../other/l", "true"}, nil, "/home/testuser/code/other/l", nil},
		{"a variable names the lock", []openFile{lockAt("/home/testuser/code/other/l")}, "/home/testuser/code/repo", []string{"make"}, []string{"LOCK=/home/testuser/code/other/l"}, "/home/testuser/code/other/l", nil},
		{"an argument names the directory holding the lock", []openFile{lockAt("/home/testuser/code/other/l")}, "/home/testuser/code/repo", []string{"tool", "--state", "/home/testuser/code/other"}, nil, "/home/testuser/code/other/l", nil},
		{"a script names the lock by ~", []openFile{lockAt("/home/testuser/code/other/l")}, "/home/testuser/code/repo", []string{"sh", "-c", "flock ~/code/other/l true"}, nil, "/home/testuser/code/other/l", nil},
		{"a lock on a directory above the repository", []openFile{lockAt("/home/testuser/code")}, "/home/testuser/code/repo/sub", []string{"make"}, nil, "/home/testuser/code", nil},
		{"a lock outside the writable paths", []openFile{lockAt("/home/testuser/.cache/uv/.lock")}, "/home/testuser/code/repo", []string{"make"}, nil, "", nil},
		{"an unreadable identity is no lock", []openFile{{path: "/home/testuser/code/repo/l"}}, "/home/testuser/code/repo", []string{"make"}, nil, "", nil},
		{"a lock with another name", []openFile{{path: "/home/testuser/code/other/l", id: fileID{1, 2, 3}, nlink: 2}}, "/home/testuser/code/repo", []string{"flock", "B", "true"}, nil, "/home/testuser/code/other/l (with 1 other names, hard links offload cannot find to make read-only)", nil},
	}
	for _, tt := range tests {
		refuse, ro := flockPlan(tt.targets, held, writable, tt.cwd, "/home/testuser/code/repo", "/home/testuser", tt.argv, tt.env)
		if refuse != tt.refuse || strings.Join(ro, "|") != strings.Join(tt.readOnly, "|") {
			t.Errorf("%s: refusal %q, read-only %q; want %q, %q", tt.name, refuse, ro, tt.refuse, tt.readOnly)
		}
	}
}

// A -wal, -shm or flocked file held from another mount namespace keeps the job here when it
// is under a writable path as dev names it, or offload cannot find it on dev, since its
// path is the holder's; walPlan and flockPlan leave it alone. One outside every writable
// path is read-only to the job already.
func TestForeignPlan(t *testing.T) {
	devs := map[fileID]bool{{major: 0, minor: 33}: true}
	at := func(p string, ino uint64, foreign bool) openFile {
		return openFile{path: p, id: fileID{0, 33, ino}, foreign: foreign}
	}
	held := map[fileID]bool{{0, 33, 7}: true}
	writable := []string{"/home/testuser/code"}
	// The container binds dev's /home/testuser/code/r onto /data, and /var/lib/c onto /cache.
	hosts := func(t openFile) []string {
		if rest, ok := strings.CutPrefix(t.path, "/data/"); ok {
			return []string{"/home/testuser/code/r/" + rest}
		}
		if rest, ok := strings.CutPrefix(t.path, "/cache/"); ok {
			return []string{"/var/lib/c/" + rest}
		}
		return nil
	}
	for _, tt := range []struct {
		name    string
		targets []openFile
		want    string
	}{
		{"a container's WAL under a writable path", []openFile{at("/data/app.db-wal", 1, true)}, "/data/app.db-wal, /home/testuser/code/r/app.db-wal on dev"},
		{"a container's flock under a writable path", []openFile{at("/data/lock", 7, true)}, "/data/lock, /home/testuser/code/r/lock on dev"},
		{"a container's WAL outside every writable path", []openFile{at("/cache/app.db-wal", 1, true)}, ""},
		{"a container's WAL offload cannot find on dev", []openFile{at("/srv/app.db-wal", 1, true)}, "/srv/app.db-wal, which offload cannot find on dev"},
		{"a container's plain file", []openFile{at("/data/app.db", 1, true)}, ""},
		{"a container's WAL on another device", []openFile{{path: "/data/app.db-wal", id: fileID{0, 99, 1}, foreign: true}}, ""},
		{"dev's own WAL", []openFile{at("/home/testuser/code/r/app.db-wal", 1, false)}, ""},
		{"an unreadable identity", []openFile{{path: "/data/app.db-wal", foreign: true}}, ""},
		{"a container's deleted WAL", []openFile{at("/data/app.db-wal (deleted)", 1, true)}, "/data/app.db-wal, /home/testuser/code/r/app.db-wal (deleted) on dev"},
	} {
		if got := foreignPlan(tt.targets, held, devs, writable, hosts); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
	foreign := []openFile{{path: "/home/testuser/code/repo/app.db-wal", id: fileID{0, 33, 7}, foreign: true}}
	if refuse, ro := walPlan(foreign, writable, "/home/testuser/code/repo", "", "/home/testuser", nil, nil); refuse != "" || ro != nil {
		t.Errorf("walPlan judged a foreign holder: %q %v", refuse, ro)
	}
	if refuse, ro := flockPlan(foreign, held, writable, "/home/testuser/code/repo", "", "/home/testuser", nil, nil); refuse != "" || ro != nil {
		t.Errorf("flockPlan judged a foreign holder: %q %v", refuse, ro)
	}
}

// A symlink an argument names, to a database a dev process holds, keeps the job here: the
// remote job would open the database through it.
func TestWalPlanRefusesADatabaseAnArgumentNamesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "data/app.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "elsewhere/current.db")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	targets := []openFile{{path: db + "-wal", id: fileID{1, 2, 3}}}
	refuse, _ := walPlan(targets, []string{dir}, filepath.Join(dir, "repo"), "", "/home/testuser", []string{"sqlite3", link}, nil)
	if refuse != db+"-wal" {
		t.Errorf("refusal %q, want %s", refuse, db+"-wal")
	}
}

// hostPaths reads a holder's path through its mount table and dev's: a bind of a folder of
// dev's root filesystem, a subvolume mounted twice, and a mount dev does not have.
func TestHostPathsFindsAContainersFileWhereDevMountsIt(t *testing.T) {
	dev := `22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw
23 22 259:2 /home/testuser/code/r /srv/r rw - ext4 /dev/nvme0n1p2 rw
24 22 0:44 /sub /mnt/a rw - btrfs /dev/sdb rw
25 22 0:44 / /mnt/b rw - btrfs /dev/sdb rw
26 22 259:2 /etc /srv/etc rw - ext4 /dev/nvme0n1p2 rw
`
	holder := `90 80 0:60 / / rw - overlay overlay rw
91 90 259:2 /home/testuser/code/r /data rw - ext4 /dev/nvme0n1p2 rw
92 90 0:44 /sub/x /x rw - btrfs /dev/sdb rw
93 90 0:61 / /scratch rw - tmpfs tmpfs rw
94 90 0:61 / /stack rw - tmpfs tmpfs rw
95 94 259:2 /home/testuser/code/r /stack rw - ext4 /dev/nvme0n1p2 rw
`
	for _, c := range []struct {
		p    string
		want []string
	}{
		{"/data/app.db-wal", []string{"/home/testuser/code/r/app.db-wal", "/srv/r/app.db-wal"}},
		{"/x/y.db-wal", []string{"/mnt/a/x/y.db-wal", "/mnt/b/sub/x/y.db-wal"}},
		{"/scratch/app.db-wal", nil},
		// The mount stacked last on a point is the one the holder sees.
		{"/stack/app.db-wal", []string{"/home/testuser/code/r/app.db-wal", "/srv/r/app.db-wal"}},
	} {
		if got := hostPaths(strings.NewReader(holder), strings.NewReader(dev), c.p); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.p, got, c.want)
		}
	}
}

// A container holding an LMDB environment, or a WAL database by its main file only, under a
// writable path keeps the job here, as a dev holder's would.
func TestForeignPlanProtectsMappedDatabases(t *testing.T) {
	dir := t.TempDir()
	sqliteDB(t, filepath.Join(dir, "app.db"), true)
	sqliteDB(t, filepath.Join(dir, "plain.db"), false)
	// A NOSUBDIR LMDB environment, whose files exist only at dev's path.
	for _, f := range []string{"store", "store-lock"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	devs := map[fileID]bool{{major: 0, minor: 33}: true}
	hosts := func(t openFile) []string {
		if rest, ok := strings.CutPrefix(t.path, "/data/"); ok {
			return []string{filepath.Join(dir, rest)}
		}
		return nil
	}
	for _, tt := range []struct {
		path   string
		refuse bool
	}{
		{"/data/env/data.mdb", true},
		{"/data/env/lock.mdb", true},
		{"/data/app.db", true},
		{"/data/plain.db", false},
		{"/data/store-lock", true},
	} {
		got := foreignPlan([]openFile{{path: tt.path, id: fileID{0, 33, 5}, foreign: true}}, nil, devs, []string{dir}, hosts)
		if (got != "") != tt.refuse {
			t.Errorf("%s: got %q, want refused %v", tt.path, got, tt.refuse)
		}
	}
}

// A WAL database held by its main file only, outside the job's own paths, keeps the job
// here: the read-only bind skips its missing -shm, which a remote reader would create.
func TestBareWALNamesADatabaseWhoseShmIsMissing(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	sqliteDB(t, db, true)
	if got := bareWAL([]string{db, db + "-wal", db + "-shm"}, nil); got != db {
		t.Errorf("got %q, want %s", got, db)
	}
	if err := os.WriteFile(db+"-shm", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bareWAL([]string{db, db + "-wal", db + "-shm"}, nil); got != "" {
		t.Errorf("with its -shm: %q", got)
	}
}

// holdOpen starts the holder argv, which prints "up" once it holds what it opens, and
// narrows the scan to it.
func holdOpen(t *testing.T, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if _, err := io.ReadFull(out, make([]byte, 2)); err != nil {
		t.Fatalf("holder %q: %v", argv, err)
	}
	g := fdGlob
	t.Cleanup(func() { fdGlob = g })
	fdGlob = "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd/*"
}

// A container's data directory guarded by a pid file (PostgreSQL's postmaster.pid),
// bind-mounted under a writable path, keeps the job here, as a dev holder's does: a remote
// server would take the data over beside the container's. The pid is the container's, which
// dev's kill cannot judge, so any pid counts. A plain file elsewhere does not.
func TestForeignPlanProtectsADataDirectoryGuardedByAPidFile(t *testing.T) {
	dir := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(dir, "pg/data")
	write(filepath.Join(data, "postmaster.pid"), "2147483646\n/var/lib/postgresql/data\n")
	write(filepath.Join(data, "base/1/1259"), "")
	write(filepath.Join(dir, "srv/base/1/1259"), "")
	devs := map[fileID]bool{{major: 0, minor: 33}: true}
	hosts := func(t openFile) []string {
		if rest, ok := strings.CutPrefix(t.path, "/var/lib/postgresql/data/"); ok {
			return []string{filepath.Join(data, rest)}
		}
		if rest, ok := strings.CutPrefix(t.path, "/srv/"); ok {
			return []string{filepath.Join(dir, "srv", rest)}
		}
		return nil
	}
	held := func(p string) []openFile { return []openFile{{path: p, id: fileID{0, 33, 5}, foreign: true}} }
	if got := foreignPlan(held("/var/lib/postgresql/data/base/1/1259"), nil, devs, []string{dir}, hosts); !strings.Contains(got, filepath.Join(data, "base/1/1259")) {
		t.Errorf("a container's guarded data directory: %q, want it refused", got)
	}
	for _, p := range []string{"/srv/base/1/1259", "/elsewhere/f"} {
		if got := foreignPlan(held(p), nil, devs, []string{dir}, hosts); got != "" {
			t.Errorf("%s: %q, want no refusal", p, got)
		}
	}
}
