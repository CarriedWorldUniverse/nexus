#!/usr/bin/env bash
# Build the lean no-Frame broker image and load it into k3s containerd.
# No registry — podman build + `k3s ctr images import`. The nexus binary is
# built on the HOST first (the host has git auth for private modules), then
# COPY'd into the image. nexus is CGO-free, so it cross-compiles to any arch.
#
#   ARCH=amd64|arm64       target arch (default: host arch). arm64 cross-compiles
#                          on an amd64 host (verified 2026-06-27).
#   IMPORT_HOST=user@host  import into THAT node's k3s containerd over ssh instead
#                          of the local node — e.g.
#                            ARCH=arm64 IMPORT_HOST=jacinta@robo-dog ./build.sh
#                          to pre-position the broker on the arm64 GB10 node.
set -euo pipefail

NEXUS_SRC="${NEXUS_SRC:-/usr/local/src/nexus}"
TAG="${TAG:-dev}"
ARCH="${ARCH:-$(go env GOARCH)}"
IMG="localhost/nexus-broker:${TAG}"
CTX="$(mktemp -d)"

echo "==> building nexus broker (GOARCH=${ARCH}) from ${NEXUS_SRC}"
( cd "$NEXUS_SRC" && CGO_ENABLED=0 GOARCH="${ARCH}" go build -o "${CTX}/nexus" ./nexus/cmd/nexus )

# cw CLI: the broker shells out to it for scoped git-credential grants
# (runner.provisionRun) — a repo dispatch fails without it on PATH.
CW_SRC="${CW_SRC:-/tmp/cw-src}"
[ -d "$CW_SRC" ] || git clone --depth 1 https://github.com/CarriedWorldUniverse/cw "$CW_SRC"
( cd "$CW_SRC" && CGO_ENABLED=0 GOARCH="${ARCH}" go build -o "${CTX}/cw" ./cmd/cw )

cp "$(dirname "$0")/Dockerfile" "${CTX}/Dockerfile"
echo "==> podman build ${IMG} (linux/${ARCH})"
( cd "$CTX" && podman build --platform "linux/${ARCH}" -t "$IMG" . )

if [ -n "${IMPORT_HOST:-}" ]; then
  echo "==> import into ${IMPORT_HOST} k3s containerd (cross-node)"
  podman save "$IMG" | ssh "$IMPORT_HOST" 'sudo k3s ctr images import -'
  ssh "$IMPORT_HOST" 'sudo k3s ctr images ls | grep nexus-broker' || true
else
  echo "==> import into local k3s containerd"
  podman save "$IMG" | sudo k3s ctr images import -
  sudo k3s ctr images ls | grep nexus-broker || true
fi
rm -rf "$CTX"
echo "==> done: ${IMG} (linux/${ARCH})"
