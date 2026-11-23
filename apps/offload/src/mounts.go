package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// unexportedMounts reads /proc/self/mountinfo and returns the mount points of dev whose
// contents a remote does not see. Dev exports `/` without crossmnt, and its /tmp beside it
// (install.sh), so the export carries those two trees only: at the mount point of any
// other filesystem (a tmpfs, a FUSE mount, another disk) a remote sees the directory
// underneath, and at a bind of another part of the root filesystem it sees what the bind
// covers. A job that works
// under one cannot run on a remote. /proc, /sys and /dev are the remote's own in the job,
// so the filesystems under them count only when they hold files (tmpfs: /dev/shm).
// exportedTmp is the device of dev's own /tmp (exportedTmpDevice): a /tmp of another device,
// a private tmpfs of the caller's mount namespace, is not what the export carries.
func unexportedMounts(mountinfo io.Reader, exportedTmp string) []string {
	all, rootDevice := readMountinfo(mountinfo)
	var mounts []mount
	for _, m := range all {
		if m.point == "/" || m.point == "/tmp" && m.root == "/" && m.device == exportedTmp {
			continue // exported: / and, beside it, /tmp (install.sh); a PrivateTmp bind of a subdirectory is not
		}
		mounts = append(mounts, m)
	}
	var out []string
	for _, m := range mounts {
		switch {
		case m.fstype == "tmpfs" || strings.HasPrefix(m.fstype, "fuse"):
			out = append(out, m.point)
		case within([]string{"/proc", "/sys", "/dev"}, m.point) != "":
		case m.device != rootDevice || m.root != m.point:
			out = append(out, m.point)
		}
	}
	return out
}

// readOnlyMounts returns the paths the job sees read-only because the export does not
// carry them: each unexported mount inside a writable path, and each writable path that
// lies on one. Written through, they would land in the directory the mount hides on dev's
// root filesystem; read-only, a write fails naming the path. A job whose cwd or argument
// is on one stays here (tmpRefusal). A writable path is judged both as written and with
// its symlinks followed, as the job's bind follows them.
func readOnlyMounts(mounts, writable []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, w := range writable {
		both := []string{w, realPath(w)}
		if within(mounts, both[0]) != "" || within(mounts, both[1]) != "" {
			add(w)
			continue
		}
		for _, m := range mounts {
			if within(both, m) != "" {
				add(m)
			}
		}
	}
	return out
}

// mount is one line of /proc/self/mountinfo: the device, the path within its filesystem
// that is mounted, where, and the filesystem type.
type mount struct {
	device, root, point, fstype string
	readOnly                    bool // mounted ro: dev's rw export of / would carry writes past it
}

// readMountinfo parses mountinfo, returning its mounts and the device of the mount in
// effect on /.
func readMountinfo(mountinfo io.Reader) (mounts []mount, rootDevice string) {
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		// <id> <parent> <maj:min> <root> <mount point> <options> [optional fields] - <fstype> <source> <options>
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		f := strings.Fields(left)
		fstype := strings.Fields(right)
		if len(f) < 6 || len(fstype) == 0 {
			continue
		}
		m := mount{f[2], unescapeMountPath(f[3]), unescapeMountPath(f[4]), fstype[0], slices.Contains(strings.Split(f[5], ","), "ro")}
		if m.point == "/" {
			rootDevice = m.device // the last mount on / is the one in effect
		}
		mounts = append(mounts, m)
	}
	return mounts, rootDevice
}

// rootBinds are the binds of one part of dev's root filesystem onto another, as mount
// point and the root filesystem path it shows, deepest mount point first.
func rootBinds(mountinfo io.Reader) [][2]string {
	all, rootDevice := readMountinfo(mountinfo)
	var out [][2]string
	for _, m := range all {
		if m.device == rootDevice && m.point != "/" && m.root != m.point {
			out = append(out, [2]string{m.point, m.root})
		}
	}
	slices.SortStableFunc(out, func(a, b [2]string) int { return len(b[0]) - len(a[0]) })
	return out
}

// onRootFS is p as dev's root filesystem names it: a path under a root bind's mount point
// becomes the path the bind shows, so a holder that reached a file through a bind is
// matched against the writable paths the job reaches it by.
func onRootFS(binds [][2]string, p string) string {
	for _, b := range binds {
		if rest, ok := strings.CutPrefix(p, b[0]); ok && (rest == "" || rest[0] == '/') {
			return filepath.Clean(b[1] + rest)
		}
	}
	return p
}

// devMounts is unexportedMounts of this process's mount table, or none when it cannot be
// read.
var devMounts = func() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return unexportedMounts(f, exportedTmpDevice())
}

// exportedTmpDevice is the device of /tmp in init's mount namespace, where nfsd exports it,
// or "" when that cannot be read, which leaves every /tmp mount unexported.
func exportedTmpDevice() string {
	f, err := os.Open("/proc/1/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	mounts, _ := readMountinfo(f)
	device := ""
	for _, m := range mounts {
		if m.point == "/tmp" && m.root == "/" {
			device = m.device // the last mount on /tmp is the one in effect
		}
	}
	return device
}

// readOnlySelfBinds are the directories of dev's root filesystem bound read-only onto
// themselves. A remote sees the same files there, so they keep no job here, but the export
// carries them read-write: the job sees them read-only, as a local run does.
func readOnlySelfBinds(mountinfo io.Reader) []string {
	all, rootDevice := readMountinfo(mountinfo)
	var out []string
	for _, m := range all {
		if m.device == rootDevice && m.point != "/" && m.root == m.point && m.readOnly {
			out = append(out, m.point)
		}
	}
	return out
}

// devReadOnlySelfBinds is readOnlySelfBinds of this process's mount table, or none when it
// cannot be read.
func devReadOnlySelfBinds() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return readOnlySelfBinds(f)
}

// devRootBinds is rootBinds of this process's mount table, or none when it cannot be read.
// A var so a test can stand in a bind without mounting one.
var devRootBinds = func() [][2]string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return rootBinds(f)
}

// unescapeMountPath undoes the kernel's octal escapes for space, tab, newline and
// backslash in mountinfo paths.
func unescapeMountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// within returns the first of dirs that is the absolute path p or holds it, or "". A path
// that is not absolute is not judged.
func within(dirs []string, p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return d
		}
	}
	return ""
}

// withRealPaths is paths, each followed by its real path where symlinks make that differ:
// a path the kernel reports (an open file, the cwd) is real, and a root spelled through a
// symlink does not prefix it.
func withRealPaths(paths []string) []string {
	out := slices.Clone(paths)
	for _, p := range paths {
		if r := realPath(p); r != p {
			out = append(out, r)
		}
	}
	return out
}
