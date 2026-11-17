"""Offline checks for proposed operator release notes."""

import unittest
from check_release_notes import issues

GOOD = """## v0.1.0 (2026-12-01)

This release brings verified HA reconciliation with explicitly scoped write quarantine behavior.

### Upgrade notes

- Review the CRD API group, supported Kubernetes versions and broker permissions before installation.

### Features

- Expose readiness conditions and refuse writes while there is no verified master ([#123](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/123)) by @reviewer.
"""


class ReleaseNotesTests(unittest.TestCase):
    def test_valid_draft(self):
        self.assertEqual(issues(GOOD, "v0.1.0"), [])

    def test_missing_summary(self):
        body = GOOD.replace(
            "This release brings verified HA reconciliation with explicitly scoped write quarantine behavior.", ""
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
            GOOD.replace("- Expose readiness", "* Expose readiness"), "v0.1.0"
        )))


if __name__ == "__main__":
    unittest.main()
