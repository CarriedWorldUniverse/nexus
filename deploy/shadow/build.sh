#!/usr/bin/env bash
# Build the always-on shadow image (builder image + claude-code) and load it
# into the single-node k3s. Depends on localhost/nexus-builder:dev
# (deploy/worker/build.sh).
set -euo pipefail

TAG="${TAG:-dev}"
IMG="localhost/nexus-shadow:${TAG}"
CTX="$(dirname "$0")"

echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )

echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep nexus-shadow || true
echo "==> done: ${IMG}"
