# Release notes review checklist and format check

**Pre-release policy only:** this repository has no published GitHub Releases. These checks do not create v0.1.0, tags, packages, charts, images, or publication metadata.

## Review checklist

- Lead with an observable, concise summary. Put **Upgrade notes** ahead of features: tested Kubernetes/CUBRID versions, CRD API group/schema migration, RBAC, storage and broker permissions, installation and rollback expectations.
- Ground master election, replication freshness, RoutingReady and read/write quarantine guarantees in actual reconciliation/e2e/soak results. Label unverified recovery behaviors as limitations. Never promise zero downtime without evidence.
- Describe security defaults and actionable user settings. Never imply that draft work is already shipped.
- Use nonempty standard headings (Upgrade notes, Compatibility, Features, Bug Fixes, Security, Documentation, CI, Tests, Known limitations) and cite implementing PRs/commits with verified human credits. Consolidate duplicated generated changes.
- The final published notes must match the reviewed release candidate and include a single accurate Full Changelog comparison or initial commit-history link. Record source SHA, version, Kubernetes matrix and distributed artifact identity separately from editorial review.
- Take feature status only from `ROADMAP.md`, and Kubernetes/CUBRID versions only from `docs/compatibility.md` (write "supported range unverified" where that document does). Take candidate evidence from `docs/testing/release-candidate-review.md`.
- Name the evidence level of every claim: unit/envtest, Kind with the real database, or VM lab. Kind or envtest never supports a VM-failure or network-partition claim.
- List every open critical or high release blocker under **Known limitations** (or **Known issues**).
- Write notes by hand in `docs/releases/vX.Y.Z.md`, starting from the release-please draft. The checker's `## vX.Y.Z (YYYY-MM-DD)` heading format differs from release-please output.
- The checker verifies format only. Factual claims need human review.

## Offline checks

Run `python3 -m unittest discover -s hack -p test_release_notes.py -v`, then `python3 hack/check_release_notes.py docs/releases/vX.Y.Z.md` when a real candidate exists. Current CI is strictly read-only and does not activate release-please. Actual release gates and permissions require separate review; refs #939.
