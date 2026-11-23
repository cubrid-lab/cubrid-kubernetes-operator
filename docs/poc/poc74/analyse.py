# analyse.py <out>: compares the acknowledged writes with the rows every member held.
# Acknowledged: writes.tsv lines ending in "ack", batch-*.txt lines "<id> ack", and id 900001 when
# the session held open on the master printed "1 row affected." for its insert. For each
# rows-<tag>-<member>.txt it prints how many acknowledged ids are missing on that member and how
# many ids it holds that were never acknowledged, against the writes acknowledged before it was read.
import glob, os, sys

out = sys.argv[1]
acked, unknown = set(), set()
for line in open(os.path.join(out, "writes.tsv")):
    f = line.rstrip("\n").split("\t")
    if len(f) == 4 and f[0].isdigit():
        (acked if f[3] == "ack" else unknown).add(int(f[0]))
for path in sorted(glob.glob(os.path.join(out, "batch-*.txt"))):
    for line in open(path):
        i, outcome = line.split()
        (acked if outcome == "ack" else unknown).add(int(i))
hold = open(os.path.join(out, "hold-master.txt"), errors="replace").read()
if "INSERT INTO dba.t VALUES (900001" in hold:
    (acked if "1 row affected." in hold.split("INSERT INTO dba.t VALUES (900001", 1)[1] else unknown).add(900001)
print("acknowledged writes: %d, unknown outcome: %d" % (len(acked), len(unknown)))
for path in sorted(glob.glob(os.path.join(out, "rows-*.txt"))):
    tag = os.path.basename(path)[5:-4]
    held = {int(x) for x in open(path).read().split()}
    # Each rows file is compared with the writes acknowledged before it was read:
    # rows-after-change before the batches (ids from 100001), rows-after-failover before
    # restart-check.sh's batch (ids from 300001), rows-final after everything.
    limit = {"after-change": 100001, "after-failover": 300001}.get(tag.rsplit("-poc74", 1)[0], None)
    expect = {i for i in acked if limit is None or i < limit or i == 900001}
    missing = sorted(expect - held)
    extra = sorted(held - acked)
    print("%-36s rows=%-5d acknowledged-expected=%-5d missing=%d %s extra=%d %s" % (
        tag, len(held), len(expect), len(missing), missing[:10], len(extra), extra[:10]))
