package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const mountinfo = `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
23 22 0:21 / /tmp rw,nosuid,nodev shared:2 - tmpfs tmpfs rw,size=8051272k
24 22 0:22 / /run rw,nosuid,nodev shared:3 - tmpfs tmpfs rw,mode=755
25 24 0:50 / /run/user/1000 rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
26 25 0:51 / /run/user/1000/doc rw,nosuid,nodev shared:5 - fuse.portal portal rw,user_id=1000
27 22 0:6 / /dev rw,nosuid shared:6 - devtmpfs devtmpfs rw
28 27 0:23 / /dev/shm rw,nosuid,nodev shared:7 - tmpfs tmpfs rw
29 22 0:60 / /mnt/with\040space rw shared:8 - fuse.sshfs host:/x rw
`

// Dev's /tmp is exported beside /, so it is not one of them, though a mount inside it is.
func TestUnexportedMountsAreTmpfsAndFuse(t *testing.T) {
	got := unexportedMounts(strings.NewReader(mountinfo+"30 23 0:61 / /tmp/.mount_app rw shared:9 - fuse.app app rw\n"), "0:21")
	want := []string{"/run", "/run/user/1000", "/run/user/1000/doc", "/dev/shm", "/mnt/with space", "/tmp/.mount_app"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The export is `/` without crossmnt, so the client sees, at any other filesystem's mount
// point, the directory underneath it on the root filesystem; at a bind of another part of
// the root filesystem it sees what the bind hides. Only the root filesystem's own tree is
// exported faithfully. /proc, /sys and /dev are the remote's own in the job.
const mixedMountinfo = `29 1 259:2 / / rw,noatime shared:1 - ext4 /dev/nvme0n1p2 rw
24 29 0:22 / /sys rw shared:6 - sysfs sysfs rw
25 29 0:23 / /proc rw shared:12 - proc proc rw
26 29 0:6 / /dev rw shared:2 - devtmpfs udev rw
27 26 0:24 / /dev/pts rw shared:3 - devpts devpts rw
38 26 0:21 / /dev/mqueue rw shared:14 - mqueue mqueue rw
31 26 0:26 / /dev/shm rw shared:4 - tmpfs tmpfs rw
51 29 259:1 / /boot/efi rw shared:90 - vfat /dev/nvme0n1p1 rw
60 29 8:17 / /srv/data rw shared:91 - ext4 /dev/sdb1 rw
61 29 0:70 / /home/testuser/sub rw shared:92 - btrfs /dev/sdc1 rw
62 29 259:2 /home/testuser/elsewhere /home/testuser/code/linked rw shared:1 - ext4 /dev/nvme0n1p2 rw
63 29 259:2 /etc/ro /etc/ro ro shared:1 - ext4 /dev/nvme0n1p2 rw
64 29 0:90 / /nfs/x rw shared:93 - nfs4 host:/ rw
`

func TestUnexportedMountsAreEveryOtherFilesystemAndForeignBinds(t *testing.T) {
	got := unexportedMounts(strings.NewReader(mixedMountinfo), "")
	want := []string{"/dev/shm", "/boot/efi", "/srv/data", "/home/testuser/sub", "/home/testuser/code/linked", "/nfs/x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOnUnexportedMountFindsAJobThatCannotRunRemotely(t *testing.T) {
	mounts := []string{"/tmp", "/run/user/1000"}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"the mount itself", "/tmp", true},
		{"inside it", "/tmp/build/x", true},
		{"a sibling with the same prefix", "/tmpfiles/x", false},
		{"on the exported disk", "/home/testuser/code", false},
		{"a relative path is not judged", "tmp/x", false},
	}
	for _, c := range cases {
		if got := within(mounts, c.path) != ""; got != c.want {
			t.Errorf("%s: %q -> %v, want %v", c.name, c.path, got, c.want)
		}
	}
}

// The export does not carry a mount inside a writable path: there the job would write
// into the directory the mount hides on dev's root filesystem. So each such mount, and
// each writable path that lies on one, is bound read-only.
func TestUnexportedMountsInWritablePathsAreReadOnly(t *testing.T) {
	const info = `29 1 259:2 / / rw shared:1 - ext4 /dev/nvme0n1p2 rw
30 29 0:40 / /home/testuser/code/repo/cache rw shared:2 - tmpfs tmpfs rw
31 29 8:17 / /home/testuser/code/disk rw shared:3 - ext4 /dev/sdb1 rw
32 29 259:2 /srv/x /home/testuser/code/linked rw shared:1 - ext4 /dev/nvme0n1p2 rw
33 29 0:41 / /home/testuser/.cache rw shared:4 - tmpfs tmpfs rw
34 29 0:42 / /home/testuser/notes rw shared:5 - tmpfs tmpfs rw
35 29 0:43 / /home/testuser/codex rw shared:6 - tmpfs tmpfs rw
36 29 0:6 / /dev rw shared:7 - devtmpfs udev rw
`
	mounts := unexportedMounts(strings.NewReader(info), "")
	writable := []string{"/home/testuser/code", "/home/testuser/.cache/go-build", "/home/testuser/elsewhere"}
	got := readOnlyMounts(mounts, writable)
	want := []string{"/home/testuser/code/repo/cache", "/home/testuser/code/disk", "/home/testuser/code/linked", "/home/testuser/.cache/go-build"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := readOnlyMounts(mounts, []string{"/home/testuser/elsewhere"}); len(got) != 0 {
		t.Errorf("a writable path with no mount in or above it: got %q", got)
	}
}

// A writable path that is a symlink onto an unexported mount, or onto a directory holding
// one, is judged where the link leads: the job's bind follows it there.
func TestReadOnlyMountsFollowsAWritablePathsSymlinks(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"data/code", "parent/sub"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	onMount, aboveMount := filepath.Join(dir, "code"), filepath.Join(dir, "work")
	for link, target := range map[string]string{onMount: "data/code", aboveMount: "parent"} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []string{filepath.Join(dir, "data"), filepath.Join(dir, "parent/sub")}
	got := readOnlyMounts(mounts, []string{onMount, aboveMount})
	want := []string{onMount, filepath.Join(dir, "parent/sub")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A writable / holds every mount, so each one stays read-only to the job.
func TestReadOnlyMountsUnderAWritableRoot(t *testing.T) {
	if got := within([]string{"/"}, "/home/x"); got != "/" {
		t.Errorf(`within(["/"], "/home/x") = %q`, got)
	}
	if got := readOnlyMounts([]string{"/mnt/ram"}, []string{"/"}); !reflect.DeepEqual(got, []string{"/mnt/ram"}) {
		t.Errorf("readOnlyMounts = %v, want [/mnt/ram]", got)
	}
}

// A PrivateTmp service's /tmp is a bind of a subdirectory of dev's /tmp, which a remote
// job does not see as /tmp; only /tmp's own root is exported.
func TestUnexportedMountsKeepsAPrivateTmpBind(t *testing.T) {
	plain := "29 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n48 29 0:42 / /tmp rw - tmpfs tmpfs rw\n"
	if got := unexportedMounts(strings.NewReader(plain), "0:42"); len(got) != 0 {
		t.Errorf("dev's own /tmp: got %q, want none", got)
	}
	private := plain + "90 48 0:42 /systemd-private-abc-x.service-def/tmp /tmp rw - tmpfs tmpfs rw\n"
	if got := unexportedMounts(strings.NewReader(private), "0:42"); !slices.Equal(got, []string{"/tmp"}) {
		t.Errorf("PrivateTmp /tmp: got %q, want [/tmp]", got)
	}
}

// A tmpfs of the caller's own mount namespace on /tmp (TemporaryFileSystem=, bwrap --tmpfs)
// is not dev's /tmp: a remote job would see dev's.
func TestUnexportedMountsKeepsAPrivateTmpfsOnTmp(t *testing.T) {
	private := "29 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n90 29 0:99 / /tmp rw - tmpfs tmpfs rw\n"
	if got := unexportedMounts(strings.NewReader(private), "0:42"); !slices.Equal(got, []string{"/tmp"}) {
		t.Errorf("private tmpfs on /tmp: got %q, want [/tmp]", got)
	}
	if got := unexportedMounts(strings.NewReader(private), ""); !slices.Equal(got, []string{"/tmp"}) {
		t.Errorf("dev's /tmp unknown: got %q, want [/tmp]", got)
	}
}

// Dev's /tmp is read from init's mount namespace, where nfsd exports it.
func TestExportedTmpDeviceIsDevsOwn(t *testing.T) {
	self, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(self), " / /tmp ") {
		t.Skip("/tmp is not a mount here")
	}
	if got := exportedTmpDevice(); got == "" {
		t.Error("exportedTmpDevice found no /tmp in init's mount table")
	}
}

// A line too short to hold the mount options is skipped, not read past.
func TestReadMountinfoSkipsAShortLine(t *testing.T) {
	mounts, _ := readMountinfo(strings.NewReader("1 2 0:1 / /x - tmpfs tmpfs rw\n"))
	if len(mounts) != 0 {
		t.Errorf("got %v, want none", mounts)
	}
}

// A holder that opened a file through a bind of the root filesystem names the bind's path;
// onRootFS gives the path the job reaches it by. The deepest bind wins, a bind of another
// filesystem is not one, and a mount point is matched by whole components.
func TestOnRootFSTranslatesAPathUnderARootBind(t *testing.T) {
	binds := rootBinds(strings.NewReader(mixedMountinfo +
		"70 29 259:2 /home/testuser/repo/data /srv/data2 rw - ext4 /dev/nvme0n1p2 rw\n" +
		"71 70 259:2 /var/lib/x /srv/data2/inner rw - ext4 /dev/nvme0n1p2 rw\n"))
	for in, want := range map[string]string{
		"/srv/data2/app.db-wal":        "/home/testuser/repo/data/app.db-wal",
		"/srv/data2":                   "/home/testuser/repo/data",
		"/srv/data2/inner/l":           "/var/lib/x/l",
		"/srv/data20/x":                "/srv/data20/x",
		"/home/testuser/code/linked/f": "/home/testuser/elsewhere/f",
		"/srv/data/x":                  "/srv/data/x",
		"/etc/ro/f":                    "/etc/ro/f",
	} {
		if got := onRootFS(binds, in); got != want {
			t.Errorf("onRootFS(%q) = %q, want %q", in, got, want)
		}
	}
}

// A directory bound read-only onto itself shows dev's own files, so it keeps no job here,
// but the export carries it read-write: a remote job would write where a local one gets
// EROFS, so the job sees it read-only.
func TestAReadOnlySelfBindIsReadOnlyInTheJob(t *testing.T) {
	info := `22 1 259:2 / / rw,noatime shared:1 - ext4 /dev/nvme0n1p2 rw
90 22 259:2 /home/testuser/code/frozen /home/testuser/code/frozen ro,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
91 22 259:2 /home/testuser/code/open /home/testuser/code/open rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
92 22 8:17 / /srv/data ro shared:91 - ext4 /dev/sdb1 ro
`
	if got := readOnlySelfBinds(strings.NewReader(info)); strings.Join(got, "|") != "/home/testuser/code/frozen" {
		t.Errorf("got %q, want only the read-only self-bind", got)
	}
	if got := unexportedMounts(strings.NewReader(info), ""); slices.Contains(got, "/home/testuser/code/frozen") {
		t.Errorf("unexported %q: a self-bind shows dev's files and must keep no job here", got)
	}
	if got := readOnlyMounts(readOnlySelfBinds(strings.NewReader(info)), []string{"/home/testuser/code"}); !slices.Contains(got, "/home/testuser/code/frozen") {
		t.Errorf("read-only %q, want the self-bind inside the writable path", got)
	}
}
