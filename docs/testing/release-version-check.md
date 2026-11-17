# Refusing to overwrite a release version

`hack/release-version-check.sh` decides, before any release step writes
anything, whether a release version may be drafted for the frozen candidate
commit, whether a rerun may continue the draft an earlier run left behind, or
whether the run must stop. It only reads the GitHub API. It never creates,
edits or deletes a tag or a release, and no workflow calls it yet: the
release workflow that will run it before drafting and again before publishing
is still to be written under
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
| A failed request, a server error, a refused token, a non-GitHub 404 or an answer it does not recognize | Retried, then refused; never read as absence |

It reads every page of the release list, so an older release on a later page
is not missed. It exits 0 for create and resume (and writes `state=` to
`GITHUB_OUTPUT` when set), 1 for a refusal or an unknown answer, and 2 for an
invalid tag or a commit that is not a full SHA.

GitHub lists drafts only to a token that can push to the repository. A token
without that right sees no draft, so the script would report `create` while a
draft exists. The release step must run it with the same token that writes
the release (`contents: write`).

## How it is tested

`go test ./test/release/` (part of `make test`) runs the script against a
stand-in GitHub API in the test process. It covers each row of the table,
a draft and a published release found only on the second page, a failure on
the second page, transient errors followed by either answer, and invalid
input. The stand-in fails the test on any request other than a GET with the
expected token, and the test checks that the token is never printed.

## What this does not show

The answers of the real GitHub API are not observed here: the table follows
GitHub's documented responses and the pinned release-please source. In
particular, whether a draft stays invisible to a token without push rights,
and the exact body of a 404 for a missing tag on this repository, are
documented behavior, not a recorded run. The script does not check the
release artifacts, the candidate freeze, the required scenario evidence, the
license and API group gates, or that release-please opens its release PR with
the GitHub App token so that CI runs on it. Those stay with issue #124, the
[release candidate procedure](release-candidate-review.md) and the
publication gates of #94.
