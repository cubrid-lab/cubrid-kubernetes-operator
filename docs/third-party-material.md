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
| CUBRID JDBC driver | `test/workload/client/Dockerfile`, `org.cubrid:cubrid-jdbc:11.3.2.0053` from Maven Central | BSD-3-Clause according to its published POM. Downloaded when the test-only workload client image is built, pinned by version and SHA-256; not stored in this repository and not part of any release image. |
| Upstream test scenarios | [docs/testing/upstream-scenarios.md](testing/upstream-scenarios.md) | The sources, revisions and licenses are recorded there; all are used as ideas only and nothing is imported. Record origin, revision, license and modifications before importing test code or datasets. |

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

## Image components (2026-10-26, commit `d6eab59`)

What each image the project builds contains besides this repository's code,
read from the images themselves (#91). Licenses are as the components state
them; nothing here was reviewed by counsel.

| Image | Base | Components that are not this repository's code |
|---|---|---|
| Manager (`Dockerfile`) | `gcr.io/distroless/static:nonroot`, `sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99` on the date above | The Go modules linked into `./cmd/...` (below). The base's Debian packages `base-files`, `ca-certificates`, `media-types`, `netbase`, `tzdata` and `tzdata-legacy`, each with its `copyright` file under `/usr/share/doc`. |
| Instance Manager (`build/instance-manager/Dockerfile`) | `cubrid/cubrid:11.4`, `sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791` on the date above | The same Go modules. CUBRID 11.4.6, whose `COPYING` reads: "CUBRID Server Engine is under Apache License 2.0 and CUBRID APIs and Connectors are under BSD License", and "CUBRID is registered trademark of CUBRID Corporation". CUBRID's `3rdparty` directory carries license files for Adoptium Temurin (and its assembly exception), libedit, libexpat, libjansson, libodbc, libopenssl, libtbb, lz4, rapidjson and re2. Rocky Linux 8.10 with 224 RPM packages, whose declared licenses include GPLv2+ (65), GPLv3+ (30), LGPLv2+ (56), BSD (66), MIT (36) and MPLv2.0 (9), among them `bash`, `glibc` and `openssl-libs`. |
| Workload client, test-only (`test/workload/client/Dockerfile`) | `eclipse-temurin:17-jre` | The CUBRID JDBC driver 11.3.2.0053 (BSD-3-Clause, see the table above). The Temurin JRE is GPLv2 with the Classpath Exception. The image is pushed as a development image (#554) and is never part of a release. |

**Go modules, recounted.** At `d6eab59`, `./cmd/...` links 102 modules besides
this one. Classified by the first license file of each, the counts are
unchanged since the check above: 63 Apache-2.0, 22 BSD, 16 MIT and 1 ISC, with
no copyleft. The same 11 ship a `NOTICE` file, some at newer versions:
`minio-go/v7@v7.4.1`, `prometheus/client_golang@v1.25.0`,
`prometheus/client_model@v0.6.3`, `prometheus/common@v0.72.0` and
`prometheus/procfs@v0.22.0`.

**Against the terms #19 set.** The project's Apache-2.0 license covers this
repository's source only.
- The images also redistribute their bases' operating-system packages, under
  those packages' own terms.
- The Instance Manager image redistributes GPL and LGPL packages from Rocky
  Linux. Distributing it obliges the distributor to make those packages'
  corresponding source available, as the GPL requires. Rocky Linux publishes
  its source RPMs, but this project has not yet arranged or documented how it
  meets that obligation.
- The development images already pushed to the public GHCR packages (#449)
  carry the same bases.
- The CUBRID name is a trademark of CUBRID Corporation. The images use it
  descriptively.
- Neither image includes the `NOTICE` texts of the linked Go modules yet.

## Still required before publishing images

Source publication needs nothing more. Published container images do:

- Include the license and `NOTICE` texts of the linked modules in the manager
  and Instance Manager images, generated from the dependency set of the
  release commit (#123).
- Record the digest of the `cubrid/cubrid` base image of the release and
  review its redistribution terms. The CUBRID binaries and the CUBRID name
  keep their own terms; this repository's license does not cover them. The
  components found on 2026-10-26 are listed above.
- Decide how the GPL and LGPL operating-system packages of the images' bases
  have their corresponding source made available (for example by pointing to
  the base distribution's source of the recorded digests), and document it
  with the release (#123).

For new reuse, provide the origin URL and revision, the license, the copied
paths and the modifications in the PR.
