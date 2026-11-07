#!/usr/bin/env python3
"""POC-53: rebuilds every version of the harness scripts from the command log of
the session that ran the trials, for a maintainer who holds that log. It keeps
only the commands that wrote those scripts, runs them in an empty directory, in
order, and saves each version with its checksum. The log is not in this
repository, and its commands run as they were recorded, so read them first.
Usage: replay.py <session-log.jsonl> <poc50-sampler.py> <empty-output-dir>
Prints the edits as runs.json holds them. The result is only as good as its last
line: each newest version must equal what was committed in 8499ad5."""
import gzip
import hashlib
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

NAMES = ["harness.py", "plans.py", "writer.py", "holder.py", "sampler.py"]
STEP = re.compile(r"(?P<cat>^cat >(?P<add>>?) ?(?P<file>[\w.]+) <<'EOF'\n(?P<body>[\s\S]*?)^EOF$)"
                  r"|(?P<py>python3 - <<'EOF'( > \S+)?\n(?P<code>[\s\S]*?)^EOF$)"
                  r"|(?P<sed>sed -i '[^'\n]*' (?:harness|plans|writer|holder|sampler)\.py)"
                  r"|(?P<cp>^cp \.\./poc50/sampler\.py sampler\.py$)", re.M)


def commands(log):
    """(time, text) of each Bash command, once each, in the order they ran."""
    seen, at = set(), None
    for line in open(log):
        entry = json.loads(line)
        at = entry.get("timestamp") or at
        content = (entry.get("message") or {}).get("content")
        for block in content if isinstance(content, list) else []:
            if isinstance(block, dict) and block.get("type") == "tool_use" and block["name"] == "Bash" and block["id"] not in seen:
                seen.add(block["id"])
                yield at, block["input"].get("command", "")


def main():
    log, sampler, out = sys.argv[1], sys.argv[2], Path(sys.argv[3])
    work = out / "work"
    (out / "scripts").mkdir(parents=True)
    work.mkdir()
    (work / "README.md").write_text("")
    last, edits = {}, []
    for at, command in commands(log):
        for s in STEP.finditer(command):
            if s["cat"] and s["file"] in NAMES + ["analyse.py"]:
                with open(work / s["file"], "a" if s["add"] else "w") as f:
                    f.write(s["body"])
            elif s["cp"]:
                shutil.copy(sampler, work / "sampler.py")
            elif s["py"] and re.search(r"open\('(harness|plans|writer|holder|sampler|analyse)\.py'\)", s["code"]) and ",'w')" in s["code"]:
                subprocess.run([sys.executable, "-"], input=s["code"], text=True, cwd=work, check=True)
            elif s["sed"]:
                subprocess.run(["bash", "-c", s["sed"]], cwd=work, check=True)
        for name in NAMES:
            if (work / name).exists():
                data = (work / name).read_bytes()
                sha = hashlib.sha256(data).hexdigest()
                if last.get(name) != sha:
                    last[name] = sha
                    edits.append(dict(at=at, file=name, sha256=sha))
                    with gzip.GzipFile(out / "scripts" / f"{name}.{sha[:12]}.gz", "wb", mtime=0) as f:
                        f.write(data)
    print(json.dumps(edits, indent=0))


if __name__ == "__main__":
    main()
