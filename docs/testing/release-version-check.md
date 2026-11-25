# Refusing to overwrite a release version

`hack/release-version-check.sh` decides, before any release step writes
anything, whether a release version may be drafted for the frozen candidate
commit, whether a rerun may continue the draft an earlier run left behind, or
whether the run must stop. It only reads the GitHub API. It never creates,
edits or deletes a tag or a release. The [release dry run](#release-dry-run)
calls it with a read-only token. The [publishing workflow](#publishing-a-release)
runs it with its write token before it writes anything, again before it
drafts the release and again before it publishes the draft.

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
2. checks out the trusted workflow commit first; confirms both that
   workflow revision **and** the requested source SHA are ancestors of
   `refs/remotes/origin/main`, then (and only then) checks out the source
   commit. No unverified requested source is checked out or executed;
3. runs this check, taken from the workflow's own commit, and continues only
   on `state=create`; a draft that a publishing rerun would resume also stops
   it;
4. builds the manager image and the Instance Manager image for linux/amd64
   from the same Dockerfiles as `make docker-build` and
   `make docker-build-instance-manager`, into OCI archives on the runner,
   with no push, no registry login, no build cache and no build record
   artifact, labelled with the commit time;
5. builds the installer image check (`hack/check-installer-image`) from the
   workflow's own commit, which the version check's sparse checkout also
   clones with its `go.mod` and `go.sum`, so an older candidate without the
   checker is still checked;
6. builds the installer manifest with `make build-installer` for the
   manager image and the Instance Manager image the release would publish
   (`IMG` and `IM_IMG`), and stops if that changed a tracked file (the
   commit's generated files are stale);
7. runs the installer image check on `install.yaml` and writes
   `release-dry-run.json`: the commit, version, each image's digest, image
   ID and archive checksum, the installer's SHA-256, and which Instance
   Manager image the installer makes the manager use, with
   `usesBuiltInstanceManagerImage`;
8. uploads `install.yaml`, `release-dry-run.json` and `SHA256SUMS` as the
   run's artifact;
9. only then fails the run if the installer does not make the manager use
   the Instance Manager image built in step 4;
10. rehearses the image publication of `hack/release-push-images.sh` with
    this run's archives and record, on a `registry:3.0.0` container (pinned
    by digest) bound to the runner's loopback address, so nothing reaches
    GHCR: an attempt that stops after pushing the manager image, its rerun
    (which must push only the missing Instance Manager image), a further
    rerun (which must push nothing), and a version whose manager tag already
    holds other bytes (which must be refused before anything is pushed);
11. when `publish.yml` calls it with `keep-images`, uploads the two image
    archives as a second artifact, kept for 7 days, so the publish job
    pushes exactly the images built and checked here.

The installer image check reads the one Deployment of kind `apps/v1`
`Deployment` whose name ends in `controller-manager` and whose
`control-plane` label is `controller-manager`, and its one container named
`manager`. That container must run the built manager image, and its `args`
must set `--instance-manager-image` exactly once, to the built Instance
Manager image, in any form Go's `flag` package parses (`-flag=v`,
`--flag=v`, `-flag v`, `--flag v`). It fails on the flag after a `--`
(where the manager stops parsing), on the flag in `command`, on a flag with
no value, and on a reference that appears only in another resource,
annotation or container. Each manifest document must be a single object: a
`kind: List` is not unpacked, so a Deployment inside one is not found and
the check fails. It exits 3 when the manager sets no
`--instance-manager-image`; the record then names the manager's built-in
default image. Any other failure is recorded as "not verified" with the
checker's reason.

`make build-installer` appends exactly one
`--instance-manager-image=<IM_IMG>` to the manager's `args` (`config/`
itself sets none). Without `IM_IMG` it derives the image from `IMG`'s
registry and tag: `IMG=<registry>/manager:<tag>` gives
`<registry>/instance-manager:<tag>`; an `IMG` without a tag or pinned by
digest stops it with a request to set `IM_IMG`. The dry run passes `IM_IMG`
explicitly, so for a candidate commit that contains this Makefile step 9
is expected to pass.

