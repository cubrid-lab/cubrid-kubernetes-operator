# ADR-0009: Rolling update vs engine upgrade

## Status

Proposed (placeholder — decision pending, tracked in issue #9)

## Context

"Update" and "upgrade" are different operations with different risk
profiles. Conflating them produces unsafe rolling behavior and unclear
documentation promises.

## Constraints

- The operator must not rely only on StatefulSet rolling-update
  semantics; restarts follow a database-aware sequence
- CUBRID engine version migration is out of MVP scope

## Options / Scopes

### Rolling update (MVP scope)

```text
compatible container image changes
operator-managed restart
configuration-compatible changes
OS/package layer update
```

### Engine upgrade (excluded from initial MVP)

```text
CUBRID engine version migration
volume format migration
cross-version HA compatibility
major/minor database upgrade
```

## Open Questions

- Which StatefulSet update strategy is used for rolling updates?
- What is the safe restart sequence (order across roles, health gates
  between steps)?
- When is an image change "compatible" vs an engine upgrade?

## Decision

Pending.

Terminology already adopted project-wide: **"database-aware rolling
updates"** for the MVP capability; "upgrade" is reserved for engine
version migration.

## Consequences

### Positive

- Clear user-facing expectations
- No implicit promise of engine version migration

### Negative

- Engine upgrades require a future, separately designed mechanism

## Validation

- Rolling update E2E with compatible image change
- Restart sequence validated under HA (no unnecessary failovers)

## Revisit When

- Engine upgrade support is scheduled
- CUBRID supports online version transitions
