#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

render() {
  helm template test "$chart_dir" "$@" > "$tmp_dir/rendered.yaml"
}

assert_contains() {
  local needle="$1"
  if ! grep -Fq "$needle" "$tmp_dir/rendered.yaml"; then
    echo "expected rendered chart to contain: $needle" >&2
    exit 1
  fi
}

assert_not_contains() {
  local needle="$1"
  if grep -Fq "$needle" "$tmp_dir/rendered.yaml"; then
    echo "expected rendered chart not to contain: $needle" >&2
    exit 1
  fi
}

render \
  --set name=demo \
  --set namespace=nexus \
  --set image=example/demo:1 \
  --set port=8080 \
  --set storage.size=1Gi \
  --set env.FOO=bar \
  --set envFromSecret=demo-env
assert_contains "kind: Deployment"
assert_contains "kind: Service"
assert_contains "type: ClusterIP"
assert_contains "kind: PersistentVolumeClaim"
assert_contains "claimName: demo-data"
assert_contains "name: FOO"
assert_contains "name: demo-env"

render \
  --set name=demo \
  --set namespace=nexus \
  --set image=example/demo:1
assert_contains "kind: Deployment"
assert_not_contains "kind: Service"
assert_not_contains "kind: PersistentVolumeClaim"

render \
  --set name=demo \
  --set namespace=nexus \
  --set image=example/demo:1 \
  --set port=8080 \
  --set tailnetEdge=true \
  --set tailnetName=demo
assert_contains "name: demo-tailnet"
assert_contains "type: LoadBalancer"
assert_contains "loadBalancerClass: tailscale"
assert_contains "tailscale.com/hostname: \"demo\""

render \
  --set name=demo \
  --set namespace=nexus \
  --set image=example/demo:1 \
  --set port=8080 \
  --set tailnetEdge=false
assert_not_contains "loadBalancerClass: tailscale"
assert_not_contains "name: demo-tailnet"

render \
  --set name=agent \
  --set namespace=nexus \
  --set kind=StatefulSet \
  --set image=example/agent:1 \
  --set identity.mode=secret \
  --set identity.secretName=agent-keyfile
assert_contains "kind: StatefulSet"
assert_contains "serviceName: agent"
assert_contains "name: keyfile"
assert_contains "secretName: agent-keyfile"
assert_contains "mountPath: \"/etc/nexus\""

render \
  --set name=agent \
  --set namespace=nexus \
  --set image=example/agent:1 \
  --set identity.mode=custodian
assert_contains "initContainers:"
assert_contains "name: identity-custodian"
assert_contains "identity custodian placeholder"

echo "render tests passed"
