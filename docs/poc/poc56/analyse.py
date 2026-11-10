#!/usr/bin/env python3
"""POC-56: recomputes every stated fact from the stored raw samples and exits 0
only if each of them holds. Usage: analyse.py <samples-dir>
Each subdirectory of samples is one whole run on one image."""
import json
import re
import sys
from pathlib import Path

DATA = "/var/lib/cubrid"
IN_SCOPE = (f"{DATA}/databases", f"{DATA}/restore-staging")
OPERATIONS = f"{DATA}/operations"
ROW_BEFORE, ROW_AFTER, ROW_B = "cluster-a-before-seed-backup", "cluster-a-after-seed-backup", "cluster-b-only"
REFUSED = ["other-cluster", "slave-named-master", "slave-named-itself", "other-cluster-staging-held", "slave-staging-held"]
ARTIFACT = {"other-cluster": "other-cluster", "other-cluster-staging-held": "other-cluster", "slave-named-master": "slave",
            "slave-named-itself": "slave", "slave-staging-held": "slave", "accepted": "good"}
ACCESS = re.compile(r'^(\S+) \S+ \S+ \[[^]]+\] "(\S+) (\S+) [^"]*" (\d+) (\S+)')
SECRET_PREFIXES = ["poc56tok", "poc56key", "poc56sec", "Bearer "]
failures = []


def check(ok, what):
    if not ok:
        failures.append(what)
        print(f"  FAILED: {what}")


def load(p):
    return json.loads(Path(p).read_text())


def snapshot(p):
    entries = {}
    for line in Path(p).read_text().splitlines():
        f = line.split("\t")
        entries[f[6]] = tuple(f[:6])
    return entries


def in_scope(path):
    return any(path == s or path.startswith(s + "/") for s in IN_SCOPE)


def diff(a, b):
    return sorted(k for k in set(a) | set(b) if a.get(k) != b.get(k))


def store_requests(p):
    requests = []
    for line in Path(p).read_text().splitlines():
        if line.strip():
            m = ACCESS.match(line)
            check(m is not None, f"unreadable store log line {line!r}")
            if m:
                requests.append(m.groups())
    return requests


def classify(path):
    if "?location" in path:
        return "bucket location"
    if path.endswith("/manifest.json"):
        return "manifest"
    if "/backup/" in path:
        return "backup object"
    return "other"


def im_events(p):
    events = []
    for line in Path(p).read_text().splitlines():
        if line.startswith("{"):
            events.append(json.loads(line))
    return events


def commands(events):
    return [e["command"] for e in events if e.get("event") == "command"]


def artifact_facts(run):
    print(" artifacts, each taken and uploaded by the Instance Manager, read back from the store:")
    for name in ["good", "other-cluster", "slave", "first-seed-a", "first-seed-b"]:
        m = load(run / "artifacts" / f"{name}.manifest.json")
        o = load(run / "artifacts" / f"{name}.objects.json")
        check([(x["key"], x["sizeBytes"], x["sha256"]) for x in m["objects"]] == [(x["key"], x["size"], x["sha256"]) for x in o["objects"]],
              f"{name}: objects read back differ from the manifest")
        check(len(m["objects"]) >= 1, f"{name}: no objects")
        print(f"  {name}: cluster {m['clusterUID'][:8]} source {m['sourceInstance']}/{m['sourceRole']} engine {m['cubridVersion']} "
              f"level {m['level']} objects {len(m['objects'])} ({sum(x['sizeBytes'] for x in m['objects'])} bytes, sizes and SHA-256 match) operation {m.get('operationID')}")


def setup_facts(run):
    rows = [json.loads(l) for l in (run / "setup" / "a-master-one-member-missing.jsonl").read_text().splitlines()]
    missing = {n["state"] for r in rows for n in r["answer"]["nodes"] if n["name"] == "demo-2"}
    check(len(rows) >= 5 and all(r["answer"]["role"] == "master" and not r["answer"]["serverActive"] for r in rows),
          "the master was active while a listed member had not joined")
    print(f" master with demo-1 joined and demo-2 not: {len(rows)} role answers from {rows[0]['at']} to {rows[-1]['at']}, "
          f"role master in all, serverActive false in all, demo-2 reported as {sorted(missing)}")
    active = [l.split("\t")[0] for l in (run / "events.tsv").read_text().splitlines() if "\tactive master\t" in l]
    check(len(active) == 2, "both masters did not become active")
    print(f" masters active once every listed member had joined, at {active}")
    a = load(run / "setup" / "seed-backup-for-empty-demo-2.role.json")
    check(a["serverActive"] and a["role"] == "master", "the source of the accepted seed was not an active master")
    slave = load(run / "setup" / "slave-backup.role.json")
    check(slave["role"] == "slave", "the slave backup's source did not answer slave")
    shas = [load(run / "artifacts" / f"{n}.objects.json")["objects"][0]["sha256"] for n in ["good", "other-cluster", "slave"]]
    check(len(set(shas)) == 3, "two artifacts hold the same object")
    print(f" slave backup's source answered role {slave['role']!r} just before it; the three artifacts' objects all differ")


