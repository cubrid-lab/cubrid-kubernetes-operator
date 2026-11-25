#!/usr/bin/env bash
# Drafts the GitHub Release of TAG for the frozen candidate EXPECTED_SHA with
# the verified files, and publishes it only once the draft holds exactly those
# files (#124). Run by the publish-production job of
# .github/workflows/publish.yml; tested by test/release against a stand-in
# GitHub API. Usage: release-publish.sh draft|publish
#
#   draft    hack/release-version-check.sh must answer create or resume.
#            create: one POST makes the draft. It is never retried: when its
#            answer is lost, at most one draft exists, and the next attempt
#            finds and resumes it.
#            resume: the one draft of TAG for EXPECTED_SHA an earlier attempt
#            left. Its notes are replaced.
#            Then every file already on the draft is deleted, every file in
#            ASSET_DIR is uploaded, and the draft is checked as below.
#   publish  the version check must answer resume: the draft, no tag and no
#            published release. The draft is checked again; only then is it
#            marked published, which makes GitHub create the tag at
#            EXPECTED_SHA. The tag must then point at EXPECTED_SHA and the
#            release must be published.
# The draft check: the release is still a draft of TAG targeting
# EXPECTED_SHA, holds exactly the files of ASSET_DIR, and each downloads to
# the bytes ASSET_DIR/SHA256SUMS lists.
# Any failure stops before publication. A draft is visible only to people who
# can push to the repository, so a failed attempt leaves nothing public, and a
# rerun resumes the same draft rather than making a second one. A published
# version is refused by the version check and is never edited here.
#
# Environment:
#   REPOSITORY    owner/name
#   TAG           vMAJOR.MINOR.PATCH with an optional suffix
#   EXPECTED_SHA  full commit SHA of the release commit
#   GITHUB_TOKEN  token with contents: write (drafts are listed only to it)
#   ASSET_DIR     the files to attach, SHA256SUMS among them
#   NOTES_FILE    release notes (draft only)
#   API_URL       default https://api.github.com
#   UPLOAD_URL    default https://uploads.github.com
#   ATTEMPTS, RETRY_DELAY  passed to the version check
# Exit codes: 0 done, 1 refused or failed, 2 invalid input.
set -euo pipefail

mode=${1:-}
case $mode in
  draft | publish) ;;
  *)
    echo "::error::Usage: release-publish.sh draft|publish"
    exit 2
    ;;
