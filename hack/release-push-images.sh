#!/usr/bin/env bash
# Pushes the release images that the same publish run built and verified, from
# their OCI archives, to the registry under the release tag, and never puts
# other bytes under a tag that already exists (#124). Run by the
# publish-production job of .github/workflows/publish.yml against GHCR, by the
# release dry run against a registry inside the runner, and by test/release
# against a stand-in registry.
#
# RECORD (release-dry-run.json of the verified build) gives each image's
# digest and the SHA-256 of its archive ARCHIVE_DIR/<image>.oci.tar. Before
# anything is pushed, every archive must match its recorded checksum and the
# registry is asked for every image's TAG:
#   404 MANIFEST_UNKNOWN or NAME_UNKNOWN  absent: push it.
#   200 with the recorded digest          present: an earlier attempt of this
#                                         run pushed it; it is not pushed again.
#   200 with another digest               refuse: the tag holds other bytes,
#                                         and a maintainer decides.
#   anything else                         unknown: retried, then refused.
# One refusal or unknown answer stops the run before the first push. After the
# pushes every tag must hold its recorded digest. A failed request, a server
# error, a refused token or an answer it does not recognize is never read as
# absence.
#
# Environment:
#   REGISTRY_HOST   e.g. ghcr.io, or localhost:5000 for the dry run's registry
#   REPOSITORY      image path prefix, e.g. cubrid-lab/cubrid-kubernetes-operator
#   IMAGES          space-separated image names, e.g. "manager instance-manager"
#   TAG             release tag, vMAJOR.MINOR.PATCH with an optional suffix
#   RECORD          the verified build's release-dry-run.json
#   ARCHIVE_DIR     directory holding <image>.oci.tar
#   REGISTRY_USER, REGISTRY_TOKEN  credentials; both or neither
#   INSECURE        "true" only for a plain-HTTP registry on the runner itself
#   ATTEMPTS        default 3; RETRY_DELAY default 5 (seconds)
# Exit codes: 0 every tag holds its recorded digest, 1 refused, unknown or a
# failed push, 2 invalid input. Invalid input is not echoed.
set -euo pipefail

: "${REGISTRY_HOST:?}" "${REPOSITORY:?}" "${IMAGES:?}" "${TAG:?}" "${RECORD:?}" "${ARCHIVE_DIR:?}"
REGISTRY_USER=${REGISTRY_USER:-}
REGISTRY_TOKEN=${REGISTRY_TOKEN:-}
INSECURE=${INSECURE:-false}
ATTEMPTS=${ATTEMPTS:-3}
RETRY_DELAY=${RETRY_DELAY:-5}

if ! [[ $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::TAG is not a release tag (vMAJOR.MINOR.PATCH)"
  exit 2
fi
if ! [[ $REGISTRY_HOST =~ ^[a-z0-9.-]+(:[0-9]+)?$ && $REPOSITORY =~ ^[a-z0-9._/-]+$ ]]; then
  echo "::error::REGISTRY_HOST or REPOSITORY is not a registry name"
  exit 2
fi
for image in $IMAGES; do
  if ! [[ $image =~ ^[a-z0-9][a-z0-9._-]*$ ]]; then
    echo "::error::IMAGES holds a name that is not an image name"
    exit 2
  fi
done
if { [ -n "$REGISTRY_USER" ] && [ -z "$REGISTRY_TOKEN" ]; } || { [ -z "$REGISTRY_USER" ] && [ -n "$REGISTRY_TOKEN" ]; }; then
  echo "::error::Set both REGISTRY_USER and REGISTRY_TOKEN, or neither"
  exit 2
fi
case $INSECURE in
  true) scheme=http ;;
  false) scheme=https ;;
  *)
    echo "::error::INSECURE is not true or false"
    exit 2
    ;;
esac
if ! [[ $ATTEMPTS =~ ^[1-9][0-9]{0,2}$ && $RETRY_DELAY =~ ^[0-9]{1,4}(\.[0-9]+)?$ ]]; then
  echo "::error::ATTEMPTS or RETRY_DELAY is not a number"
  exit 2
