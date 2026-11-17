# First operator release notes

**Pre-release policy only:** this repository has no published GitHub Releases. These checks do not create v0.1.0, tags, packages, charts, images, or publication metadata.

## Review checklist

- Lead with an observable, concise summary. Put **Upgrade notes** ahead of features: tested Kubernetes/CUBRID versions, CRD API group/schema migration, RBAC, storage and broker permissions, installation and rollback expectations.
- Ground master election, replication freshness, RoutingReady and read/write quarantine guarantees in actual reconciliation/e2e/soak results. Label unverified recovery behaviors as limitations. Never promise zero downtime without evidence.
- Describe security defaults and actionable user settings. Never imply that draft work is already shipped.
- Use nonempty standard headings (Upgrade notes, Compatibility, Features, Bug Fixes, Security, Documentation, CI, Tests) and cite implementing PRs/commits with verified human credits. Consolidate duplicated generated changes.
- The final published notes must match the reviewed release candidate and include a single accurate Full Changelog comparison or initial commit-history link. Record source SHA, version, Kubernetes matrix and distributed artifact identity separately from editorial review.

## Offline checks

Run `python3 -m unittest discover -s hack -p test_release_notes.py -v`, then `python3 hack/check_release_notes.py docs/releases/vX.Y.Z.md` when a real candidate exists. Current CI is strictly read-only and does not activate release-please. Actual release gates and permissions require separate review; refs #939.