A candidate commit older than that Makefile renders an installer without
the flag: the manager would fall back to the mutable built-in default
`ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:dev`, not
the image built and recorded here, so step 9 fails and the record and the
job summary say so. The first dry run on GitHub
([44947969205](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/44947969205),
commit `b94f1524`, 2026-11-21 UTC) failed this way.

`make deploy`, which the E2E suite and the Kind checks run with `IMG`
only, sets `--instance-manager-image` only when `IM_IMG` is given on the
command line or in the environment; otherwise the deployed manager keeps
its built-in default, as before.

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
  release or publication on GitHub. The draft and publication steps are
  tested only against a stand-in API (see below); the image steps against
  the registry inside the runner.

## Publishing a release

Three workflows take part, and only the last one publishes.

- `.github/workflows/release-please.yml` (*Release PR*) runs after every
  merge to `main`. release-please reads `release-please-config.json`
  (`release-type: go`) and `.release-please-manifest.json` and opens or
  updates one release PR that sets the next version in the manifest and
  adds its `CHANGELOG.md` section. The action's own `release-type` input is
  left out: with it, the action would build its configuration from its
  inputs and ignore the repository's files. `skip-github-release: true`
  keeps it from creating a tag or a GitHub Release, so no merge publishes
  anything, the merge of the release PR included. It writes with an
  installation token of the repository's GitHub App, so that CI runs on the
  release PR; `GITHUB_TOKEN` gets no permission.
- `.github/workflows/release-dry-run.yml` builds and checks the artifacts
  without publishing, as [above](#release-dry-run).
- `.github/workflows/publish.yml` (*Publish Release*) publishes. A
  maintainer starts it from `main` after the release PR has merged:

  ```sh
  gh workflow run publish.yml --ref main -f sha=<merge commit of the release PR> -f version=v0.1.0
  ```

`publish.yml` has no push, pull request or schedule trigger. Its jobs run in
this order, and a failure stops every later step:

1. **`release-commit`** (read-only) refuses a malformed input; refuses to
   continue unless the `production` environment has at least one required
   reviewer (GitHub would otherwise create the environment unprotected);
   checks that the workflow revision and the commit are on `main`; and
   checks that the commit is the release PR's merge for this version:
   `.release-please-manifest.json` at the commit sets the version and at its
   first parent does not. A later ordinary commit, which still names the same
   version, is refused. It takes the version's `CHANGELOG.md` section as the
   release notes.
2. **`verify`** calls the release dry run with the same commit and version
   and `keep-images`, with a read-only token. It refuses a version with a tag
   or a published release, builds both images and `install.yaml`, requires
   the installer to use the Instance Manager image of the same build, and
   rehearses the image push on the runner's own registry.
3. **`publish-production`** runs in the GitHub Environment `production` and
   starts only after its required reviewers approve. They confirm the
   publication gates of issue #94 first (the
   [release candidate review](release-candidate-review.md)): the required
   scenario evidence, the license acceptance and the disclosure of the API
   group risk. It is the only job that can write (`contents`, `packages` and
   `pull-requests`), and it runs only the workflow's own copies of the
   scripts. In order it:
   1. checks the downloaded files against the verified `SHA256SUMS` and
      requires the record to be this version and commit;
   2. runs the version check with its write token, which also sees drafts:
      a tag, a published release, a draft for another commit or several
      drafts stop the run;
   3. pushes the verified image archives with `hack/release-push-images.sh`
      (below);
   4. drafts the release with `hack/release-publish.sh draft`: the files are
      `install.yaml` and its `SHA256SUMS`, and the notes end with the two
      image digests;
   5. publishes it with `hack/release-publish.sh publish`, only after the
      draft is checked again; publishing makes GitHub create the version's
      tag at the commit, which is then required;
   6. moves the release PR's label from `autorelease: pending` to
      `autorelease: tagged`, so that release-please opens the next release
      PR.