fi
for tool in jq curl skopeo sha256sum base64; do
  command -v "$tool" > /dev/null || {
    echo "::error::$tool is required"
    exit 2
  }
done

accept='application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json, */*;q=0.1'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Credentials go to curl and skopeo in files only this user can read, never on
# a command line other processes can see. printf is a shell builtin.
(
  umask 077
  if [ -n "$REGISTRY_USER" ]; then
    basic=$(printf '%s:%s' "$REGISTRY_USER" "$REGISTRY_TOKEN" | base64 -w0)
    printf 'Authorization: Basic %s\n' "$basic" > "$work/basic"
    printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$REGISTRY_HOST" "$basic" > "$work/authfile.json"
  fi
)

# Every archive must be the one the verified build recorded.
declare -A expected
for image in $IMAGES; do
  if ! digest=$(jq -er --arg n "$image" \
    '[.images[] | select(.name == $n)] | if length == 1 then .[0].digest else error end' "$RECORD") ||
    ! sum=$(jq -er --arg n "$image" '.images[] | select(.name == $n) | .ociArchiveSha256' "$RECORD") ||
    ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ && $sum =~ ^[0-9a-f]{64}$ ]]; then
    echo "::error::The record has no single digest and archive checksum for $image"
    exit 1
  fi
  archive="$ARCHIVE_DIR/$image.oci.tar"
  if [ ! -f "$archive" ] || [ "$(sha256sum "$archive" | cut -d' ' -f1)" != "$sum" ]; then
    echo "::error::The archive of $image is missing or is not the one the verified build recorded"
    exit 1
  fi
  expected[$image]=$digest
done

# request prints the HTTP status of a GET, with the body in $work/body and the
# headers in $work/headers. It prints 000 when no answer came.
request() {
  local code
  : > "$work/body"
  : > "$work/headers"
  code=$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' \
    --connect-timeout 10 --max-time 30 "$@" 2> "$work/err") || code=000
  printf '%s' "$code"
}

detail() {
  printf '%s %s' "$(head -c 300 "$work/body" 2> /dev/null | tr -d '\r\n')" "$(head -c 300 "$work/err" | tr -d '\r\n')"
}

header() {
  sed -n "s/^$1:[[:space:]]*\\([^[:space:]]*\\)[[:space:]]*\$/\\1/Ip" "$work/headers" | tail -n 1
}

