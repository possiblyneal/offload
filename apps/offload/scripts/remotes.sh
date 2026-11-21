# shellcheck shell=bash
# Sourced by install.sh and uninstall.sh: how to reach a remote (remote_ssh), the remotes
# config/offload.conf lists, and the check that none of them runs a job. The last two take
# the words a refusal ends with ("nothing installed", "not uninstalling").

# remote_ssh <address> <command…>: ssh to a remote with no user config and no prompt,
# giving up on one that stops answering (about 15 s) rather than hanging the install.
remote_ssh() {
  ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 "$@"
}

# one_host <address>: an IPv4 address or a host name, never a pattern.
one_host() {
  local octet='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])' label='[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?'
  if [[ $1 =~ ^[0-9.]+$ ]]; then
    [[ $1 =~ ^($octet\.){3}$octet$ ]]
  else
    [[ $1 =~ ^$label(\.$label)*$ ]]
  fi
}

# read_remotes <conf> <refusal>: sets remote_names and remote_addrs, or exits 1. Every
# remote line names one host, by IPv4 address or host name: the export line is built from
# them, and an empty, wildcard, network or netgroup address there would let hosts other
# than the remotes mount dev's / read-write.
read_remotes() {
  local -a f
  remote_names=() remote_addrs=()
  while read -r -a f || ((${#f[@]})); do # the last line may lack its newline
    if [[ ${f[0]:-} == remote ]]; then
      if ((${#f[@]} != 3)) || ! one_host "${f[2]}"; then
        echo "offload: $1: \"${f[*]}\" is not \"remote <name> <IPv4 address or host name>\"; $2" >&2
        exit 1
      fi
      remote_names+=("${f[1]}") remote_addrs+=("${f[2]}")
    fi
  done <"$1"
  if ((${#remote_addrs[@]} == 0)); then
    echo "offload: $1 lists no remote; $2" >&2
    exit 1
  fi
}

# refuse_while_jobs <refusal>: exits 1 while any remote runs a job, or cannot say whether
# it does. A launcher still setting up has no scope yet, so it is looked for too, and a
# scope still stopping (`deactivating`, its processes alive, as in a close dev never
# answers) counts as running.
refuse_while_jobs() {
  local addr jobs
  for addr in "${remote_addrs[@]}"; do
    if ! jobs=$(remote_ssh "$addr" \
      "systemctl list-units --state=activating,active,deactivating,reloading 'offload-job-*.scope' --no-legend --plain &&
       { pgrep -af '/usr/local/libexec/[o]ffload-launch' || [ \$? = 1 ]; }"); then
      echo "offload: cannot list jobs on $addr; $1" >&2
      exit 1
    fi
    if [[ -n "$jobs" ]]; then
      echo "offload: jobs are running on $addr; $1:" >&2
      echo "$jobs" >&2
      exit 1
    fi
  done
}
