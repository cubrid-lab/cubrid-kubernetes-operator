#!/bin/bash
# copiers.sh <member> [STOP|CONT]: lists every copylogdb and applylogdb process of <member> with
# its pid, kernel state and command line, read from /proc (the image has no ps). With STOP or CONT
# it first sends that signal to the member's copylogdb for the master's log only: the process whose
# command line starts with `cub_admin copylogdb ` and ends with `@<ha-0>`. The copier of the other
# slave's log and both appliers are left alone. The anchored match keeps the shell of this exec,
# whose command line holds the pattern, from being signalled (as POC-67 and POC-68 do).
. "$(dirname "$0")/env.sh"
n=$1; sig=${2:-}
docker exec -u cubrid $n bash -c "
for p in /proc/[0-9]*; do
  a=\$(tr '\\0' ' ' < \$p/cmdline 2>/dev/null)
  case \"\$a\" in
    'cub_admin copylogdb '*'@$N0 ') if [ -n '$sig' ]; then kill -$sig \${p#/proc/} && echo \"sent SIG$sig to \${p#/proc/}\"; fi ;;
  esac
done
for p in /proc/[0-9]*; do
  a=\$(tr '\\0' ' ' < \$p/cmdline 2>/dev/null)
  case \"\$a\" in
    'cub_admin applylogdb '*|'cub_admin copylogdb '*) echo \"\${p#/proc/} \$(grep State: \$p/status | tr -s '\t ' ' ') | \$a\" ;;
  esac
done"
