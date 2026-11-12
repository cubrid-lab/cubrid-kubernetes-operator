#!/bin/bash
# Usage: sampler.sh <outfile> <stopfile>. Reads every /proc/*/cmdline about every 20 ms;
# writes each distinct argv seen, and a SAMPLES count. Run as root to see all processes.
out=$1; stop=$2; n=0; rm -f $stop
declare -A seen
while [ ! -e $stop ]; do
  for p in /proc/[0-9]*; do
    a=$(tr '\0' ' ' < $p/cmdline 2>/dev/null) || continue
    [ -n "$a" ] && [ -z "${seen[$a]:-}" ] && { seen[$a]=1; echo "$a" >> $out; }
  done
  n=$((n+1)); sleep 0.02
done
echo "SAMPLES=$n" >> $out
