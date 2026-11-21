# Refusing to overwrite a release version

`hack/release-version-check.sh` decides, before any release step writes
anything, whether a release version may be drafted for the frozen candidate
commit, whether a rerun may continue the draft an earlier run left behind, or
whether the run must stop. It only reads the GitHub API. It never creates,
edits or deletes a tag or a release. The [release dry run](#release-dry-run)
calls it with a read-only token; the publishing release workflow that will
run it before drafting and again before publishing is still to be written
under
[issue #124](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/124).

## What it decides

The release configuration (`release-please-config.json`) creates the GitHub
Release as a draft. GitHub creates the version's tag only when the draft is
published. release-please 17.3.0 sets the draft's target to the release commit
SHA ([`createRelease`](https://github.com/googleapis/release-please/blob/891bcf6253b390e39df9ff3e1c059a836bd39c98/src/github.ts#L1423)).
For a version tag such as `v0.1.0` and the candidate's full commit SHA:

| What GitHub shows for the version | Result |
| --- | --- |
| No tag and no release | `state=create`: draft it |
| No tag, one draft whose target is the candidate SHA | `state=resume`: a rerun after a failure continues that draft and never creates a second one |
| A published release | Refused: a published version is never overwritten |
| The tag exists, at any commit | Refused: the version was published or tagged by hand; a maintainer decides |
| A draft for another commit or a branch name | Refused: the version belongs to another candidate |
| More than one release with the tag name | Refused: a maintainer removes the extra drafts |
| A failed request, a server error, a refused token, a non-GitHub 404, a found tag without a full commit SHA, or an answer it does not recognize | Retried, then refused; never read as absence |

It reads every page of the release list, so an older release on a later page
is not missed. A page is accepted only if it is a JSON array whose every entry
is an object with a string tag name and a boolean draft flag; a truncated page
or an entry such as `null` or a bare string makes the answer unknown. A list
longer than 100 pages is not read to its end and is unknown too. It exits 0
for create and resume (and writes `state=` to `GITHUB_OUTPUT` when set), 1 for
a refusal or an unknown answer, and 2 for invalid input: a tag that is not
`vMAJOR.MINOR.PATCH`, a commit that is not a full SHA, `ATTEMPTS` that is not
a whole number of at least 1, or `RETRY_DELAY` that is not a number. Invalid
input is not echoed, and values quoted from the API in a refusal are escaped,
so a line break cannot start a workflow command of its own. The token reaches
curl through a file only the runner user can read, not on its command line.

GitHub lists drafts only to a token that can push to the repository. A token
without that right sees no draft, so the script would report `create` while a
draft exists. The release step must run it with the same token that writes
the release (`contents: write`).

## How it is tested

`go test ./test/release/` (part of `make test`) runs the script against a
stand-in GitHub API in the test process. It covers each row of the table,
a draft and a published release found only on the second page, a failure on
the second page, transient errors followed by either answer, malformed and
truncated pages, an endless list, invalid input and values that carry a
workflow command. Refusals must come from the first lookup, not after retries.
The stand-in fails the test on any request other than a GET with the
expected token, and the test checks that the token is neither printed nor on
a curl command line.

## What this does not show

The answers of the real GitHub API are not observed here: the table follows
GitHub's documented responses and the pinned release-please source. In
particular, whether a draft stays invisible to a token without push rights,
and the exact body of a 404 for a missing tag on this repository, are
documented behavior, not a recorded run. So is the draft-then-tag order itself:
that GitHub creates the version's tag only when the draft is published, and
not when the draft is created, has not been observed on this repository. The
script does not check the
release artifacts, the candidate freeze, the required scenario evidence, the
license and API group gates, or that release-please opens its release PR with
the GitHub App token so that CI runs on it. Those stay with issue #124, the
[release candidate procedure](release-candidate-review.md) and the
publication gates of #94.

## Release dry run

`.github/workflows/release-dry-run.yml` builds the artifacts of a proposed
release without publishing anything. A maintainer starts it from `main`:

```sh
gh workflow run release-dry-run.yml --ref main -f sha=<full commit SHA on main> -f version=v0.1.0
```

It runs only on that manual dispatch and its token can only read the
repository. In order, it:

1. refuses a commit that is not a full 40-character SHA or a version that is
   not `vMAJOR.MINOR.PATCH`, before anything is cloned;
2. clones that commit and refuses it unless it is an ancestor of
   `refs/remotes/origin/main`; no code from the commit runs before this;
3. runs this check, taken from the workflow's own commit, and continues only
   on `state=create`; a draft that a publishing rerun would resume also stops
   it;
4. builds the manager image and the Instance Manager image for linux/amd64
   from the same Dockerfiles as `make docker-build` and
   `make docker-build-instance-manager`, into OCI archives on the runner,
   with no push, no registry login, no build cache and no build record
   artifact, labelled with the commit time;
5. builds the installer manifest with `make build-installer` for the
   manager image the release would publish, and stops if that changed a
   tracked file (the commit's generated files are stale);
6. uploads `install.yaml`, `release-dry-run.json` (the commit, version, each
   image's digest, image ID and archive checksum, the installer's SHA-256
   and which Instance Manager image the installer makes the manager use)
   and `SHA256SUMS` as the run's artifact;
7. fails, after that upload, unless `install.yaml` names the Instance
   Manager image built in the same run.

Step 7 fails every run today: `make build-installer` sets only the manager
image, so an installed release would run the manager's built-in default
Instance Manager image, the mutable `instance-manager:dev` tag, not the
image built and recorded here. The record and the job summary name the image
that would be used. Until the installer sets the Instance Manager image, a
dry run shows this gap instead of a candidate.

Any failed step fails the job, and the job summary shows how far it got.
It writes nothing outside the run (the Go setup's module cache is turned
off too), so a rerun starts from the same state.

Its limits:

- **Drafts are invisible to it.** GitHub lists draft releases only to a
  token that can push, and this job's token cannot. It therefore refuses an
  existing tag or published release, but not a draft: with this token the
  check's draft refusals (a draft for this or another commit, several
  drafts) will in practice not trigger. The publishing workflow must run the
  same check with its write token.
- **The digests identify this build only.** The base images are mutable
  tags, so a later build of the same commit can produce other digests. The
  image archives are not kept.
- The installer manifest names the manager image by tag, not by digest.
- It does not exercise release-please, the GitHub App token, a draft
  release, publication, or a failure or rerun on the publishing path. Those
  stay with issue #124.
