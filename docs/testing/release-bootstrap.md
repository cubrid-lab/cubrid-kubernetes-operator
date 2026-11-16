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