esac
: "${REPOSITORY:?}" "${TAG:?}" "${EXPECTED_SHA:?}" "${GITHUB_TOKEN:?}" "${ASSET_DIR:?}"
API_URL=${API_URL:-https://api.github.com}
UPLOAD_URL=${UPLOAD_URL:-https://uploads.github.com}
here=$(cd "$(dirname "$0")" && pwd)

if ! [[ $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::TAG is not a release tag (vMAJOR.MINOR.PATCH)"
  exit 2
fi
if ! [[ $EXPECTED_SHA =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::EXPECTED_SHA is not a full commit SHA"
  exit 2
fi
if ! [[ $REPOSITORY =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "::error::REPOSITORY is not owner/name"
  exit 2
fi
if [ "$mode" = draft ] && [ ! -f "${NOTES_FILE:-}" ]; then
  echo "::error::NOTES_FILE is not a file"
  exit 2
fi
for tool in jq curl sha256sum; do
  command -v "$tool" > /dev/null || {
    echo "::error::$tool is required"
    exit 2
  }
done

# The files to attach: every regular file in ASSET_DIR, each listed in
# SHA256SUMS with its checksum, and SHA256SUMS itself.
if [ ! -f "$ASSET_DIR/SHA256SUMS" ]; then
  echo "::error::ASSET_DIR has no SHA256SUMS"
  exit 2
fi
assets=()
for f in "$ASSET_DIR"/*; do
  name=${f##*/}
  if [ ! -f "$f" ] || ! [[ $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    echo "::error::ASSET_DIR holds an entry that is not a plainly named file"
    exit 2
  fi
  assets+=("$name")
  if [ "$name" != SHA256SUMS ] && ! grep -Eq "^[0-9a-f]{64}  $name\$" "$ASSET_DIR/SHA256SUMS"; then
    echo "::error::$name is not listed in SHA256SUMS"
    exit 1
  fi
done
if ! (cd "$ASSET_DIR" && sha256sum --check --strict --quiet SHA256SUMS); then
  echo "::error::The files to attach do not match SHA256SUMS"
  exit 1
fi
if [ "$(wc -l < "$ASSET_DIR/SHA256SUMS")" -ne $((${#assets[@]} - 1)) ]; then
  echo "::error::SHA256SUMS lists a file that is not in ASSET_DIR"
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(
  umask 077
  printf 'Authorization: Bearer %s\n' "$GITHUB_TOKEN" > "$work/auth"
)

# call prints the HTTP status of a request and leaves the body in $work/body.
# It prints 000 when no answer came. A file download sets accept to
# application/octet-stream; curl would send a second Accept header otherwise.
accept=application/vnd.github+json
call() {
  local method=$1 url=$2 code
  shift 2
  : > "$work/body"
  code=$(curl -sS -X "$method" -o "$work/body" -w '%{http_code}' --connect-timeout 10 --max-time 300 \
    -H "@$work/auth" -H "Accept: $accept" -H 'X-GitHub-Api-Version: 2022-11-28' \
    "$@" "$url" 2> "$work/err") || code=000
  printf '%s' "$code"
}

# fail writes to stderr, so a failure inside a command substitution is still
# shown. Values quoted from the API are escaped as workflow command data.
fail() {
  local s=$1
  s=${s//%/%25}
  s=${s//$'\r'/%0D}
  s=${s//$'\n'/%0A}
  echo "::error::$s" >&2
  exit 1
}

detail() {
  printf '%s %s' "$(head -c 300 "$work/body" 2> /dev/null | tr -d '\r\n')" "$(head -c 300 "$work/err" | tr -d '\r\n')"
}

version_check() {
  local out
  out=$(mktemp -p "$work")
  if ! GITHUB_OUTPUT=$out bash "$here/release-version-check.sh" >&2; then
    exit 1
  fi
  sed -n 's/^state=//p' "$out"
}

# find_draft prints the id of the one draft of TAG that targets EXPECTED_SHA.
find_draft() {
  local page=1 code count
  : > "$work/drafts"
  while [ "$page" -le 100 ]; do
    code=$(call GET "$API_URL/repos/$REPOSITORY/releases?per_page=100&page=$page")
    if [ "$code" != 200 ] || ! count=$(jq -e 'if type == "array" then length else error end' "$work/body" 2> /dev/null); then
      fail "Could not list the releases (page $page answered $code): $(detail)"
    fi
    jq -c --arg tag "$TAG" --arg sha "$EXPECTED_SHA" \
      '.[] | select(.tag_name == $tag and .draft == true and .target_commitish == $sha) | .id' \
      "$work/body" >> "$work/drafts"
    [ "$count" -lt 100 ] && break
    page=$((page + 1))
  done
  if [ "$(wc -l < "$work/drafts")" -ne 1 ] || ! [[ $(cat "$work/drafts") =~ ^[0-9]+$ ]]; then
    fail "Expected exactly one draft of $TAG for $EXPECTED_SHA"
  fi
  cat "$work/drafts"
}

# list_assets leaves the draft's assets, as {id, name, state}, in $work/assets.
list_assets() {
  local id=$1 code
  code=$(call GET "$API_URL/repos/$REPOSITORY/releases/$id/assets?per_page=100")
  if [ "$code" != 200 ] || ! jq -e 'type == "array" and length < 100 and
    all(.[]; (.id | type) == "number" and (.name | type) == "string")' "$work/body" > /dev/null 2>&1; then
    fail "Could not list the files of the draft of $TAG (answered $code): $(detail)"
  fi
  jq -c '.[] | {id, name, state}' "$work/body" > "$work/assets"
}

# check_draft requires release id to be a draft of TAG targeting EXPECTED_SHA
# whose files are exactly ASSET_DIR's, each with the listed bytes.
check_draft() {
  local id=$1 code want have name aid
  code=$(call GET "$API_URL/repos/$REPOSITORY/releases/$id")
  if [ "$code" != 200 ] || ! jq -e --arg tag "$TAG" --arg sha "$EXPECTED_SHA" \
    '.draft == true and .tag_name == $tag and .target_commitish == $sha' "$work/body" > /dev/null 2>&1; then
    fail "Release $id is no longer a draft of $TAG for $EXPECTED_SHA (answered $code): $(detail)"
  fi
  list_assets "$id"
  want=$(printf '%s\n' "${assets[@]}" | sort)
  have=$(jq -r 'select(.state == "uploaded") | .name' "$work/assets" | sort)
  if [ "$want" != "$have" ] || [ "$(wc -l < "$work/assets")" -ne "${#assets[@]}" ]; then
    fail "The draft of $TAG holds [${have//$'\n'/ }], not the verified files [${want//$'\n'/ }]"
  fi
  for name in "${assets[@]}"; do
    aid=$(jq -r --arg n "$name" 'select(.name == $n) | .id' "$work/assets")
    # GitHub answers with a redirect to storage; curl does not send the
    # Authorization header to another host.
    accept=application/octet-stream
    code=$(call GET "$API_URL/repos/$REPOSITORY/releases/assets/$aid" -L)
    accept=application/vnd.github+json
    if [ "$code" != 200 ]; then
      fail "Could not download $name from the draft of $TAG (answered $code)"
    fi
    if [ "$(sha256sum "$work/body" | cut -d' ' -f1)" != "$(sha256sum "$ASSET_DIR/$name" | cut -d' ' -f1)" ]; then
      fail "$name on the draft of $TAG is not the verified file"
    fi
  done
  echo "The draft of $TAG for $EXPECTED_SHA holds exactly the verified files: ${assets[*]}"
}

if [ "$mode" = draft ]; then
  state=$(version_check)
  payload=$(jq -n --arg tag "$TAG" --arg sha "$EXPECTED_SHA" --rawfile body "$NOTES_FILE" \
    '{tag_name: $tag, target_commitish: $sha, name: $tag, body: $body, draft: true}')
  case $state in
    create)
      code=$(call POST "$API_URL/repos/$REPOSITORY/releases" --data-binary "$payload")
      if [ "$code" != 201 ] || ! id=$(jq -er 'select(.draft == true) | .id | numbers' "$work/body" 2> /dev/null); then
        fail "Could not create the draft of $TAG (answered $code): $(detail). A rerun resumes a draft this request may have made"
      fi
      echo "Created the draft release $id of $TAG for $EXPECTED_SHA"
      ;;
    resume)
      id=$(find_draft)
      code=$(call PATCH "$API_URL/repos/$REPOSITORY/releases/$id" \
        --data-binary "$(jq -n --arg tag "$TAG" --rawfile body "$NOTES_FILE" '{name: $tag, body: $body}')")
      if [ "$code" != 200 ] || ! jq -e '.draft == true' "$work/body" > /dev/null 2>&1; then
        fail "Could not update the notes of the draft $id of $TAG (answered $code): $(detail)"
      fi
      echo "Resuming the draft release $id of $TAG for $EXPECTED_SHA"
      ;;
    *) fail "The version check answered no state" ;;
  esac
  # Only a draft's files are replaced; check_draft below refuses anything else.
  code=$(call GET "$API_URL/repos/$REPOSITORY/releases/$id")
  if [ "$code" != 200 ] || ! jq -e '.draft == true' "$work/body" > /dev/null 2>&1; then
    fail "Release $id of $TAG is not a draft; its files are not touched"
  fi
  list_assets "$id"
  while read -r aid; do
    code=$(call DELETE "$API_URL/repos/$REPOSITORY/releases/assets/$aid")
    if [ "$code" != 204 ]; then
      fail "Could not remove an earlier file from the draft of $TAG (answered $code): $(detail)"
    fi
  done < <(jq -r '.id' "$work/assets")
  for name in "${assets[@]}"; do
    code=$(call POST "$UPLOAD_URL/repos/$REPOSITORY/releases/$id/assets?name=$name" \
      -H 'Content-Type: application/octet-stream' --data-binary "@$ASSET_DIR/$name")
    if [ "$code" != 201 ]; then
      fail "Could not attach $name to the draft of $TAG (answered $code): $(detail). Nothing is published; a rerun replaces the draft's files"
    fi
  done
  check_draft "$id"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "release_id=$id" >> "$GITHUB_OUTPUT"
  fi
  exit 0
fi

# publish
state=$(version_check)
if [ "$state" != resume ]; then
  fail "$TAG has no draft for $EXPECTED_SHA; only a checked draft is published"
fi
id=$(find_draft)
check_draft "$id"
code=$(call PATCH "$API_URL/repos/$REPOSITORY/releases/$id" \
  --data-binary "$(jq -n --arg tag "$TAG" --arg sha "$EXPECTED_SHA" '{tag_name: $tag, target_commitish: $sha, draft: false}')")
if [ "$code" != 200 ]; then
  # The answer may be lost after GitHub applied it; what GitHub now shows
  # decides.
  echo "::warning::Publishing the draft $id answered $code; checking what GitHub shows"
fi
code=$(call GET "$API_URL/repos/$REPOSITORY/releases/$id")
if [ "$code" != 200 ] || ! jq -e --arg tag "$TAG" '.draft == false and .tag_name == $tag' "$work/body" > /dev/null 2>&1; then
  fail "Release $id of $TAG is not published (answered $code); nothing else was changed, and a rerun of this job tries again"
fi
code=$(call GET "$API_URL/repos/$REPOSITORY/git/ref/tags/$TAG")
if [ "$code" != 200 ] || ! jq -e --arg ref "refs/tags/$TAG" --arg sha "$EXPECTED_SHA" \
  '.ref == $ref and .object.type == "commit" and .object.sha == $sha' "$work/body" > /dev/null 2>&1; then
  fail "$TAG was published, but its tag does not point at $EXPECTED_SHA (answered $code): $(detail). A maintainer checks the release by hand"
fi
echo "Published $TAG at $EXPECTED_SHA"