### Failures and reruns

| What happened | What a rerun of the failed job does |
| --- | --- |
| A failure before the images were pushed | Starts over; nothing was written |
| Some images were pushed | Pushes only the missing ones; a tag that already holds the verified digest is left alone |
| An image tag holds other bytes | Refused before anything is pushed; a maintainer decides |
| The draft is missing a file, or the answer to its creation was lost | Resumes the one draft for this commit, replaces its notes and files, and checks it again; never creates a second draft |
| A draft file differs from the verified one | Not published; the rerun replaces the draft's files |
| The answer to the publication was lost | The step decides from what GitHub then shows: a published release whose tag points at the commit |
| The version is already published, or its tag exists | Refused before anything is written; nothing is overwritten |
| Only the label step failed | Refused, because the version is now published; a maintainer moves the label by hand |

Rerun the **failed job** (*Re-run failed jobs*), not the whole run: the
rerun uses the artifacts the same run already verified, which are kept for
7 days. A new run rebuilds the images. The base images are mutable tags, so
the rebuild can have other digests; tags that an earlier run pushed then
hold other bytes, and the new run is refused. Until the draft is published,
nothing is announced: a draft is visible only to people who can push to the
repository. Image tags that a failed run pushed are public on GHCR, but no
release refers to them.

`hack/release-push-images.sh` checks every archive against the recorded
checksum and asks the registry for every tag before the first push: a tag
the registry answers `MANIFEST_UNKNOWN` or `NAME_UNKNOWN` for is pushed with
`skopeo copy --preserve-digests`; a tag that holds the recorded digest is
not pushed again; a tag that holds another digest, or any answer it does not
recognize after retries, stops the run before anything is pushed. After the
pushes every tag must hold its recorded digest. Credentials reach curl and
skopeo in files only the runner user can read, and go only to a token
endpoint on the registry's own host.

`hack/release-publish.sh` creates the draft with one request that it never
retries; when the answer is lost, the next attempt finds that draft and
resumes it. A draft is checked by its state (still a draft, of this tag,
targeting the commit), by its exact set of files, and by downloading every
file and comparing its SHA-256 with `SHA256SUMS`.

### What an admin provisions

- **The GitHub App** that the maintainer chose on 2026-11-13, installed on
  this repository only, with *Contents: read and write* and *Pull requests:
  read and write*. Its Client ID goes in the repository secret
  `RELEASE_APP_CLIENT_ID` and its private key in `RELEASE_APP_PRIVATE_KEY`.
  Until both exist, *Release PR* warns and updates no release PR. No token
  is stored in the repository.
- **The `production` environment** with required reviewers and a deployment
  branch rule that allows `main` only. `publish.yml` refuses to run while it
  has no required reviewer.

### How the publishing path is tested

`go test ./test/release/` (part of `make test`) runs
`hack/release-publish.sh` against a stand-in GitHub API that keeps releases,
files and tags: a first draft and publication; a failed upload, after which
publication is refused and the rerun resumes the same draft; a lost answer to
the draft's creation and to the publication; a published release, a tag
without a release and a draft for another commit, each refused without a
write; a draft file changed after the check; and files that do not match
`SHA256SUMS`. It runs `hack/release-push-images.sh` against a stand-in
registry and a stand-in skopeo: a push that fails after the first image and
its reruns; a taken tag, a changed archive and a registry error, each
refused before any push; and the registry token exchange. It also reads the
workflows: `publish.yml` has only the manual trigger, only
`publish-production` can write and runs in `production`, and the writing
steps come in the order above; `release-please.yml` leaves the action's
`release-type` input out, skips the GitHub Release and gives `GITHUB_TOKEN`
no permission, and the configuration keeps `release-type: go`.

Not shown by these tests or by the dry run: the real GitHub API's answers to
the draft, upload and publication requests; that publishing a draft creates
the tag at its target commit on this repository; a push to GHCR; the App
token; and release-please opening the release PR. The first real
publication is the first observation of these.
