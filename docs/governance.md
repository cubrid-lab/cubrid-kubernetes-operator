# Governance notes

This file tracks project-governance decisions that require organizational or
legal confirmation, so contributors know their current status.

## License (#19)

**Status: pending organizational confirmation.**

The kubebuilder scaffold generated source files with Apache-2.0 headers
("Licensed under the Apache License, Version 2.0"), so the code is currently
authored *as if* Apache-2.0. However, per issue #19 the license must be chosen
against `cubrid-lab` organizational policy and upstream compatibility rather
than defaulted — the official CUBRID Operator being Apache-2.0 does not
automatically make that the right choice here.

Action required (maintainers): confirm the license, then add a top-level
`LICENSE` file and reconcile the source headers. Until then, do not add source
headers that conflict with the existing Apache-2.0 boilerplate.

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
