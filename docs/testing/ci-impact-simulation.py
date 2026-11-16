#!/usr/bin/env python3
"""Exercise the actual CI classifier with local, synthetic GitHub metadata.

Owned by issue #610. Keep this manually invoked reproducer while the classifier
is a release gate; review its retention when that classifier is replaced.
No token, GitHub request, Actions runner or Kubernetes cluster is used.
"""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


CHECKS = {"go_lint", "go_tests", "go_e2e", "govulncheck", "codeql_go", "codeql_actions"}


def cases():
    result = []

    def add(name, pages=None, expected=None, event="pull_request", number="42",
            count=0, api_error=False, raw=None):
        result.append((name, pages, CHECKS if expected is None else expected,
                       event, number, count, api_error, raw))

    for event in ("push", "schedule", "workflow_dispatch"):
        add(event, event=event)
    add("unknown PR number", number="missing")
    add("changed-file API error", api_error=True)
    add("empty file list", pages=[[]])
    add("incomplete pages", pages=[[{"filename": "README.md"}]], count=2)
    add("invalid JSON", raw="{bad", count=1)
    add("malformed page", pages=[None], count=1)
    add("missing filename", pages=[[{}]], count=1)
    add("rename missing previous path", pages=[[
        {"filename": "README.md", "status": "renamed"}]], count=1)
    for name, previous in (("source renamed into docs", "internal/controller/broker.go"),
                           ("scenario contract renamed", "docs/testing/scenario-contract.md")):
        add(name, pages=[[{"filename": "docs/archive.md", "previous_filename": previous,
                           "status": "renamed"}]], count=1)
    add("scenario contract changed", pages=[[
        {"filename": "docs/testing/scenario-contract.md"}]], count=1)
    add("multiple pages include source", pages=[
        [{"filename": "README.md"}], [{"filename": "cmd/main.go"}]], count=2)
    add("ordinary docs", pages=[[{"filename": "README.md"},
                                 {"filename": "docs/guide.svg"}]], expected=set(), count=2)
    add("issue form", pages=[[{"filename": ".github/ISSUE_TEMPLATE/bug.yml"}]],
        expected=set(), count=1)
    add("unknown workflow", pages=[[
        {"filename": ".github/workflows/unrecognized.yml"}]], count=1)
    workflows = {
        "lint.yml": {"go_lint", "codeql_actions"},
        "test.yml": {"go_tests", "codeql_actions"},
        "test-e2e.yml": {"go_e2e", "codeql_actions"},
        "security.yml": {"govulncheck", "codeql_actions"},
        "codeql.yml": {"codeql_go", "codeql_actions"},
        "pr-title.yml": {"codeql_actions"},
        "ci-impact.yml": CHECKS,
    }
    for name, expected in workflows.items():
        add(name, pages=[[{"filename": ".github/workflows/" + name}]],
            expected=expected, count=1)
    return result


def main():
    repo = Path(__file__).resolve().parents[2]
    workflow = repo / ".github/workflows/ci-impact.yml"
    source = workflow.read_text(encoding="utf-8")
    body = source.split("        run: |\n", 1)[1]
    script = "\n".join(line[10:] if line.startswith("          ") else line
                       for line in body.splitlines()) + "\n"
    results = []
    with tempfile.TemporaryDirectory(prefix="cko-ci-impact-") as temporary:
        directory = Path(temporary)
        (directory / "classifier.sh").write_text(script, encoding="utf-8")
        fake_gh = directory / "gh"
        fake_gh.write_text(
            '#!/bin/sh\necho called >> "$GH_CALLS"\n'
            'if [ "$SIMULATE_API_ERROR" = 1 ]; then\n'
            '  echo "SIMULATED API failure" >&2; exit 1\nfi\n'
            'cat "$FAKE_PAGES"\n', encoding="utf-8")
        fake_gh.chmod(0o755)
        for name, pages, expected, event, number, count, api_error, raw in cases():
            (directory / "pages.json").write_text(
                json.dumps(pages) if raw is None else raw, encoding="utf-8")
            for name_of_file in ("outputs", "calls"):
                (directory / name_of_file).write_text("", encoding="utf-8")
            environment = {
                "PATH": str(directory) + os.pathsep + os.environ["PATH"],
                "EVENT_NAME": event, "PR_NUMBER": number,
                "PR_CHANGED_FILES": str(count), "RUNNER_TEMP": str(directory),
                "GITHUB_REPOSITORY": "cubrid-lab/cubrid-kubernetes-operator",
                "GITHUB_OUTPUT": str(directory / "outputs"),
                "GH_CALLS": str(directory / "calls"),
                "SIMULATE_API_ERROR": str(int(api_error)),
                "FAKE_PAGES": str(directory / "pages.json"),
            }
            run = subprocess.run(["bash", str(directory / "classifier.sh")],
                                 env=environment, text=True, capture_output=True,
                                 timeout=10, check=False)
            output = dict(line.split("=", 1) for line in
                          (directory / "outputs").read_text(encoding="utf-8").splitlines())
            selected = {key for key, value in output.items() if value == "true"}
            called = bool((directory / "calls").read_text(encoding="utf-8"))
            expected_output = {key: "true" if key in expected else "false" for key in CHECKS}
            if run.returncode != 0 or output != expected_output:
                raise RuntimeError(f"{name}: exit={run.returncode}, outputs={output}, "
                                   f"expected={sorted(expected)}\n{run.stdout}\n{run.stderr}")
            if (event != "pull_request" or number == "missing") and called:
                raise RuntimeError(f"{name}: metadata was requested unnecessarily")
            results.append({"case": name, "exit": run.returncode,
                            "selected": sorted(selected), "gh_called": called})
    print(json.dumps({
        "workflow_sha256": hashlib.sha256(workflow.read_bytes()).hexdigest(),
        "level": "actual classifier shell with fake metadata; no Actions scheduler or Kind run",
        "cases_passed": len(results), "cases": results,
    }, indent=2))


if __name__ == "__main__":
    main()