def refused_case(run, name, facts):
    d = run / "cases" / name
    req = load(d / "request.json")["body"]
    response, op, window = load(d / "response.json"), load(d / "operation.json"), load(d / "window.json")
    m = load(run / "artifacts" / f"{ARTIFACT[name]}.manifest.json")
    if ARTIFACT[name] == "other-cluster":
        want = f'manifest clusterUID "{m["clusterUID"]}" does not match expected "{req["expectedClusterUID"]}"'
        check(m["clusterUID"] != req["expectedClusterUID"], f"{name}: same cluster")
        check(m["sourceInstance"] == req["seedFromMaster"] and m["sourceRole"] == "master",
              f"{name}: differs in more than the cluster, so the refusal says less")
    else:
        want = 'manifest sourceRole "slave" does not match required "master"'
        check(m["sourceRole"] == "slave" and m["clusterUID"] == req["expectedClusterUID"], f"{name}: artifact is not this cluster's slave backup")
    check(m["database"] == req["database"] and m["level"] == 0 and m["cubridVersion"].startswith(req["expectedCubridVersion"]),
          f"{name}: differs in database, level or engine")
    if name == "slave-named-itself":
        check(m["sourceInstance"] == req["seedFromMaster"], f"{name}: the source role is not the only difference")

    check(response["status"] == 202, f"{name}: status {response['status']}")
    check(op["state"] == "Failed", f"{name}: state {op['state']}")
    reason = op.get("failureReason", "")
    check(want in reason, f"{name}: reason {reason!r} does not hold {want!r}")

    before, after = snapshot(d / "before.txt"), snapshot(d / "after.txt")
    changed = diff(before, after)
    scope_paths = [p for p in before if in_scope(p)]
    check(not [p for p in changed if in_scope(p)], f"{name}: changed in the member's directories: {[p for p in changed if in_scope(p)]}")
    check(all(p == OPERATIONS or p.startswith(OPERATIONS + "/") for p in changed), f"{name}: changed outside the operation records: {changed}")
    if "staging-held" in name:
        held = [p for p in before if "left-by-an-earlier-attempt" in p]
        check(len(held) == 1 and held[0] in after, f"{name}: the file in staging is not there both times")

    reqs = store_requests(d / "store-access.log")
    check(all(r[0] == window["targetIP"] for r in reqs), f"{name}: a request from another client in the window")
    kinds = [classify(r[2]) for r in reqs]
    check(all(r[1] == "GET" for r in reqs), f"{name}: a store request that is not a GET")
    check(kinds.count("manifest") == 1, f"{name}: {kinds.count('manifest')} manifest reads")
    check("backup object" not in kinds and "other" not in kinds, f"{name}: store requests {kinds}")

    events = im_events(d / "im.log")
    cmds = commands(events)
    check(not [c for c in cmds if c not in ("cubrid_rel", "cubrid heartbeat status")], f"{name}: commands ran: {cmds}")
    states = [e["state"] for e in events if e.get("event") == "operation_state_changed"]
    text = (d / "processes.txt").read_text()
    samples = text.count("sample ")
    procs = [l for l in text.splitlines() if re.search(r"\bcub_|restoredb|restoreslave|backupdb|cubrid ", l)]
    check(samples >= 5 and not procs, f"{name}: {samples} process samples, CUBRID lines {procs[:3]}")
    facts.append((name, response["status"], op["state"], reason.split(": ")[-1], kinds, cmds, states, len(scope_paths), len(changed), samples))


