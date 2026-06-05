#!/usr/bin/env bash
# Build the builder-agent worker image (NEX-436) and load it into the
# single-node k3s on dMon. No registry — podman build + `k3s ctr import`.
#
# cw + the nexus binaries are built on the HOST first (the host has the git
# auth for cw's private modules), then COPY'd into the image — avoids
# in-Dockerfile private-module auth.
set -euo pipefail

NEXUS_SRC="${NEXUS_SRC:-/usr/local/src/nexus}"
CW_SRC="${CW_SRC:-/tmp/cw-src}"        # github.com/CarriedWorldUniverse/cw checkout
TAG="${TAG:-dev}"
IMG="localhost/nexus-builder:${TAG}"
CTX="$(mktemp -d)"

echo "==> building nexus binaries from ${NEXUS_SRC}"
( cd "$NEXUS_SRC"
  for b in agentfunnel nexus-issue-mcp nexus-jira-mcp nexus-comms-mcp; do
    go build -o "${CTX}/${b}" "./runtime/cmd/${b}"
  done )

echo "==> building cw from ${CW_SRC}"
[ -d "$CW_SRC" ] || git clone --depth 1 https://github.com/CarriedWorldUniverse/cw "$CW_SRC"
( cd "$CW_SRC" && go build -o "${CTX}/cw" ./cmd/cw )

cp "$(dirname "$0")/Dockerfile" "${CTX}/Dockerfile"
echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )

echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep nexus-builder || true
rm -rf "$CTX"
echo "==> done: ${IMG}"
