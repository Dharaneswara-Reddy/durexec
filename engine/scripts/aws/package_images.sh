#!/usr/bin/env bash
# Turn the built images into tarballs ready to copy to the instance.
# LOCAL ONLY -- no AWS API call, no charge, nothing published.
#
# Why no container registry at all:
#
#   ECR costs $0.10 per GB-month and the push needs credentials on whatever
#   runs it. These two images are about 30 MB together, they are needed by
#   exactly one instance, and that instance can be reached over SSH. `docker
#   save` into a gzipped tar and `scp` costs nothing, publishes nothing, and
#   has no registry authentication to get wrong. It also satisfies the
#   requirement not to publish artifacts to a cloud registry during
#   preparation.
#
#   ECR becomes the right answer the moment there is more than one instance,
#   or a CI job doing the deploying. The commands for that are in
#   docs/cloud-deployment/DEPLOYMENT-RUNBOOK.md, prepared but not run.
set -euo pipefail

cd "$(dirname "$0")/../.."

OUT="${DUREXEC_ARTIFACT_DIR:-./.artifacts}"
VERSION="${DUREXEC_VERSION:-$(cat .durexec-version 2>/dev/null || true)}"
if [ -z "$VERSION" ]; then
  echo "FATAL: no version. Run scripts/aws/build_images.sh first, or set DUREXEC_VERSION." >&2
  exit 1
fi

for img in "durexec-engine:$VERSION" "durexec-extsimd:$VERSION"; do
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    echo "FATAL: $img is not present locally. Run scripts/aws/build_images.sh." >&2
    exit 1
  fi
done

mkdir -p "$OUT"

# Both images in ONE archive. Two archives could be copied in the wrong
# combination -- a new engine with an old simulator -- and nothing downstream
# would notice. One archive makes the pair atomic.
ARCHIVE="$OUT/durexec-images-$VERSION.tar.gz"
echo "saving durexec-engine:$VERSION and durexec-extsimd:$VERSION"
docker save "durexec-engine:$VERSION" "durexec-extsimd:$VERSION" | gzip -9 > "$ARCHIVE"

# A checksum, so the instance can prove it loaded the bytes that were built
# rather than a truncated copy from an interrupted scp. Phase 6 lost a whole
# run to three nodes silently running three different builds; a manifest is
# the cheapest guard against the cloud version of that.
sha256sum "$ARCHIVE" | awk '{print $1}' > "$ARCHIVE.sha256"

cat > "$OUT/manifest-$VERSION.txt" <<MEOF
durexec cloud deployment artifact manifest
version:        $VERSION
built:          $(date -Is)
git HEAD:       $(git rev-parse HEAD)
git dirty:      $([ -n "$(git status --porcelain)" ] && echo yes || echo no)
platform:       ${DUREXEC_PLATFORM:-linux/amd64}
archive:        $(basename "$ARCHIVE")
archive sha256: $(cat "$ARCHIVE.sha256")
images:
  durexec-engine:$VERSION   $(docker image inspect "durexec-engine:$VERSION" --format '{{.Id}}')
  durexec-extsimd:$VERSION  $(docker image inspect "durexec-extsimd:$VERSION" --format '{{.Id}}')
MEOF

echo
cat "$OUT/manifest-$VERSION.txt"
echo
ls -lh "$ARCHIVE"
echo
echo "Nothing has been uploaded. Copy it with scripts/aws/deploy_stack.sh once a"
echo "deployment is authorized."
