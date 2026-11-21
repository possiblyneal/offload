package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// foreignPlan names a -wal, -shm or flocked file that a process in another mount namespace
// (a container with a bind mount) holds on the device of a writable path, and that is under
// a writable path as dev names it (hostPathsOf), or "". walPlan and flockPlan cannot judge
// it by the holder's path, so it keeps the job here; so does one offload cannot place on
// dev. One outside every writable path is read-only to the job already. So does any file
// it holds in a data directory a pid file guards (pidDataDir) under a writable path: a
// container's PostgreSQL data, bind-mounted from dev. The pid is the container's, which
// dev's kill cannot judge, so any pid counts.
func foreignPlan(targets []openFile, held, devs map[fileID]bool, writable []string, hostPathsOf func(openFile) []string) string {
	pidDirs := map[string]bool{}
	anyPid := func(int) bool { return true }
	for _, t := range targets {
		dev := t.id
		dev.ino = 0
		if !t.foreign || t.id == (fileID{}) || !devs[dev] {
			continue
		}
		p := strings.TrimSuffix(t.path, " (deleted)")
		// Its name gives away a mapped database's side file; a WAL database held by its
		// main file only shows in that file's header, and a data directory in its pid file,
		// each read through dev's path to it.
		hosts := hostPathsOf(t)
		guarded := slices.ContainsFunc(hosts, func(h string) bool {
			root := within(writable, h)
			return root != "" && pidDataDir(h, root, pidDirs, anyPid) != nil
		})
		// An LMDB file's sibling (`store` beside `store-lock`) exists only at dev's path.
		mapped := mappedDatabase(p) != nil || slices.ContainsFunc(hosts, func(h string) bool { return mappedDatabase(h) != nil })
		if !held[t.id] && !mapped && !slices.ContainsFunc(hosts, sqliteWAL) && !guarded {
			continue
		}
		if len(hosts) == 0 {
			return p + ", which offload cannot find on dev"
		}
		for _, h := range hosts {
			if within(writable, h) != "" {
				return p + ", " + h + " on dev"
			}
		}
	}
	return ""
}

// hostPathsOf is hostPaths of the holder's mount table and dev's.
func hostPathsOf(t openFile) []string {
	holder, err := os.Open("/proc/" + t.pid + "/mountinfo")
	if err != nil {
		return nil
	}
	defer holder.Close()
	own, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer own.Close()
	return hostPaths(holder, own, t.path)
}

// hostPaths are the paths at which dev's mount table shows the file a holder names p
// through its own: p's mount there gives the device and the path within its filesystem,
// and every mount of that device on dev that shows that path names it.
func hostPaths(holderInfo, devInfo io.Reader, p string) []string {
	holder, _ := readMountinfo(holderInfo)
	var in *mount
	for i := range holder {
		if within([]string{holder[i].point}, p) != "" && (in == nil || len(holder[i].point) >= len(in.point)) {
			in = &holder[i]
		}
	}
	if in == nil {
		return nil
	}
	fsPath := filepath.Join(in.root, strings.TrimPrefix(p, in.point))
	own, _ := readMountinfo(devInfo)
	var out []string
	for _, m := range own {
		if m.device == in.device && within([]string{m.root}, fsPath) != "" {
			out = append(out, filepath.Join(m.point, strings.TrimPrefix(fsPath, m.root)))
		}
	}
	return out
}

// parseLocks keeps the files of the held FLOCK entries of /proc/locks (parseLockLines).
func parseLocks(s string) map[fileID]bool {
	held := map[fileID]bool{}
	for _, l := range parseLockLines(s) {
		if l.kind == "FLOCK" {
			held[l.id] = true
		}
	}
	return held
}

// flockPlan judges the files under a writable path that a dev process holds a flock on,
// as walPlan judges databases: one the job is likely to use (under the cwd or the
// repository, or named by an argument or a variable) keeps it here, and the refusal names
// it; every other one the job sees read-only, so taking the lock fails instead of
// succeeding beside dev's holder. NFS turns a remote job's flock into a POSIX lock that
// never conflicts with dev's flock, so a dev process that takes the flock after the job
// started is not excluded either. A file with other names (hard links) keeps the job here,
// since a read-only bind covers one name only.
func flockPlan(targets []openFile, held map[fileID]bool, writable []string, cwd, repo, home string, argv, env []string) (refuse string, readOnly []string) {
	mine := ownDirs(cwd, repo)
	seen := map[string]bool{}
	for _, t := range targets {
		p := strings.TrimSuffix(t.path, " (deleted)")
		if t.foreign || t.id == (fileID{}) || !held[t.id] || within(writable, p) == "" || seen[p] {
			continue
		}
		seen[p] = true
		// A flocked directory above the job's own (`flock /a` over cwd /a/b) would make them read-only too.
		holdsMine := slices.ContainsFunc(mine, func(m string) bool { return within([]string{p}, m) != "" })
		if holdsMine || within(mine, p) != "" || namesFile([]string{p}, cwd, home, argv, env) {
			return p, nil
		}
		// The read-only bind covers this name only; another would stay writable.
		if t.nlink > 1 {
			return fmt.Sprintf("%s (with %d other names, hard links offload cannot find to make read-only)", p, t.nlink-1), nil
		}
		readOnly = append(readOnly, p)
	}
	return "", readOnly
}

// ownDirs are where the job's own files are: the cwd, its real path, and the repository.
func ownDirs(cwd, repo string) []string {
	var mine []string
	for _, d := range []string{cwd, realPath(cwd), repo} {
		if d != "" {
			mine = append(mine, d)
		}
	}
	return mine
}

