# Third-party material

This inventory identifies reuse boundaries and records the provenance check
made for issue #19. It is an engineering inventory, not legal advice. Preserve
existing notices.

| Material | Location or reference | Handling |
|---|---|---|
| Kubebuilder scaffold and generated outputs | `PROJECT`, scaffold markers in `cmd/`, `api/`, `internal/controller/`, `test/`; `Makefile`, `config/`, `AGENTS.md` | Existing Apache-2.0 Go headers are preserved. Confirm template provenance and any upstream notices before release. |
| CUBRID runtime base image | `build/instance-manager/Dockerfile`, `cubrid/cubrid:11.4` | Base-image/runtime terms remain separate. Record the distributed digest and review image redistribution requirements. |
| Go modules and tooling | `go.mod`, `go.sum`, Makefile tool pins | Dependencies keep their own licenses. Inventory the actual release dependency set and retain required notices. |
| Official Operator reference | `https://github.com/CUBRID/cubrid-operator`, README and design references | A reference is not proof of copied code. Identify any actual copied/adapted material and preserve its notices. |
| Upstream test scenarios | Issue #78 | Record origin, revision, license and modifications before importing test code or datasets. |

## Provenance check (2026-10-04, commit `77b1fcc`)

**Source tree.** No copied or adapted third-party source was found:

- Every tracked Go file carries the Apache-2.0 header from
  `hack/boilerplate.go.txt` with `Copyright 2026.`; no other copyright line
  exists in the tree, and no file names an upstream it was copied from.
- The Kubebuilder scaffold is Apache-2.0 and its repository has no `NOTICE`
  file, so it adds no attribution text beyond the headers already kept.
- The official Operator (`CUBRID/cubrid-operator`, Apache-2.0) is referenced
  in documents only. No code or test from it is in this repository.
- `Dockerfile`, `Makefile`, `build/instance-manager/Dockerfile`,
  `build/instance-manager/entrypoint.sh` and `.devcontainer/post-install.sh`
  have no license header. They are project-authored or scaffold files under
  the repository license; a header is not required.

The repository therefore has no `NOTICE` file. Add one when material with its
own notice is copied in.

**Compiled dependencies.** The two binaries (`./cmd/...`, linux/amd64 and
linux/arm64, same set) link 102 Go modules. Classified by the license file in
each module: 63 Apache-2.0, 22 BSD, 16 MIT, 1 ISC. No copyleft license was
found. The classification reads only the first license file of each module
and was not reviewed by hand.

These linked modules ship a `NOTICE` file, which Apache-2.0 section 4(d)
requires to accompany a redistributed binary:

- `github.com/go-openapi/jsonpointer@v1.0.0`
- `github.com/go-openapi/jsonreference@v1.0.0`
- `github.com/minio/minio-go/v7@v7.3.0`
- `github.com/prometheus/client_golang@v1.24.1`
- `github.com/prometheus/client_model@v0.6.2`
- `github.com/prometheus/common@v0.70.1`
- `github.com/prometheus/procfs@v0.21.1`
- `go.yaml.in/yaml/v2@v2.4.4`
- `go.yaml.in/yaml/v3@v3.0.5`
- `google.golang.org/grpc@v1.83.2`
- `sigs.k8s.io/randfill@v1.0.0`

To repeat the list for the current `go.mod`:

```bash
GOOS=linux go list -deps -f '{{with .Module}}{{.Path}}@{{.Version}}{{end}}' ./cmd/... | sort -u
```

## Still required before publishing images

Source publication needs nothing more. Published container images do:

- Include the license and `NOTICE` texts of the linked modules in the manager
  and Instance Manager images, generated from the dependency set of the
  release commit (#123).
- Record the digest of the `cubrid/cubrid` base image and review its
  redistribution terms. The CUBRID binaries and the CUBRID name keep their
  own terms; this repository's license does not cover them.

For new reuse, provide the origin URL and revision, the license, the copied
paths and the modifications in the PR.
