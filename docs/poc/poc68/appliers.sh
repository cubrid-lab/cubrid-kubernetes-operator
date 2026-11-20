#!/bin/bash
# appliers.sh <member> [STOP|CONT]: lists every copylogdb and applylogdb process of <member> with
# its pid, kernel state and command line, read from /proc (the image has no ps). With STOP or CONT
# it first sends that signal to every applylogdb process of the member (every applier, whatever
# peer's log it applies). The match is anchored on `cub_admin applylogdb `, so the shell of this
# exec, whose command line holds the pattern, is never signalled (POC-67, freeze.sh).
. "$(dirname "$0")/env.sh"
n=$1; sig=${2:-}
docker exec -u cubrid $n bash -c "
for p in /proc/[0-9]*; do
  a=\$(tr '\\0' ' ' < \$p/cmdline 2>/dev/null)
  case \"\$a\" in
    'cub_admin applylogdb '*) if [ -n '$sig' ]; then kill -$sig \${p#/proc/} && echo \"sent SIG$sig to \${p#/proc/}\"; fi ;;
  esac
done
for p in /proc/[0-9]*; do
  a=\$(tr '\\0' ' ' < \$p/cmdline 2>/dev/null)
  case \"\$a\" in
    'cub_admin applylogdb '*|'cub_admin copylogdb '*) echo \"\${p#/proc/} \$(grep State: \$p/status | tr -s '\t ' ' ') | \$a\" ;;
  esac
done"
