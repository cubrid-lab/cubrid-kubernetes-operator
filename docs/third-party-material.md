# Third-party material

This inventory identifies reuse boundaries. It is not a completed legal or
transitive dependency audit. Preserve existing notices; issue #19 remains
open until provenance and NOTICE requirements have been checked.

| Material | Location or reference | Handling |
|---|---|---|
| Kubebuilder scaffold and generated outputs | `PROJECT`, scaffold markers in `cmd/`, `api/`, `internal/controller/`, `test/`; `Makefile`, `config/`, `AGENTS.md` | Existing Apache-2.0 Go headers are preserved. Confirm template provenance and any upstream notices before release. |
| CUBRID runtime base image | `build/instance-manager/Dockerfile`, `cubrid/cubrid:11.4` | Base-image/runtime terms remain separate. Record the distributed digest and review image redistribution requirements. |
| Go modules and tooling | `go.mod`, `go.sum`, Makefile tool pins | Dependencies keep their own licenses. Inventory the actual release dependency set and retain required notices. |
| Official Operator reference | `https://github.com/CUBRID/cubrid-operator`, README and design references | A reference is not proof of copied code. Identify any actual copied/adapted material and preserve its notices. |
| Upstream test scenarios | Issue #78 | Record origin, revision, license and modifications before importing test code or datasets. |

The tracked Go files were checked for license-header consistency during this
change: existing Apache-2.0 headers were retained, including generated files.
No source copyright owner was inferred or replaced. This check does not
establish the origin of every line or the terms of every dependency.

Before publishing artifacts, maintainers must finish issue #19's reuse audit,
identify any upstream NOTICE content that must be retained, and add NOTICE
where required. Do not interpret the absence of NOTICE here as a finding that
no attribution is required. For new reuse, provide the origin URL/revision,
license, copied paths and modifications in the PR.
