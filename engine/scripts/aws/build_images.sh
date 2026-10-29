#!/usr/bin/env bash
# Build the two images the cloud deployment needs. LOCAL ONLY -- touches no
# AWS API, creates no resource, costs nothing.
#
# Produces:
#   durexec-engine:<version>   from engine/Dockerfile
#   durexec-extsimd:<version>  from engine/Dockerfile.extsimd  (TEST-ONLY)
#
# The version defaults to the short git SHA plus a -dirty suffix when the tree
# has uncommitted changes, which it does at the time of writing: Phases 6, 7
# and 8 are all uncommitted. That matters more than it looks. Phase 6 found
# three nodes running three different builds of the engine while every health
# check passed, and the cause was an image tag that did not change when the
# source did. A tag carrying the SHA and the dirty flag cannot silently mean
# two different things.
set -euo pipefail

cd "$(dirname "$0")/../.."

VERSION="${DUREXEC_VERSION:-}"
if [ -z "$VERSION" ]; then
  sha=$(git rev-parse --short HEAD)
  if [ -n "$(git status --porcelain)" ]; then
    VERSION="${sha}-dirty"
  else
    VERSION="${sha}"
  fi
fi
echo "version: $VERSION"

# The gpg credential-helper lock on this host wedges `docker build` with an
# "error getting credentials" failure that looks like a Docker bug. A throwaway
# DOCKER_CONFIG with an empty config.json sidesteps the helper without
# touching ~/.gnupg or ~/.docker, which is the fix recorded in CLAUDE.md.
if [ -z "${DOCKER_CONFIG:-}" ]; then
  tmpcfg=$(mktemp -d)
  echo '{}' > "$tmpcfg/config.json"
  export DOCKER_CONFIG="$tmpcfg"
  trap 'rm -rf "$tmpcfg"' EXIT
  echo "using a throwaway DOCKER_CONFIG at $tmpcfg"
fi

# linux/amd64 explicitly, because the default t3.medium is x86_64 and a build
# on an arm64 laptop would otherwise produce an image the instance cannot run.
# Switch both this and var.instance_type together, never one alone.
PLATFORM="${DUREXEC_PLATFORM:-linux/amd64}"
echo "platform: $PLATFORM"

echo
echo "=== engine ==="
docker build --platform "$PLATFORM" \
  --build-arg "VERSION=$VERSION" \
  -t "durexec-engine:$VERSION" \
  -f Dockerfile .

echo
echo "=== extsimd (TEST-ONLY) ==="
docker build --platform "$PLATFORM" \
  --build-arg "VERSION=$VERSION" \
  -t "durexec-extsimd:$VERSION" \
  -f Dockerfile.extsimd .

echo
echo "=== built ==="
docker images --format '{{.Repository}}:{{.Tag}}\t{{.Size}}' \
  | grep -E "durexec-(engine|extsimd):$VERSION"

# Proof that the binary in the image is the one intended, rather than a
# cached layer from an older source tree. The engine refuses to start without
# a DATABASE_URL, so --version is the only subcommand that can be checked
# without a database.
echo
echo "=== recorded version inside the engine image ==="
docker run --rm --platform "$PLATFORM" "durexec-engine:$VERSION" --version 2>&1 | head -3 \
  || echo "NOTE: the engine has no --version flag; tag provenance rests on the build above."

echo
echo "$VERSION" > .durexec-version
echo "wrote engine/.durexec-version = $VERSION (read by package_images.sh)"
