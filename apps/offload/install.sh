#!/usr/bin/env bash
# Set up offloading from dev, idempotently: a synthetic NFS export limited to the remotes in
# config/offload.conf, the launcher on each remote, ~/.local/bin/offload and its health
# timer. Needs ssh and passwordless sudo on every machine. `uninstall.sh` undoes all of it.
#   install.sh [--export-only]   dev's export and nfsd tuning only: no remote, no command
set -euo pipefail

UNIT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF="$UNIT/config/offload.conf"
export_only=0
case "$#:${1-}" in
  0:) ;;
  1:--export-only) export_only=1 ;;
  *)
    echo "usage: install.sh [--export-only]" >&2
    exit 2
    ;;
esac
# shellcheck source=SCRIPTDIR/scripts/remotes.sh
. "$UNIT/scripts/remotes.sh"
read_remotes "$CONF" "nothing installed"

# An export-only migration must not strand an installed legacy wrapper. Accept only
# unambiguous linker metadata; never execute the wrapper to inspect it.
scoped_wrapper() {
  local metadata line flags='' token next=0 count=0 flag_lines=0
  local -a words
  metadata=$(go version -m "$HOME/.local/bin/offload" 2>/dev/null) || return 1
  while IFS= read -r line; do
    if [[ $line =~ ^[[:space:]]*build[[:space:]]+-ldflags=\"([^\"]*)\"$ ]]; then
      ((flag_lines += 1))
      [[ $flag_lines == 1 ]] || return 1
      flags=${BASH_REMATCH[1]}
    elif [[ $line == *-ldflags=* ]]; then
      return 1
    fi
  done <<<"$metadata"
  [[ -n $flags && $flags != *\\* && $flags != *\"* ]] || return 1
  read -r -a words <<<"$flags"
  for token in "${words[@]}"; do
    # Go treats a single quote as quoting only at the start of a field.
    [[ $token != \'* ]] || return 1
    if [[ $token == *main.scopedExportRoot* ]]; then
      [[ $next == 1 && $token == main.scopedExportRoot=/run/offload-export ]] || return 1
      ((count += 1))
    fi
    if [[ $token == -X ]]; then next=1; else next=0; fi
  done
  [[ $count == 1 ]]
}
if ((export_only)) && [[ -e $HOME/.local/bin/offload || -L $HOME/.local/bin/offload ]] &&
  ! scoped_wrapper; then
  echo 'offload: installed wrapper is not known to use the scoped export; run a full install; nothing installed' >&2
  exit 1
fi

# A running job would read a launcher half replaced (its job half re-execs the file),
# lose its firewall while the rules reload, or see its NFS calls fail while nfs-server
# restarts (--export-only too), so nothing is installed while any remote runs one, or
# cannot say whether it does.
refuse_while_jobs "nothing installed"

# dev: the export. Only the listed addresses may mount; root_squash keeps remote root
# from acting as dev's root. v4 only, with the threads pinned so serving does not wait
# on a thread pool sized by whatever the package defaults to. The threads are reniced to
# NFSD_NICE after each start (a systemd drop-in): they sit in the root cgroup, where
# CPUWeight cannot reach them, and at nice 0 they queue behind dev's own batch load.
NFSD_NICE=-10
if ! dpkg -s nfs-kernel-server >/dev/null 2>&1; then
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nfs-kernel-server
fi

write_root_file() { # <path> <content>
  if [[ "$(sudo cat "$1" 2>/dev/null || true)" != "$2" ]]; then
    printf '%s\n' "$2" | sudo install -D -m 0644 /dev/stdin "$1"
  fi
}

write_root_file /etc/nfs.conf.d/offload.conf "[nfsd]
threads=8
vers2=n
vers3=n
udp=n"

# Stop serving before replacing the view or its exports. A failed view start must leave
# nfs-server stopped rather than expose an underlying directory or an incomplete view.
home=$(getent passwd "$(id -u)" | cut -d: -f6)
[[ $home =~ ^/home/[a-zA-Z0-9_][a-zA-Z0-9_.-]*$ ]] || { echo 'offload: unsupported account home' >&2; exit 1; }
sudo systemctl stop nfs-server
if systemctl is-active --quiet offload-export-view.service; then
  sudo exportfs -f
  sudo systemctl stop offload-export-view.service
fi
sudo install -d -m 0755 /run/offload-export
sudo install -Dm0755 "$UNIT/scripts/export-view" /usr/local/libexec/offload-export-view
sudo install -Dm0644 "$UNIT/config/remote-local-paths" /etc/offload-export-paths
write_root_file /etc/systemd/system/offload-export-view.service "[Unit]
Description=Offload synthetic NFS export view
Before=nfs-server.service
[Service]
Type=oneshot
RemainAfterExit=yes
RuntimeDirectory=offload-export
RuntimeDirectoryMode=0755
ExecStart=/usr/local/libexec/offload-export-view /run/offload-export $home /etc/offload-export-paths
ExecStop=/usr/bin/umount --recursive /run/offload-export"
write_root_file /etc/systemd/system/nfs-server.service.d/offload.conf "[Unit]
Requires=offload-export-view.service
After=offload-export-view.service
[Service]
ExecStartPost=/bin/sh -c 'for p in \$(pgrep -x nfsd); do renice -n $NFSD_NICE -p \$p; done'"

exports=""
for path in / /usr /etc /tmp /var/tmp "$home/code" "$home/orca/workspaces"; do
  line="/run/offload-export${path%/}"
  case $path in
    /) options=ro,sync,root_squash,no_subtree_check,fsid=0 ;;
    /usr) options=ro,sync,root_squash,subtree_check,fsid=1 ;;
    /etc) options=ro,sync,root_squash,subtree_check,fsid=2 ;;
    /tmp) options=rw,sync,root_squash,subtree_check,fsid=3 ;;
    /var/tmp) options=rw,sync,root_squash,subtree_check,fsid=4 ;;
    "$home/code") options=rw,sync,root_squash,subtree_check,fsid=5 ;;
    "$home/orca/workspaces") options=rw,sync,root_squash,subtree_check,fsid=6 ;;
  esac
  for addr in "${remote_addrs[@]}"; do line+=" $addr($options)"; done
  exports+="${exports:+$'\n'}$line"
