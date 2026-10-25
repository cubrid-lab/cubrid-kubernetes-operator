#!/usr/bin/env bash
# Refuses to publish development images (#449) over a tag that exists, and
# refuses as well when it cannot tell (#462). Run by the "Refuse to overwrite an
# existing tag" step of .github/workflows/dev-images.yml before any push, and by
# test/devimages against stand-in servers.
#
# For each image in IMAGES it asks the registry for the manifest of TAG:
#   200                              the tag exists: refuse.
#   404 MANIFEST_UNKNOWN/NAME_UNKNOWN the tag is absent: publish.
#   token denied                     for an image named in BOOTSTRAP_IMAGES
#                                    only (a first publication): absent if the
#                                    GitHub packages API answers 404 for it.
#   anything else                    unknown: retried, then refused.
# A failed request, a timeout, a server error or an answer it does not
# recognize is never read as absence.
#
# Environment:
#   REPOSITORY      image path prefix, e.g. cubrid-lab/cubrid-kubernetes-operator
#   IMAGES          space-separated image names under REPOSITORY
#   TAG             tag to check
#   REGISTRY_USER   user for the registry token request
#   REGISTRY_TOKEN  password for the registry token request and bearer token
#                   for the packages API (the workflow's GITHUB_TOKEN)
#   REGISTRY_URL    default https://ghcr.io
#   BOOTSTRAP_IMAGES images whose package may not exist yet; set it in the
#                   change that adds an image and remove it once published.
#                   For any other image a refused token is unknown.
#   API_URL         default https://api.github.com
#   ATTEMPTS        default 3; RETRY_DELAY default 5 (seconds)
set -euo pipefail

: "${REPOSITORY:?}" "${IMAGES:?}" "${TAG:?}" "${REGISTRY_USER:?}" "${REGISTRY_TOKEN:?}"
REGISTRY_URL=${REGISTRY_URL:-https://ghcr.io}
API_URL=${API_URL:-https://api.github.com}
ATTEMPTS=${ATTEMPTS:-3}
RETRY_DELAY=${RETRY_DELAY:-5}
org=${REPOSITORY%%/*}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# request prints the HTTP status of a GET and leaves the body in $work/body.
# It prints 000 when no answer came (connection failure or timeout).
request() {
  local code
  : > "$work/body"
  code=$(curl -sS -o "$work/body" -w '%{http_code}' --connect-timeout 10 --max-time 30 "$@" 2> "$work/err") ||
    code=000
  printf '%s' "$code"
}

detail() {
  printf '%s %s' "$(head -c 300 "$work/body" 2> /dev/null | tr -d '\n')" "$(head -c 300 "$work/err" | tr -d '\n')"
}

# package_absent succeeds when the packages API answers 404 for the image's
# package. A token that cannot read a package cannot write it either, so a 404
# also rules out an overwrite by this run.
package_absent() {
  local name code
  name=$(printf '%s/%s' "${REPOSITORY#*/}" "$1" | sed 's|/|%2F|g')
  code=$(request -H "Authorization: Bearer $REGISTRY_TOKEN" -H 'Accept: application/vnd.github+json' \
    "$API_URL/orgs/$org/packages/container/$name")
  case $code in
    404) return 0 ;;
    *)
      echo "packages API answered $code for $1: $(detail)" >&2
      return 1
      ;;
  esac
}

# check prints exists, absent or unknown for one image, with the reason for
# unknown on stderr.
check() {
  local img=$1 code token
  code=$(request -u "$REGISTRY_USER:$REGISTRY_TOKEN" \
    "$REGISTRY_URL/token?scope=repository:$REPOSITORY/$img:pull")
  case $code in
    200)
      token=$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$work/body")
      if [ -z "$token" ]; then
        echo "token answer for $img has no token" >&2
        echo unknown
        return
      fi
      ;;
    401 | 403)
      local refusal
      refusal=$(detail)
      if [[ " ${BOOTSTRAP_IMAGES:-} " == *" $img "* ]] && grep -q '"DENIED"' "$work/body" &&
        package_absent "$img"; then
        echo absent
      else
        echo "registry refused a token for $img ($code) and the package was not shown absent: $refusal" >&2
        echo unknown
      fi
      return
      ;;
    *)
      echo "token request for $img answered $code: $(detail)" >&2
      echo unknown
      return
      ;;
  esac
  code=$(request -I -H "Authorization: Bearer $token" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "$REGISTRY_URL/v2/$REPOSITORY/$img/manifests/$TAG")
  case $code in
    200) echo exists ;;
    404)
      # A HEAD answer has no body: ask again with GET for the error code.
      code=$(request -H "Authorization: Bearer $token" \
        -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
        "$REGISTRY_URL/v2/$REPOSITORY/$img/manifests/$TAG")
      if [ "$code" = 404 ] && grep -Eq '"(MANIFEST_UNKNOWN|NAME_UNKNOWN)"' "$work/body"; then
        echo absent
      elif [ "$code" = 200 ]; then
        echo exists
      else
        echo "manifest of $img:$TAG answered 404 without a recognized error code: $(detail)" >&2
        echo unknown
      fi
      ;;
    *)
      echo "manifest of $img:$TAG answered $code: $(detail)" >&2
      echo unknown
      ;;
  esac
}

for img in $IMAGES; do
  attempt=1
  while :; do
    verdict=$(check "$img")
    case $verdict in
      exists)
        echo "::error::$REPOSITORY/$img:$TAG already exists; development image tags are never overwritten"
        exit 1
        ;;
      absent)
        echo "$REPOSITORY/$img:$TAG is absent"
        break
        ;;
    esac
    if [ "$attempt" -ge "$ATTEMPTS" ]; then
      echo "::error::Could not tell whether $REPOSITORY/$img:$TAG exists after $ATTEMPTS attempts; nothing is pushed"
      exit 1
    fi
    attempt=$((attempt + 1))
    sleep "$RETRY_DELAY"
  done
done
