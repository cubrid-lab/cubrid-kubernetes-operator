#!/usr/bin/env python3
"""Read-only editorial check for a proposed Kubernetes operator Release description."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

HEADER = re.compile(r"^## (v\d+\.\d+\.\d+) \(\d{4}-\d{2}-\d{2}\)$")
SUB = re.compile(r"^### ([^\n]+)$")
ALLOWED = {
    "Upgrade notes", "Compatibility", "Features", "Bug Fixes", "Added", "Changed",
    "Deprecated", "Removed", "Fixed", "Security", "Performance", "Documentation",
    "CI", "Tests",
}


def issues(body: str, expected_tag: str) -> list[str]:
    lines = body.strip().splitlines()
    if not lines or not (match := HEADER.fullmatch(lines[0])):
        return ["missing version/date heading"]
    errors: list[str] = []
    if match.group(1) != expected_tag:
        errors.append("version heading differs from filename")
    sections = [(i, m.group(1)) for i, line in enumerate(lines) if (m := SUB.fullmatch(line))]
    if not sections:
        return errors + ["missing categorized ### changes"]
    intro = " ".join(line.strip() for line in lines[1 : sections[0][0]] if line.strip())
    if not intro:
        errors.append("missing observable user-impact summary")
    elif len(intro) > 900:
        errors.append("summary too long")
    if len({title for _, title in sections}) != len(sections):
        errors.append("repeated section heading")
    seen = set()
    for index, (start, title) in enumerate(sections):
        if title not in ALLOWED:
            errors.append(f"unknown section: {title}")
        end = sections[index + 1][0] if index + 1 < len(sections) else len(lines)
        changes = [s.strip() for s in lines[start + 1 : end] if s.startswith(("- ", "* "))]
        if not changes:
            errors.append(f"empty section: {title}")
        for change in changes:
            if change.startswith("* "):
                errors.append(f"raw generated bullet: {title}")
            key = re.sub(r"\s+", " ", change.casefold())
            if key in seen:
                errors.append(f"duplicate change: {title}")
            seen.add(key)
    if re.search(r"\b(?:TODO|TBD|PLACEHOLDER)\b", body, re.I):
        errors.append("unresolved placeholder")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("files", nargs="+", help="Versioned docs/releases/vX.Y.Z.md")
    args = parser.parse_args()
    total = 0
    for name in args.files:
        path = Path(name)
        problems = issues(path.read_text(encoding="utf-8"), path.stem)
        for problem in problems:
            print(f"ERROR: {path}: {problem}", file=sys.stderr)
            total += 1
    if total == 0:
        print(f"Release-note format: {len(args.files)} file(s) passed (read-only)")
    return 1 if total else 0


if __name__ == "__main__":
    raise SystemExit(main())
