#!/usr/bin/env bash
# Decides, before a release step writes anything, whether release version TAG
# may be drafted for commit EXPECTED_SHA, whether a rerun may continue the
# draft an earlier run left behind, or whether the run must stop (#124). It
# only reads the GitHub API; it never creates, edits or deletes a tag or a
# release. Tested by test/release against a stand-in API.
#
# The release configuration (release-please-config.json) drafts a release and
# GitHub creates its tag only when the draft is published. So:
#   no tag, no release of TAG                  create: draft it.
#   no tag, one draft of TAG for EXPECTED_SHA  resume: a rerun after a failure
#                                              continues that draft; it never
#                                              creates a second one.
#   a published release of TAG                 refuse: never overwrite a
#                                              published version.
#   the tag TAG exists                         refuse: a published or
#                                              hand-made version; a maintainer
#                                              decides.
#   a draft of TAG for another commit or       refuse: the version belongs to
#   branch name, or several releases of TAG    another candidate or the state
#                                              is ambiguous.
#   anything it cannot read                    unknown: retried, then refused.
# A failed request, a server error, a refused token or an answer it does not
# recognize is never read as absence.
#
# Drafts are listed only to a token that can push to the repository; a token
# without that right sees no draft and would read a draft as absent. Give the
# step the same token that writes the release (contents: write).
#
# Environment:
#   REPOSITORY    owner/name, e.g. cubrid-lab/cubrid-kubernetes-operator
#   TAG           release tag, vMAJOR.MINOR.PATCH with an optional suffix
#   EXPECTED_SHA  full commit SHA of the frozen release candidate
#   GITHUB_TOKEN  token for the API
#   API_URL       default https://api.github.com
#   ATTEMPTS      default 3; RETRY_DELAY default 5 (seconds)
#   GITHUB_OUTPUT when set, receives state=create or state=resume
# Exit codes: 0 create or resume, 1 refused or unknown, 2 invalid input.
set -euo pipefail

: "${REPOSITORY:?}" "${TAG:?}" "${EXPECTED_SHA:?}" "${GITHUB_TOKEN:?}"
API_URL=${API_URL:-https://api.github.com}
ATTEMPTS=${ATTEMPTS:-3}
RETRY_DELAY=${RETRY_DELAY:-5}

if ! [[ $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::$TAG is not a release tag (vMAJOR.MINOR.PATCH)"
  exit 2
fi
if ! [[ $EXPECTED_SHA =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::$EXPECTED_SHA is not a full commit SHA"
  exit 2
fi
command -v jq > /dev/null || {
  echo "::error::jq is required"
  exit 2
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# request prints the HTTP status of a GET and leaves the body in $work/body.
# It prints 000 when no answer came (connection failure or timeout).
request() {
  local code
  : > "$work/body"
  code=$(curl -sS -o "$work/body" -w '%{http_code}' --connect-timeout 10 --max-time 30 \
    -H "Authorization: Bearer $GITHUB_TOKEN" -H 'Accept: application/vnd.github+json' \
    "$1" 2> "$work/err") || code=000
  printf '%s' "$code"
}

detail() {
  printf '%s %s' "$(head -c 300 "$work/body" 2> /dev/null | tr -d '\n')" "$(head -c 300 "$work/err" | tr -d '\n')"
}

# check prints create, resume, refuse:<reason> or unknown, with the reason for
# unknown on stderr.
check() {
  local code tag_sha="" page=1 count n published target
  code=$(request "$API_URL/repos/$REPOSITORY/git/ref/tags/$TAG")
  case $code in
    200)
      # The single-ref endpoint matches exactly; check it anyway.
      if [ "$(jq -r '.ref? // empty' "$work/body" 2> /dev/null)" != "refs/tags/$TAG" ]; then
        echo "tag lookup answered another or no ref: $(detail)" >&2
        echo unknown
        return
      fi
      tag_sha=$(jq -r '.object.sha // "unknown"' "$work/body")
      ;;
    404)
      if [ "$(jq -r '.message? // empty' "$work/body" 2> /dev/null)" != "Not Found" ]; then
        echo "tag lookup answered 404 without GitHub's not-found message: $(detail)" >&2
        echo unknown
        return
      fi
      ;;
    *)
      echo "tag lookup answered $code: $(detail)" >&2
      echo unknown
      return
      ;;
  esac

  # Every page of releases, drafts included; keep those named TAG.
  : > "$work/matches"
  while :; do
    code=$(request "$API_URL/repos/$REPOSITORY/releases?per_page=100&page=$page")
    if [ "$code" != 200 ] || ! count=$(jq -e 'if type == "array" then length else error end' "$work/body" 2> /dev/null); then
      echo "release list page $page answered $code: $(detail)" >&2
      echo unknown
      return
    fi
    jq -c --arg tag "$TAG" '.[] | select(.tag_name == $tag) | {draft, target_commitish}' "$work/body" >> "$work/matches"
    [ "$count" -lt 100 ] && break
    page=$((page + 1))
  done

  n=$(wc -l < "$work/matches")
  published=$(jq -s 'map(select(.draft != true)) | length' "$work/matches")
  if [ "$published" -gt 0 ]; then
    echo "refuse:$TAG is already published; a published version is never overwritten"
  elif [ -n "$tag_sha" ]; then
    echo "refuse:tag $TAG already exists at $tag_sha; a version with a tag is published or was made by hand, and a maintainer decides what happens to it"
  elif [ "$n" -gt 1 ]; then
    echo "refuse:$n releases are named $TAG; a maintainer removes the extra drafts"
  elif [ "$n" -eq 1 ]; then
    target=$(jq -r '.target_commitish // ""' "$work/matches")
    if [ "$target" = "$EXPECTED_SHA" ]; then
      echo resume
    else
      echo "refuse:the draft of $TAG targets $target, not the candidate $EXPECTED_SHA"
    fi
  else
    echo create
  fi
}

attempt=1
while :; do
  verdict=$(check)
  case $verdict in
    create | resume)
      if [ "$verdict" = create ]; then
        echo "$TAG has no tag and no release; it may be drafted for $EXPECTED_SHA"
      else
        echo "$TAG has a draft for $EXPECTED_SHA and no tag; a rerun continues that draft"
      fi
      echo "state=$verdict"
      if [ -n "${GITHUB_OUTPUT:-}" ]; then
        echo "state=$verdict" >> "$GITHUB_OUTPUT"
      fi
      exit 0
      ;;
    refuse:*)
      echo "::error::${verdict#refuse:}"
      exit 1
      ;;
  esac
  if [ "$attempt" -ge "$ATTEMPTS" ]; then
    echo "::error::Could not tell whether $TAG exists after $ATTEMPTS attempts; nothing is drafted or published"
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep "$RETRY_DELAY"
done