// walPlan judges the databases dev's processes hold open under a writable path whose
// writers share a memory map (mappedDatabase): over NFS the map is not shared, so a remote
// writer and dev's would both write at once. A database the job is likely to use keeps it
// here, and the refusal names the open file: one under the cwd or the repository, or one
// an argument or a variable names. Every other such database is returned with its files
// for the job to see read-only, so a write to it fails instead of corrupting it.
func walPlan(targets []openFile, writable []string, cwd, repo, home string, argv, env []string) (refuse string, readOnly []string) {
	mine := ownDirs(cwd, repo)
	seen, pidDirs := map[string]bool{}, map[string]bool{}
	for _, t := range targets {
		p := strings.TrimSuffix(t.path, " (deleted)")
		root := within(writable, p)
		if t.foreign || root == "" {
			continue
		}
		files := mappedDatabase(p)
		if files == nil {
			files = pidDataDir(p, root, pidDirs, devAlive)
		}
		if files == nil {
			continue
		}
		// A data directory that holds the cwd or repository could not be made read-only
		// without the job's own files.
		if seen[files[0]] {
			continue
		}
		seen[files[0]] = true
		holdsMine := slices.ContainsFunc(mine, func(m string) bool { return within(files[:1], m) != "" })
		if holdsMine || within(mine, files[0]) != "" || namesFile(files, cwd, home, argv, env) {
			return p, nil
		}
		// The read-only bind covers each name only; another would stay writable.
		if extra := otherNames(files); extra != "" {
			return extra, nil
		}
		readOnly = append(readOnly, files...)
	}
	return "", readOnly
}

// otherNames names the first of files, a database's, that has hard links, as flockPlan
// words it, or "". A directory (pidDataDir's) has one name; a missing file none.
func otherNames(files []string) string {
	for _, f := range files {
		var st syscall.Stat_t
		if syscall.Lstat(f, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFREG && st.Nlink > 1 {
			return fmt.Sprintf("%s (with %d other names, hard links offload cannot find to make read-only)", f, st.Nlink-1)
		}
	}
	return ""
}

// mappedDatabase returns the files of the database that p, a file a process holds open, is
// part of, the database's main file first, when its writers coordinate through a shared
// memory map: SQLite in WAL mode (db, -wal, -shm) and LMDB (data.mdb and lock.mdb in its
// directory, or, opened with MDB_NOSUBDIR, a file and its -lock); nil for any other file.
func mappedDatabase(p string) []string {
	if db, ok := strings.CutSuffix(p, "-wal"); ok {
		return []string{db, db + "-wal", db + "-shm"}
	}
	if db, ok := strings.CutSuffix(p, "-shm"); ok {
		return []string{db, db + "-wal", db + "-shm"}
	}
	if b := filepath.Base(p); b == "lock.mdb" || b == "data.mdb" {
		dir := filepath.Dir(p)
		return []string{dir + "/data.mdb", dir + "/lock.mdb"}
	}
	if db, ok := strings.CutSuffix(p, "-lock"); ok {
		if st, err := os.Stat(db); err == nil && st.Mode().IsRegular() {
			return []string{db, p}
		}
	}
	// A connection to a WAL database holds only the main file until its first read.
	if sqliteWAL(p) {
		return []string{p, p + "-wal", p + "-shm"}
	}
	return nil
}

// bareWAL names the first WAL database among walPlan's read-only files whose -shm is
// missing, or "": its holder has not read it yet, so the read-only bind (which skips a
// missing file) would leave a remote reader free to create the -shm dev's process maps.
// One whose main file readerLocked holds (a connection in exclusive locking mode) is not:
// it never has a -shm, and a remote reader, whose lock NFS carries to dev, fails to take
// the lock it needs before it would create one.
func bareWAL(readOnly []string, readerLocked map[fileID]bool) string {
	for _, p := range readOnly {
		if db, ok := strings.CutSuffix(p, "-shm"); ok {
			if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
				var st syscall.Stat_t
				if syscall.Stat(db, &st) == nil && readerLocked[idOf(st)] {
					continue
				}
				return db
			}
		}
	}
	return ""
}

// sqliteWAL reports whether p is a regular file holding a SQLite database in WAL mode: its
// header's magic string, and 2 in its write-version byte.
func sqliteWAL(p string) bool {
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 20)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head[:16]) == "SQLite format 3\x00" && head[18] == 2
}

// pidDataDir returns the directory holding p, below the writable root it is under, that
// holds a pid file naming a live dev process (postmaster.pid, or any other *.pid), or nil.
// A program that guards its data with a pid file, as PostgreSQL does, checks it with
// kill(pid, 0), which finds nothing in the job's pid namespace: a remote one would take
// the data over beside dev's. cache keeps each directory's answer. A shared temporary
// directory (sharedTmp) is never one: a stray pid file in /tmp must not make all of /tmp
// read-only.
func pidDataDir(p, root string, cache map[string]bool, alive func(pid int) bool) []string {
	for d := filepath.Dir(p); within([]string{root}, d) != ""; d = filepath.Dir(d) {
		if d == root && slices.Contains(sharedTmp, root) {
			break
		}
		live, ok := cache[d]
		if !ok {
			live = holdsLivePidFile(d, alive)
			cache[d] = live
		}
		if live {
			return []string{d}
		}
	}
	return nil
}

// holdsLivePidFile reports whether dir holds a regular *.pid file whose first line is the
// pid of a process alive on dev.
func holdsLivePidFile(dir string, alive func(pid int) bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		head := make([]byte, 32)
		n, _ := io.ReadFull(f, head)
		f.Close()
		line, _, _ := strings.Cut(string(head[:n]), "\n")
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			continue
		}
		if alive(pid) {
			return true
		}
	}
	return false
}

// devAlive reports whether pid is a live dev process, another account's included (kill
// gives EPERM).
func devAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
