#!/usr/bin/env bash
# Copy the images to the instance and start the engine. Needs an instance that
# already exists; creates no infrastructure. Still requires confirmation,
# because it changes a running cloud cluster.
source "$(dirname "$0")/_common.sh"

require_confirmation "copy images to the EC2 instance and start the engine"

VERSION="${DUREXEC_VERSION:-$(cat "$ENGINE_DIR/.durexec-version" 2>/dev/null || true)}"
[ -n "$VERSION" ] || { echo "FATAL: no version; run build_images.sh first." >&2; exit 1; }

ART="$ENGINE_DIR/.artifacts/durexec-images-$VERSION.tar.gz"
[ -f "$ART" ] || { echo "FATAL: $ART missing; run package_images.sh." >&2; exit 1; }

IP=$(instance_ip)
echo "version:  $VERSION"
echo "instance: $IP"
echo

# Refuse to deploy an image that was never checked. The safety property that
# the simulator cannot become a public service lives in the compose file, and
# this is the last moment before that file is used in anger.
echo "=== re-checking that nothing would be publicly bound ==="
"$(dirname "$0")/assert_no_public_bind.sh"
echo

echo "=== copying the image archive and the compose files ==="
scp -o StrictHostKeyChecking=accept-new -i "$(ssh_key)" \
  "$ART" "$ART.sha256" "ec2-user@$IP:/tmp/"
# -r for the postgres replication scripts, which the compose file bind-mounts
# via a relative path out of deploy/aws. The directory layout on the instance
# has to match what those relative paths expect.
ssh -i "$(ssh_key)" "ec2-user@$IP" 'mkdir -p /opt/durexec/deploy/aws /opt/durexec/deploy/postgres /opt/durexec/deploy/grafana'
scp -o StrictHostKeyChecking=accept-new -i "$(ssh_key)" \
  "$ENGINE_DIR/deploy/aws/docker-compose.cloud.yml" \
  "$ENGINE_DIR/deploy/aws/prometheus.cloud.yml" \
  "ec2-user@$IP:/opt/durexec/deploy/aws/"
scp -r -o StrictHostKeyChecking=accept-new -i "$(ssh_key)" \
  "$ENGINE_DIR/deploy/postgres/." "ec2-user@$IP:/opt/durexec/deploy/postgres/"
scp -r -o StrictHostKeyChecking=accept-new -i "$(ssh_key)" \
  "$ENGINE_DIR/deploy/grafana/." "ec2-user@$IP:/opt/durexec/deploy/grafana/"

echo
echo "=== verifying the checksum ON THE INSTANCE, then loading ==="
# The checksum is verified where the file landed, not where it was made. An
# scp truncated by a dropped connection produces a short archive that
# `docker load` may accept partially, and Phase 6 already lost a run to nodes
# running an image that was not the one intended.
remote bash -s <<REMOTE
set -euo pipefail
cd /tmp
echo "\$(cat durexec-images-$VERSION.tar.gz.sha256)  durexec-images-$VERSION.tar.gz" > /tmp/check.sha
sha256sum -c /tmp/check.sha
docker load -i "durexec-images-$VERSION.tar.gz"
rm -f "durexec-images-$VERSION.tar.gz" "durexec-images-$VERSION.tar.gz.sha256" /tmp/check.sha
echo "--- images on the instance ---"
docker images --format '{{.Repository}}:{{.Tag}}' | grep durexec
REMOTE

echo
echo "=== recording the version in the instance env file ==="
remote "sed -i 's/^DUREXEC_VERSION=.*/DUREXEC_VERSION=$VERSION/' /opt/durexec/.env && grep '^DUREXEC_VERSION=' /opt/durexec/.env"

echo
echo "=== starting the stack ==="
# The extsim profile is deliberately NOT included. A plain bring-up has no
# simulator: Phase 8 scenario H was corrupted once because an unrelated
# workflow reached a globally-armed fault point, and the same discipline
# applies here -- test-only components start only when a scenario asks.
remote bash -s <<'REMOTE'
set -euo pipefail
cd /opt/durexec/deploy/aws
set -a; . /opt/durexec/.env; set +a
docker compose -f docker-compose.cloud.yml up -d
echo
docker compose -f docker-compose.cloud.yml ps
REMOTE

echo
echo "Stack started. Verify it with:"
echo "  ./scripts/aws/smoke.sh"
