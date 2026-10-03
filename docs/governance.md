# Governance notes

This document records project ownership, contribution terms and release
responsibilities. Changing implementation and validation status lives in
[ROADMAP.md](../ROADMAP.md).

## Maintainer responsibilities

The two maintainers decide scope and architecture, review contributions and
approve release candidates. A contributor's PR does not automatically grant
write access, merge rights or a release role. Contributions are welcomed
within the agreed issue scope; no response-time guarantee is made.

A behavior change is reviewed by the other maintainer. Release publication
requires reviewed evidence for the candidate; public source availability is
not a claim of production readiness. See [CONTRIBUTING.md](../CONTRIBUTING.md).

## License (#19)

The maintainer confirmed Apache-2.0 for STP IV on 2026-10-02 in issue #19.
The full license is in [LICENSE](../LICENSE). Project-authored source and
documentation use Apache-2.0 unless a file states otherwise.

Preserve upstream copyright, license and attribution notices when reusing
material. Dependencies and container images keep their respective licenses;
this repository's license does not relicense CUBRID binaries or grant CUBRID
trademark rights. Record reused material and any required NOTICE attribution
in [third-party-material.md](./third-party-material.md). The provenance audit
must be completed before public artifact publication; adding LICENSE alone
does not complete that audit.

No CLA or enforced DCO check is introduced here. Contributors must have the
right to submit their work under the repository license.

## API group ownership (#20)

**Status: pending organizational confirmation (before public release).**

The API group is `database.cubrid.io` (domain `cubrid.io`, group `database`).
This was chosen to keep this experimental project distinct from the official
CUBRID Operator's `k8s.cubrid.com` group.

To resolve before any public release, maintainers must confirm:

- the project/organization controls the `cubrid.io` domain,
- the group can be maintained long-term,
- it will not conflict with upstream contribution.

Changing the API group later is a breaking change (it rewrites every type,
CRD, and import path), so this should be settled before `v1alpha1` is
published for external use.

## Contribution process (#21)

See [CONTRIBUTING.md](../CONTRIBUTING.md).

## Compatibility (#22)

See [compatibility.md](./compatibility.md).
