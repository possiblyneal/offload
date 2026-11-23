#!/usr/bin/env bash
# Remove what install.sh set up, idempotently: the export, the NFS server config, each
# remote's launcher and remote-local caches, ~/.local/bin/offload and its health timer.
# The nfs packages stay installed. Jobs hold no mount outside their own namespaces, so
# there is none to unmount. Refuses, changing nothing, while any remote runs a job or
# cannot say whether it does (refuse_while_jobs). Leaves nfs-server enabled when it still
# serves another export.
set -euo pipefail

UNIT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF="$UNIT/config/offload.conf"

# shellcheck source=SCRIPTDIR/scripts/remotes.sh
. "$UNIT/scripts/remotes.sh"
# Read leniently, not by read_remotes: a config with its remotes removed must still
# uninstall dev's export, which would otherwise go on serving the old addresses.
mapfile -t remote_addrs < <(awk '$1 == "remote" && $3 != "" { print $3 }' "$CONF")
refuse_while_jobs "not uninstalling"

systemctl --user disable --now offload-health.timer >/dev/null 2>&1 || true
SYSTEMD="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
rm -f "$HOME/.local/bin/offload" "$HOME/.local/share/offload/auto.bash" \
  "$SYSTEMD/offload-health.service" "$SYSTEMD/offload-health.timer"
systemctl --user daemon-reload

sudo rm -f /etc/exports.d/offload.exports /etc/nfs.conf.d/offload.conf /etc/systemd/system/nfs-server.service.d/offload.conf
sudo systemctl daemon-reload
if dpkg -s nfs-kernel-server >/dev/null 2>&1; then
  sudo exportfs -ra
  [[ -n "$(sudo exportfs -s)" ]] || sudo systemctl disable --now nfs-server >/dev/null 2>&1 || true
fi

for addr in "${remote_addrs[@]}"; do
  remote_ssh "$addr" \
    'sudo systemctl disable --now offload-net.service offload-cut.service >/dev/null 2>&1; sudo rm -f /etc/systemd/system/offload-net.service /etc/systemd/system/offload-cut.service /etc/systemd/system/offload-net.service.new /etc/systemd/system/offload-cut.service.new; sudo systemctl daemon-reload
     sudo rm -rf /usr/local/libexec/offload-launch /usr/local/libexec/offload-bind /usr/local/libexec/offload-forward /usr/local/libexec/offload-wait /usr/local/libexec/offload-launch.new /usr/local/libexec/offload-bind.new /usr/local/libexec/offload-forward.new /usr/local/libexec/offload-wait.new /etc/offload /var/lib/offload'
done
echo "offload: uninstalled"
