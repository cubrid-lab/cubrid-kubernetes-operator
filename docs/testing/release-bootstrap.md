# First-release bootstrap inputs and offline SDK check

The release configuration proposes `0.1.0` for the first release. The versions
manifest uses `{".": "0.0.0"}` to represent no previous root-package release.
These are inputs for release-please; neither file creates a release PR, tag,
GitHub Release or published image.

The distinction matters: release-please treats a nonzero manifest entry as a
previous version even if it finds no matching release. With a feature commit,
seeding the manifest with `0.1.0` therefore proposes `0.2.0`. With the zero
sentinel but no explicit initial version, the Go strategy defaults to `1.0.0`.
The pair in this repository instead proposes `0.1.0`.

## Reproduce with the actual pinned SDK

Run from the repository root using Node.js 18 or later. Install the already
chosen release-please runtime into a temporary directory, outside the repository:

```sh
sdk_prefix="$(mktemp -d)"
npm install --prefix "$sdk_prefix" --ignore-scripts --no-audit --no-fund \
  --save-exact release-please@17.3.0
node docs/testing/release-bootstrap-simulation.cjs \
  "$sdk_prefix/node_modules/release-please"
```

The install needs registry access; the simulation uses only local files and
synthetic GitHub reads. Its optional argument identifies an installed SDK
directory; without it, normal Node module resolution, including `NODE_PATH`,
must resolve release-please. The script rejects any SDK version other than
`17.3.0`. It uses the exported `Manifest.fromManifest` and
`Manifest.buildPullRequests` APIs, rather than reimplementing version selection.
It blocks HTTP/TCP/fetch entry points before loading the SDK and rejects every
GitHub API method outside its read-only fixture.

The upstream npm artifact has this integrity value:

```text
sha512-dB7HsFUpAvU1Wj9RGCINUz8Zi2qQDZiGbDEEVnJ7baNfJmk5XdYhuUFL6RcuDGjReGhp1gzY8Tc2g7vHlM4zpg==
```

Retain the temporary install's `package-lock.json` and its SHA-256 with a run
record when comparing environments. Pinning the SDK version alone does not
freeze all transitive dependency versions. No repository dependency or lockfile
is needed for this manual check. Issue #124 owns the reproducer; review its
retention when the bootstrap configuration or pinned runtime changes.

## Assertions and evidence level

Every case supplies one synthetic `feat` commit and no releases or tags:

| Inputs | Required SDK result |
| --- | --- |
| Checked-in config and manifest | First version `0.1.0` |
| Same zero manifest, with `initial-version` removed | Default first version `1.0.0` |
| Config with `initial-version: 0.1.0`, but manifest seed `0.1.0` | Next version `0.2.0` |

The script checks that the draft, pre-1.0 bump, tag-prefix and ordinary
changelog-section settings retain their intended values. It also checks that
the SDK leaves its input objects unchanged and attempts neither a network
request nor an unexpected GitHub API call. A failed assertion exits nonzero.
Proposed update paths are printed, not written.

