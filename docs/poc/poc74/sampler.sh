#!/bin/bash
# sampler.sh <outfile> <stopfile>, run as root inside a container: reads every /proc/*/cmdline about
# every 20 ms (as POC-61) and appends each distinct argv seen to <outfile>, then a SAMPLES count.
# run.sh searches the file for the test passwords afterwards without printing it.
out=$1; stop=$2; n=0; rm -f $stop; : > $out
declare -A seen
while [ ! -e $stop ]; do
  for p in /proc/[0-9]*; do
    a=$(tr '\0' ' ' < $p/cmdline 2>/dev/null) || continue
    [ -n "$a" ] && [ -z "${seen[$a]:-}" ] && { seen[$a]=1; echo "$a" >> $out; }
  done
  n=$((n+1)); sleep 0.02
done
echo "SAMPLES=$n" >> $out
