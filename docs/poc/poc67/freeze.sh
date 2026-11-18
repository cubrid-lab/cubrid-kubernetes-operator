#!/bin/bash
# freeze.sh stop|cont <pod> <copylogdb|applylogdb> <peer>
# Sends SIGSTOP or SIGCONT to the one copylogdb or applylogdb process of <pod> that works on
# <peer>'s log, found by its command line under /proc (the image has no ps). The process keeps
# its state, its connection and its files; SIGCONT lets it go on from where it was.
# The pattern is anchored on `cub_admin `: unanchored, it also matched the exec's own shell,
# whose command line contains the pattern, and stopped it (it happened once in this run).
# Used because `cubrid heartbeat copylogdb|applylogdb stop` answered SUCCESS on this build and
# left the processes running (see README).
. "$(dirname "$0")/env.sh"
sig=$1; pod=$2; kind=$3; peer=$4
case "$sig" in stop) s=STOP ;; cont) s=CONT ;; *) echo "stop|cont" >&2; exit 2 ;; esac
C "$pod" "for p in /proc/[0-9]*; do a=\$(tr '\\0' ' ' < \$p/cmdline 2>/dev/null); case \"\$a\" in 'cub_admin $kind -L /var/lib/cubrid/databases/${DB}_$peer '*) kill -$s \${p#/proc/} && echo \"\${p#/proc/} \$a SIG$s\"; grep State: \$p/status;; esac; done"
