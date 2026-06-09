#!/usr/bin/env bash
# Build the always-on maren artist image (builder image + headless Secret
# Service) and load it into the single-node k3s. Depends on
# localhost/nexus-builder:dev already being built (deploy/worker/build.sh).
set -euo pipefail

TAG="${TAG:-dev}"
IMG="localhost/nexus-maren:${TAG}"
CTX="$(dirname "$0")"

echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )

echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep nexus-maren || true
echo "==> done: ${IMG}"