done
write_root_file /etc/exports.d/offload.exports "$exports"
sudo systemctl daemon-reload
sudo systemctl enable nfs-server >/dev/null 2>&1
sudo systemctl start nfs-server
sudo exportfs -ra
if ((export_only)); then
  echo "offload: export and nfsd tuning applied"
  exit 0
fi

# The remote helpers, built once for every remote: static, so the job's init loads nothing
# over NFS once dev's / is its root. linux/amd64, the one arch the remotes run.
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
for helper in offload-bind offload-forward offload-wait; do
  (cd "$UNIT/src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o "$build/$helper" "./cmd/$helper")
done

# Each remote: the NFS client, the launcher and its helpers, the remote-local path list.
for i in "${!remote_addrs[@]}"; do
  remote_ssh "${remote_addrs[$i]}" bash -s <<'REMOTE'
set -euo pipefail
for pkg in nfs-common nftables; do
  dpkg -s "$pkg" >/dev/null 2>&1 || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y "$pkg"
done
sudo install -d -m 0755 /usr/local/libexec /etc/offload /var/lib/offload
REMOTE
  # <mode> <local file> <remote path>: written beside it and checked against the local
  # file's sha256 (a connection cut mid-copy can still end the copy with a clean EOF). A
  # file that does not arrive whole removes every file staged so far, so the remote keeps
  # its old set whole; a cleanup the cut connection also stops leaves `.new` files that
  # the next install overwrites and uninstall removes.
  staged=()
  stage() {
    local sum
    sum=$(sha256sum <"$2") sum=${sum%% *}
    staged+=("$3")
    remote_ssh "${remote_addrs[$i]}" \
      "sudo install -m $1 /dev/stdin '$3.new' && echo '$sum  $3.new' | sudo sha256sum -c --quiet -" <"$2" && return
    echo "offload: $3.new did not arrive whole; nothing replaced" >&2
    remote_ssh "${remote_addrs[$i]}" "sudo rm -f ${staged[*]/%/.new}" || true
    exit 1
  }
  stage 0755 "$UNIT/src/offload-launch" /usr/local/libexec/offload-launch
  stage 0755 "$build/offload-bind" /usr/local/libexec/offload-bind
  stage 0755 "$build/offload-forward" /usr/local/libexec/offload-forward
  stage 0755 "$build/offload-wait" /usr/local/libexec/offload-wait
  stage 0644 "$UNIT/config/remote-local-paths" /etc/offload/remote-local-paths
  # The jobs' network: NAT out, nothing in. A unit loads the rules at boot.
  stage 0644 "$UNIT/config/offload-net.nft" /etc/offload/net.nft
  stage 0644 "$UNIT/deploy/systemd/offload-net.service" /etc/systemd/system/offload-net.service
  # Started, never restarted: its stop fails every running job's NFS calls.
  stage 0644 "$UNIT/deploy/systemd/offload-cut.service" /etc/systemd/system/offload-cut.service
  # Every file arrived whole, so each is renamed over the old one.
  renames=""
  for p in "${staged[@]}"; do renames+="sudo mv -f '$p.new' '$p' && "; done
  remote_ssh "${remote_addrs[$i]}" "${renames}true"
  remote_ssh "${remote_addrs[$i]}" \
    'sudo systemctl daemon-reload && sudo systemctl enable offload-net.service offload-cut.service >/dev/null 2>&1 &&
     sudo systemctl start offload-net.service offload-cut.service'
  # The rules replaced in one nft transaction, never restarted: a restart deletes the
  # tables before it loads them, leaving the jobs unfirewalled in between, or after it if
  # the load fails. Each `table` line creates the table if missing so its delete succeeds.
  { printf 'table inet offload_job\ndelete table inet offload_job\ntable ip offload_nat\ndelete table ip offload_nat\n'
    cat "$UNIT/config/offload-net.nft"; } |
    remote_ssh "${remote_addrs[$i]}" 'sudo nft -f /dev/stdin'
  echo "offload: ${remote_names[$i]} (${remote_addrs[$i]}) ready"
done

# dev: the command. The config path is baked in so the binary finds it from anywhere.
mkdir -p "$HOME/.local/bin"
(cd "$UNIT/src" && go build -ldflags "-X main.configDir=$UNIT/config -X main.scopedExportRoot=/run/offload-export" -o "$HOME/.local/bin/offload" .)
echo "offload: installed $HOME/.local/bin/offload"
install -Dm0644 "$UNIT/scripts/auto.bash" "$HOME/.local/share/offload/auto.bash"

# dev: the health check, every 30 minutes, reporting to the homelab panel. Copied, as the
# command is, so the timer never runs units from a checkout that may be removed.
SYSTEMD="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
install -Dm0644 -t "$SYSTEMD" "$UNIT/deploy/systemd/offload-health.service" "$UNIT/deploy/systemd/offload-health.timer"
systemctl --user daemon-reload
systemctl --user enable --now offload-health.timer
echo "offload: health check every 30 minutes (offload-health.timer)"
