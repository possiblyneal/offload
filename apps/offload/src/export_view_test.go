package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestExportViewSharesOnlyWorkspaceTreesFromHome(t *testing.T) {
	needNamespaces(t, "-rm", "mount -t tmpfs tmpfs /run")
	viewScript, err := os.Open("../scripts/export-view")
	if err != nil {
		t.Fatal(err)
	}
	defer viewScript.Close()
	cmd := exec.Command("unshare", "-rm", "--propagation", "private", "bash", "-euc", `
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /home
home=/home/probe
view=/run/view
mkdir -p "$view" "$home"/{code,orca/workspaces,.ssh,.claude,orca/state}
mkdir "$home/code/nested"
mount -t tmpfs tmpfs "$home/code/nested"
touch "$home/code/nested/private"
bash /proc/self/fd/3 "$view" "$home"
for path in code orca/workspaces; do
  [[ "$home/$path" -ef "$view$home/$path" ]]
  if ! findmnt -M "$view$home/$path" -n -o VFS-OPTIONS | grep -qw rw; then exit 1; fi
done
for path in .ssh .claude orca/state code/nested/private; do
  [[ ! -e "$view$home/$path" ]]
done
for path in /etc /usr; do
  if ! findmnt -M "$view$path" -n -o VFS-OPTIONS | grep -qw ro; then exit 1; fi
done
`)
	cmd.ExtraFiles = []*os.File{viewScript}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restricted export view: %v\n%s", err, out)
	}
}
