#!/usr/bin/env bash
# Build the lean no-Frame broker image and load it into the single-node k3s.
# No registry — podman build + `k3s ctr images import`. The nexus binary is
# built on the HOST first (the host has git auth for private modules), then
# COPY'd into the image.
set -euo pipefail

NEXUS_SRC="${NEXUS_SRC:-/usr/local/src/nexus}"
TAG="${TAG:-dev}"
IMG="localhost/nexus-broker:${TAG}"
CTX="$(mktemp -d)"

echo "==> building nexus broker from ${NEXUS_SRC}"
( cd "$NEXUS_SRC" && go build -o "${CTX}/nexus" ./nexus/cmd/nexus )

cp "$(dirname "$0")/Dockerfile" "${CTX}/Dockerfile"
echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )

echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep nexus-broker || true
rm -rf "$CTX"
echo "==> done: ${IMG}"