def accepted_case(run):
    name = "accepted"
    d = run / "cases" / name
    req = load(d / "request.json")["body"]
    response, op = load(d / "response.json"), load(d / "operation.json")
    m = load(run / "artifacts" / "good.manifest.json")
    check(response["status"] == 202 and op["state"] == "Completed", f"accepted: {response['status']} {op['state']} {op.get('failureReason')}")
    check(m["clusterUID"] == req["expectedClusterUID"] and m["sourceRole"] == "master" and m["sourceInstance"] == req["seedFromMaster"], "accepted: manifest does not match the request")
    reqs = store_requests(d / "store-access.log")
    kinds = [classify(r[2]) for r in reqs]
    got = {r[2].split("/backup/")[1] for r in reqs if classify(r[2]) == "backup object" and r[3] in ("200", "206")}
    want = {o["key"].split("/", 1)[1] for o in m["objects"]}
    check(got == want, f"accepted: objects read {got}, manifest lists {want}")
    cmds = commands(im_events(d / "im.log"))
    slave = [c for c in cmds if c.startswith("cubrid restoreslave ")]
    check(len(slave) == 1 and "-m demo-0 " in slave[0], f"accepted: restoreslave {slave}")
    check("cubrid heartbeat start" in cmds, "accepted: heartbeat was not started")
    before, after = snapshot(d / "before.txt"), snapshot(d / "after.txt")
    check(f"{DATA}/databases/appdb" not in before, "accepted: the member was not empty before")
    check(not [p for p in before if p.startswith(f"{DATA}/restore-staging")], "accepted: staging was not empty before")
    check(f"{DATA}/databases/appdb/appdb" in after or any(p.startswith(f"{DATA}/databases/appdb/") for p in after), "accepted: no database after")
    check(not [p for p in after if p.startswith(f"{DATA}/restore-staging/") ], "accepted: staging was left behind")
    check(f"{DATA}/databases/appdb/.im-operation" not in after, "accepted: the ownership marker was left")
    role = load(run / "member" / "role-after.json")
    check(role["role"] == "slave" and any(n["name"] == "demo-0" and n["state"] == "master" for n in role["nodes"]), "accepted: not a slave of demo-0")
    polls = (run / "member" / "rows-after.txt").read_text().split("\n=====\n")
    last = polls[-1]
    check("ERROR" not in last, f"accepted: the last read failed: {last[-300:]}")
    for row, present in [(ROW_BEFORE, True), (ROW_AFTER, True), (ROW_B, False)]:
        check((f"row={row}" in last) == present, f"accepted: row {row} present={not present}")
    check(not any(f"row={ROW_B}" in p for p in polls), "accepted: cluster B's row was seen")
    on_master = re.findall(r"row=([a-z0-9-]+)", (run / "member" / "rows-on-master.txt").read_text())
    check(on_master == re.findall(r"row=([a-z0-9-]+)", last), "accepted: the master holds other rows")
    hb = (run / "member" / "heartbeat-after.txt").read_text()
    check("current demo-2, state slave" in hb, "accepted: heartbeat status")
    print(f" accepted: {response['status']} then {op['state']}; store {sorted(set(kinds))} "
          f"with {kinds.count('backup object')} object read(s) of {len(want)}; commands {[c.split(' -')[0] for c in cmds if 'status' not in c and c != 'cubrid_rel']}")
    print(f"  role {role['role']}, rows on the member after {len(polls)} read(s): {re.findall(r'row=([a-z0-9-]+)', last)}")
    print(f"  process samples showing cub_admin: {(d / 'processes.txt').read_text().count('cub_admin')}")
    print(f"  files in the database directory after: {len([p for p in after if p.startswith(DATA + '/databases/appdb/')])}")


def one_run(run):
    print(f"\n=== run {run.name}")
    img, store = load(run / "image" / "image.json"), load(run / "image" / "store.json")
    binary = load(run / "image" / "binary.json")
    print(f" image {img['reference']}\n  id {img['id']}\n  created {img['created']}\n  store {store['reference']}")
    print(f" {(run / 'image' / 'cubrid_rel.txt').read_text().strip()}")
    print(f" manager binary {binary['sha256'][:16]}, holds {binary['holds']}")
    check(binary["holds"]["does not match required"], "the binary does not hold the role check's message")
    info = load(run / "run.json")
    print(f" Docker {info['docker']}; ran {info['started']} to {info['ended']}, {info['seconds']} s, load at end {info['loadAtEnd']}")
    artifact_facts(run)
    setup_facts(run)
    print(f" empty member's role answer before: {load(run / 'member' / 'role-before.json').get('role')!r}")
    facts = []
    for name in REFUSED:
        refused_case(run, name, facts)
    print(" refusals (status, final state, reason; store requests; commands; states; paths compared; paths changed outside operations; process samples):")
    for f in facts:
        print(f"  {f[0]}: {f[1]} {f[2]}: {f[3]}\n    store {f[4]}; commands {f[5]}; states {f[6]}; {f[7]} paths unchanged; {f[8]} operation-record paths changed; {f[9]} process samples")
    accepted_case(run)
    loads = sorted(float(load(run / "cases" / n / "window.json")["load"]) for n in REFUSED + ["accepted"])
    print(f" load average at the end of each case: {loads[0]} to {loads[-1]}")
    check(load(run / "scrub-check.json")["occurrences"] == 0, "the harness found a credential in the samples")


def main():
    root = Path(sys.argv[1])
    runs = sorted(p for p in root.iterdir() if (p / "run.json").exists())
    check(bool(runs), "no runs")
    for run in runs:
        one_run(run)
    hits = [(str(p), s) for p in root.rglob("*") if p.is_file() for s in SECRET_PREFIXES if s.encode() in p.read_bytes()]
    check(not hits, f"credential patterns in the samples: {hits[:5]}")
    print(f"\ncredential patterns in {sum(1 for p in root.rglob('*') if p.is_file())} stored files: {len(hits)}")
    print(f"checks failed: {len(failures)}")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
