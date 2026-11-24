# Source from Bash startup to route direct Go build and test invocations.
# Do not export the function: scripts and explicit Offload jobs retain their own behavior.
go() {
  if [[ ${OFFLOAD_AUTO-} != 0 && -z ${OFFLOAD_JOB-} && -x $HOME/.local/bin/offload ]] &&
    [[ ${1-} = build || ${1-} = test ]] &&
    [[ -t 0 || /proc/self/fd/0 -ef /dev/null ]]; then
    local arg real_go goflags
    for arg in "$@"; do
      # Keep custom executors, test arguments, and explicit output paths local.
      [[ $arg = -* ]] || continue
      arg=${arg#-}
      arg=${arg#-}
      arg=${arg#test.}
      case ${arg%%=*} in
        '' | exec | toolexec | args | o | *profile | outputdir | trace)
          command go "$@"
          return
          ;;
      esac
      if [[ $1 = test ]]; then
        # Go can forward unknown flags to the test binary without -args.
        case ${arg%%=*} in
          bench | benchmem | benchtime | count | cover | covermode | coverpkg | cpu | failfast | fullpath | fuzz | fuzzminimizetime | fuzztime | json | list | parallel | race | run | short | shuffle | skip | timeout | v) ;;
          *) command go "$@"; return ;;
        esac
      fi
    done
    # Check persistent GOFLAGS too: an alternate executor can need local input.
    if real_go=$(type -P go) && [[ $real_go = /* ]] &&
      goflags=$("$real_go" env GOFLAGS 2>/dev/null) && [[ -z $goflags ]]; then
      "$HOME/.local/bin/offload" -- "$real_go" "$@" </dev/null
      return
    fi
  fi
  command go "$@"
}
