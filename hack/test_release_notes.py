"""Offline checks for proposed operator release notes."""

import unittest
from check_release_notes import issues

GOOD = """## v0.1.0 (2026-12-01)

Adds a `RoutingReady` condition; see Known limitations for write-path behavior that has not been validated.

### Upgrade notes

- Review the CRD API group, supported Kubernetes versions and broker permissions before installation.

### Features

- Expose a `RoutingReady` condition ([#123](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/123)) by `@example-author`.

### Known limitations

- Write quarantine has unit, envtest and one Kind run only; behavior under VM failure or network partition is not validated.
"""


class ReleaseNotesTests(unittest.TestCase):
    def test_valid_draft(self):
        self.assertEqual(issues(GOOD, "v0.1.0"), [])

    def test_missing_summary(self):
        body = GOOD.replace(
            "Adds a `RoutingReady` condition; see Known limitations for write-path behavior that has not been validated.", ""
        )
        self.assertIn("missing observable user-impact summary", issues(body, "v0.1.0"))

    def test_wrong_version(self):
        self.assertTrue(any("version heading" in e for e in issues(GOOD, "v0.2.0")))

    def test_duplicate_change(self):
        entry = "- Review the CRD API group, supported Kubernetes versions and broker permissions before installation."
        self.assertTrue(any("duplicate change" in e for e in issues(
            GOOD.replace(entry, entry + "\n" + entry), "v0.1.0"
        )))

    def test_raw_generated(self):
        self.assertTrue(any("raw generated bullet" in e for e in issues(
            GOOD.replace("- Expose a `RoutingReady`", "* Expose a `RoutingReady`"), "v0.1.0"
        )))

    def test_indented_raw_bullet(self):
        self.assertTrue(any("raw generated bullet" in e for e in issues(
            GOOD.replace("- Expose a `RoutingReady`", "  * Expose a `RoutingReady`"), "v0.1.0"
        )))

    def test_known_limitations_allowed(self):
        self.assertFalse(any("unknown section" in e for e in issues(GOOD, "v0.1.0")))

    def test_known_issues_allowed(self):
        body = GOOD.replace("### Known limitations", "### Known issues")
        self.assertEqual(issues(body, "v0.1.0"), [])


if __name__ == "__main__":
    unittest.main()
