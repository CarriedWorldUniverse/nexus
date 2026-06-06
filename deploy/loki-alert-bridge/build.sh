#!/usr/bin/env bash
# Build the loki-alert-bridge image (NEX logging Phase 2) and load it into k3s.
set -euo pipefail
NEXUS_SRC="${NEXUS_SRC:-/usr/local/src/nexus}"
TAG="${TAG:-dev}"
IMG="localhost/loki-alert-bridge:${TAG}"
CTX="$(mktemp -d)"
echo "==> building loki-alert-bridge from ${NEXUS_SRC}"
( cd "$NEXUS_SRC" && go build -o "${CTX}/loki-alert-bridge" ./runtime/cmd/loki-alert-bridge )
cp "$(dirname "$0")/Dockerfile" "${CTX}/Dockerfile"
echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )
echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep loki-alert-bridge || true
rm -rf "$CTX"
echo "==> done: ${IMG}"