# lookup prints absent, present or refuse:<reason> for an image's TAG, or
# unknown with the reason on stderr.
lookup() {
  local image=$1 url code challenge realm service scope bearer digest
  url="$scheme://$REGISTRY_HOST/v2/$REPOSITORY/$image/manifests/$TAG"
  code=$(request -H "Accept: $accept" "$url")
  if [ "$code" = 401 ]; then
    # A token for pulling this repository, from the realm the registry names.
    challenge=$(sed -n 's/^www-authenticate:[[:space:]]*Bearer[[:space:]]*\(.*\)$/\1/Ip' "$work/headers" | tr -d '\r' | tail -n 1)
    realm=$(printf '%s' "$challenge" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')
    service=$(printf '%s' "$challenge" | sed -n 's/.*service="\([^"]*\)".*/\1/p')
    scope="repository:$REPOSITORY/$image:pull"
    if ! [[ $realm =~ ^https?:// ]]; then
      echo "registry asked for authentication without a token realm: $(detail)" >&2
      echo unknown
      return
    fi
    # The credentials go only to the registry's own host.
    if [ -f "$work/basic" ] && [[ $realm != "$scheme://$REGISTRY_HOST/"* ]]; then
      echo "registry named a token realm on another host: $realm" >&2
      echo unknown
      return
    fi
    if [ -f "$work/basic" ]; then
      code=$(request -H "@$work/basic" -G --data-urlencode "service=$service" --data-urlencode "scope=$scope" "$realm")
    else
      code=$(request -G --data-urlencode "service=$service" --data-urlencode "scope=$scope" "$realm")
    fi
    if [ "$code" != 200 ] || ! bearer=$(jq -er '(.token // .access_token) | strings' "$work/body" 2> /dev/null); then
      echo "token request for $image answered $code: $(detail)" >&2
      echo unknown
      return
    fi
    (
      umask 077
      printf 'Authorization: Bearer %s\n' "$bearer" > "$work/bearer"
    )
    code=$(request -H "@$work/bearer" -H "Accept: $accept" "$url")
  fi
  case $code in
    200)
      digest=$(header docker-content-digest | tr -d '\r')
      if [ -z "$digest" ]; then
        digest="sha256:$(sha256sum "$work/body" | cut -d' ' -f1)"
      fi
      if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
        echo "manifest of $image answered a digest it does not recognize: $(detail)" >&2
        echo unknown
      elif [ "$digest" = "${expected[$image]}" ]; then
        echo present
      else
        echo "refuse:$image:$TAG already holds $digest, not the verified build's ${expected[$image]}; a published image is never overwritten, and a maintainer decides"
      fi
      ;;
    404)
      if jq -e 'any(.errors[]?; .code == "MANIFEST_UNKNOWN" or .code == "NAME_UNKNOWN")' "$work/body" > /dev/null 2>&1; then
        echo absent
      else
        echo "manifest of $image answered 404 without a registry error code: $(detail)" >&2
        echo unknown
      fi
      ;;
    *)
      echo "manifest of $image answered $code: $(detail)" >&2
      echo unknown
      ;;
  esac
}

# state prints the settled answer for an image, retrying unknown ones.
state() {
  local attempt=1 verdict
  while :; do
    verdict=$(lookup "$1") || verdict=unknown
    if [ "$verdict" != unknown ] || [ "$attempt" -ge "$ATTEMPTS" ]; then
      printf '%s' "$verdict"
      return
    fi
    attempt=$((attempt + 1))
    sleep "$RETRY_DELAY"
  done
}

escape() {
  local s=$1
  s=${s//%/%25}
  s=${s//$'\r'/%0D}
  s=${s//$'\n'/%0A}
  printf '%s' "$s"
}

declare -A before
for image in $IMAGES; do
  verdict=$(state "$image")
  case $verdict in
    absent | present) before[$image]=$verdict ;;
    refuse:*)
      echo "::error::$(escape "${verdict#refuse:}")"
      echo "Nothing was pushed."
      exit 1
      ;;
    *)
      echo "::error::Could not tell whether $image:$TAG exists after $ATTEMPTS attempts; nothing was pushed"
      exit 1
      ;;
  esac
done

skopeo_args=(--retry-times 3)
if [ "$INSECURE" = true ]; then
  skopeo_args+=(--dest-tls-verify=false)
fi
if [ -f "$work/authfile.json" ]; then
  skopeo_args+=(--dest-authfile "$work/authfile.json")
fi
for image in $IMAGES; do
  ref="$REGISTRY_HOST/$REPOSITORY/$image:$TAG"
  if [ "${before[$image]}" = present ]; then
    echo "$ref already holds ${expected[$image]}; not pushed again"
    continue
  fi
  # --preserve-digests fails rather than push a manifest whose digest differs
  # from the archive's.
  echo "Pushing $ref (${expected[$image]})"
  if ! skopeo copy "${skopeo_args[@]}" --preserve-digests "oci-archive:$ARCHIVE_DIR/$image.oci.tar" "docker://$ref"; then
    echo "::error::Could not push $ref; a rerun pushes the images that are still missing"
    exit 1
  fi
done

for image in $IMAGES; do
  verdict=$(state "$image")
  if [ "$verdict" != present ]; then
    echo "::error::After the push, $image:$TAG does not answer the verified digest ${expected[$image]}: $(escape "${verdict#refuse:}")"
    exit 1
  fi
  echo "image=$REGISTRY_HOST/$REPOSITORY/$image:$TAG@${expected[$image]}"
done
