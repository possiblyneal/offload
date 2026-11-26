# Source from Bash startup to route direct Go build and test invocations.
# Do not export the function: scripts and explicit Offload jobs retain their own behavior.
go() {
  if [[ ${OFFLOAD_AUTO-} != 0 && -z ${OFFLOAD_JOB-} && -x $HOME/.local/bin/offload ]] &&
    [[ ${1-} = build || ${1-} = test ]] &&
    [[ -t 0 || /proc/self/fd/0 -ef /dev/null ]]; then
    local arg flag real_go goflags need_value=0 packages=0 after_packages=0
    for arg in "${@:2}"; do
      if ((need_value)); then
        need_value=0
        continue
      fi
      if [[ $arg != -* ]]; then
        if [[ $1 = test ]] && ((after_packages)); then
          command go "$@"
          return
        fi
        packages=1
        continue
      fi
      if ((packages)); then after_packages=1; fi
      arg=${arg#-}
      arg=${arg#-}
      flag=${arg%%=*}
      # Keep custom executors and explicit output paths local for either command.
      case ${flag#test.} in
        '' | C | exec | toolexec | args | o | *profile | outputdir | trace)
          command go "$@"
          return
          ;;
      esac
      if [[ $1 = test ]]; then
        # Only test-binary flags have a test. alias. Driver flags do not.
        case $flag in
          cover | json | race) continue ;;
          covermode | coverpkg | tags) ;;
          *)
            case ${flag#test.} in
              benchmem | failfast | fullpath | short | v) continue ;;
              bench | benchtime | count | cpu | fuzz | fuzzminimizetime | fuzztime | list | parallel | run | shuffle | skip | timeout) ;;
              *) command go "$@"; return ;;
            esac
            ;;
        esac
        if [[ $arg != *=* ]]; then need_value=1; fi
      fi
    done
    if ((need_value)); then command go "$@"; return; fi
    # Check persistent GOFLAGS too: an alternate executor can need local input.
    if real_go=$(type -P go) && [[ $real_go = /* ]] &&
      goflags=$("$real_go" env GOFLAGS 2>/dev/null) && [[ -z $goflags ]]; then
      "$HOME/.local/bin/offload" -- "$real_go" "$@" </dev/null
      return
    fi
  fi
  command go "$@"
}
