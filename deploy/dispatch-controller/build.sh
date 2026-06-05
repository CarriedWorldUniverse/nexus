#!/usr/bin/env bash
# Build the dispatch-controller image and load it into the single-node k3s.
set -euo pipefail

NEXUS_SRC="${NEXUS_SRC:-/usr/local/src/nexus}"
CW_SRC="${CW_SRC:-/tmp/cw-src}"
TAG="${TAG:-dev}"
IMG="localhost/dispatch-controller:${TAG}"
CTX="$(mktemp -d)"

echo "==> building dispatch-controller from ${NEXUS_SRC}"
( cd "$NEXUS_SRC" && go build -o "${CTX}/dispatch-controller" ./runtime/cmd/dispatch-controller )

echo "==> building cw from ${CW_SRC}"
[ -d "$CW_SRC" ] || git clone --depth 1 https://github.com/CarriedWorldUniverse/cw "$CW_SRC"
( cd "$CW_SRC" && go build -o "${CTX}/cw" ./cmd/cw )

cp "$(dirname "$0")/Dockerfile" "${CTX}/Dockerfile"
echo "==> podman build ${IMG}"
( cd "$CTX" && podman build -t "$IMG" . )

echo "==> import into k3s containerd"
podman save "$IMG" | sudo k3s ctr images import -
sudo k3s ctr images ls | grep dispatch-controller || true
rm -rf "$CTX"
echo "==> done: ${IMG}"
