#!/usr/bin/env python3
"""POC-56: shows that analyse.py can fail. Each change below is made to a fresh
copy of the samples, as the fault it stands for would have looked, and
analyse.py must then exit 1. Usage: mutate.py <samples-dir>"""
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
RUN = "current-main"
TARGET = "cases/other-cluster"


def add(rel, line):
    def change(root):
        p = root / RUN / rel
        p.write_text(p.read_text() + line + "\n")
    return change


def replace(rel, old, new):
    def change(root):
        p = root / RUN / rel
        text = p.read_text()
        if old not in text:
            sys.exit(f"{rel} does not hold {old!r}: this change would test nothing")
        p.write_text(text.replace(old, new))
    return change


MUTATIONS = {
    "a file appeared in the database directory after a refusal":
        add(f"{TARGET}/after.txt", "-rw-------\t1000\t1\t1\t99\tabc\t/var/lib/cubrid/databases/appdb"),
    "a file appeared in staging after a refusal":
        add(f"{TARGET}/after.txt", "-rw-------\t1000\t1\t1\t99\tabc\t/var/lib/cubrid/restore-staging/x/backup/appdb_bk0v000"),
    "a backup object was read during a refusal":
        add(f"{TARGET}/store-access.log", '192.168.64.5 - - [07/Nov/2026:05:23:44 +0000] "GET /poc56-seed/x/backup/appdb_bk0v000 HTTP/1.1" 200 2110464'),
    "a command ran during a refusal":
        add(f"{TARGET}/im.log", '{"event":"command","command":"cubrid restoreslave -u -s master -m demo-0 -B /x appdb"}'),
    "cub_admin was seen during a refusal":
        add(f"{TARGET}/processes.txt", "cub_admin restoreslave -u"),
    "the refusal gave another reason":
        replace(f"{TARGET}/operation.json", "does not match expected", "differs from"),
    "the refused request completed":
        replace(f"{TARGET}/operation.json", '"state": "Failed"', '"state": "Completed"'),
    "the other cluster's row reached the member":
        add("member/rows-after.txt", "  'row=cluster-b-only'"),
    "the accepted member lacks the row replication brings":
        replace("member/rows-after.txt", "row=cluster-a-after-seed-backup", "row=gone"),
    "the accepted case read no object":
        replace("cases/accepted/store-access.log", "/backup/", "/elsewhere/"),
    "a generated token reached the samples":
        add("logs/store-access.log", "poc56tok0123"),
}


def main():
    samples = Path(sys.argv[1]).resolve()
    missed = 0
    for what, change in MUTATIONS.items():
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "samples"
            shutil.copytree(samples, root)
            change(root)
            r = subprocess.run([sys.executable, "-I", str(HERE / "analyse.py"), str(root)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        caught = r.returncode == 1 and "FAILED" in r.stdout
        missed += not caught
        print(f"{'caught' if caught else 'MISSED'}: {what}")
    print(f"{len(MUTATIONS) - missed} of {len(MUTATIONS)} caught")
    sys.exit(1 if missed else 0)


if __name__ == "__main__":
    main()