This proves bootstrap selection on those fixtures. It does not run the remote
CLI, analyze this repository's full commit history, open a release PR or prove
that GitHub schedules its CI. It does not qualify breaking-change changelog
rendering, draft/tag discovery, retries, duplicate-version refusal, artifact
integrity, candidate freeze, database scenarios or publication. Those require
their own evidence under [issue #124](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/124)
and the [release candidate procedure](release-candidate-review.md).

## Pinned upstream sources

- [Manifest version handling in release-please 17.3.0](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/manifest.ts#L702)
  treats a nonzero manifest value as a previous release.
- [Initial version selection](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/strategies/base.ts#L539)
  uses `initial-version` only without a previous release; the
  [default](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/strategies/base.ts#L740)
  is `1.0.0`.
- [The pinned configuration schema](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/schemas/config.json)
  defines `initial-version` as a string; the
  [manifest schema](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/schemas/manifest.json)
  defines package-path version entries.

## Remote observation on 2026-11-16 UTC

This separate experiment used real GitHub reads, not the synthetic fixture
above. It froze `ci/first-release-bootstrap` at
[`140041edee7ecfada70991b3f091f044b447e10a`](https://github.com/cubrid-lab/cubrid-kubernetes-operator/tree/140041edee7ecfada70991b3f091f044b447e10a),
the reviewed head of [PR #916](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/916).
Both successful previews verified that exact remote head before and after
reading. They did not preview the later merge commit or a moving `main`.

The installed SDK was `17.3.0`, with the npm integrity recorded above. Its public
[`GitHub.create({fetch})`](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/github.ts#L254)
accepted a temporary read-only transport; the calls were
`Manifest.fromManifest`, `buildPullRequests` and `GitHub.buildChangeSet`.
The transport used `gh api` for authentication without reading or forwarding
credentials. It allowed only target-repository REST GETs and the SDK's named
GraphQL read queries, rejected mutations before dispatch, preserved HTTP status, headers
and bodies, and never called a PR, tag, release or label creation API.

| Attempt | Transport reads | Commit bound | Outcome |
| --- | ---: | ---: | --- |
| Initial owner/name URL guard | 25 | 500 | Safely blocked before dispatching a numeric pagination URL; no completed preview |
| Verified same-repository alias | 63 | 500 | Preview selected 500 commits; history coverage was incomplete |
| Expanded public depth option | 71 | 543 | Preview selected all 542 reachable commits, verified by SHA sets |

GitHub's pagination link used `/repositories/1370637128/`. Before allowing
that exact GET prefix, a repository metadata read verified that ID belonged
to `cubrid-lab/cubrid-kubernetes-operator`. No other repository was authorized.
The first blocked receipt was retained; correcting the transport boundary was
not a retry of a failed HTTP response. All 159 dispatched transport reads
across these attempts returned HTTP 200; separate source/identity preflight
reads are not included in that count. No publication occurred.

The expanded run used the supported
[`Manifest.fromManifest` options override](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/manifest.ts#L433),
`commitSearchDepth: 543`; the release-search bound remained 400. A read-only
facade recorded each public `GitHub.mergeCommitIterator` yield and forwarded
it unchanged. Its 542 SHAs were unique and exactly matched `git rev-list` for
the frozen head in a non-shallow clone: zero missing, extra or duplicate
SHAs. This verifies commit selection, not that every commit appears in the
changelog; configured changelog filtering still applies.

Both previews proposed version `0.1.0`, the title
`chore(ci/first-release-bootstrap): release 0.1.0`, and updates to
`CHANGELOG.md` and `.release-please-manifest.json`. Only the proposed changelog
hash changed when the additional history was included:

| SHA-256 input | Digest |
| --- | --- |
| Source `release-please-config.json` at the frozen head | `1fb69cf9232b3b1ca669e9e789ed87c63251782a335c4fa60f106753befab75d` |
| Source manifest at the frozen head | `b9c66e0126774ab9fa34536f15972438356e7035724a2c0f0ca3b125967e6399` |
| Reachable SHA set and independently recorded SDK selection, sorted with one SHA per LF-terminated line | `17ff5864db3a89e0fb206c4c9c76d8412082723e37329b16069fbd705106055f` |
| Proposed changelog, 500-commit bound | `c3581b22105931b0440d7b7a9df79e86f5773d1a8e961db3d368ffada7449579` |
| Proposed changelog, all 542 commits | `793b090929d47cccc7ec1ac6560a4c52b296283ffc9627dd9ab0c806266cbdd1` |
| Proposed manifest, identical in both previews | `5f2fad2ced4b17aba50a3070f44ce85ae1447e4aa6c16a787a9efcd5a744014b` |

To reproduce the source-history count and digest with Git and Python's standard
library, obtain the immutable head locally (or fetch `refs/pull/916/head` and
verify its SHA), then run:

```sh
git rev-list 140041edee7ecfada70991b3f091f044b447e10a |
  python3 -c 'import hashlib,sys; s=sys.stdin.read().splitlines(); assert len(s)==len(set(s))==542; print("count",len(s),"unique",len(set(s))); print(hashlib.sha256(("\n".join(sorted(s))+"\n").encode("ascii")).hexdigest())'
```

Require a non-shallow clone; `git rev-parse --is-shallow-repository` must print
`false`. The expected counts are 542/542 and the digest is the SHA-set value
above. This command reproduces the Git side, not the remote SDK observation.
The raw SDK SHA lists, sanitized request traces and comparison receipts were
retained locally as `remote-preview-depth543.json` and
`remote-preview-depth-comparison.json`, alongside the separate blocked and
500-commit receipts. They are local investigation records, not durable release
archives; their retention and the complete transport are not supplied by this
document.

Future consumers must freeze their own source and compare actual selected SHAs
against its reachable history; hardcoding 543 does not establish coverage after
more commits. This observation created no release PR, branch, tag, release or
published image. It does not qualify PR CI authentication, changelog policy for
every commit shape, draft/tag discovery, retries, duplicate-version refusal,
artifact verification, candidate freeze, database scenarios or publication.
