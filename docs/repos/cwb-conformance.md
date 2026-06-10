<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/cwb-conformance/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`cwb-conformance`](https://github.com/CarriedWorldUniverse/cwb-conformance)'s live `README.md`.
    Edit the README in the repo, not this page.

# cwb-conformance

The CWB platform conformance + end-to-end test suite. Exercises **herald**
(identity), **interchange-gateway** (boundary), **commonplace** (knowledge
store), **cairn** (git host), and **ledger** (issues/tracker) through their
real public interfaces — plus entitlement enforcement and the cross-service
lifecycle that ties them together.

The suite is a **true external client**: it shares no code with the services
it tests and drives real wire protocols (real `git` over SSH/HTTPS, raw HTTP
through the gateway, real herald token exchange). That's what lets it catch
integration-boundary bugs a shared-code test would miss.

## Design

See [`docs/2026-05-31-cwb-conformance-design.md`](https://github.com/CarriedWorldUniverse/cwb-conformance/blob/HEAD/docs/2026-05-31-cwb-conformance-design.md).

## Usage

```sh
cwb-conform -target dmon -layers all   # full run against dMon — all layers
cwb-conform -target dmon -layers cairn # a single layer
cwb-conform -target dmon -reap         # clean up orphan test orgs
```

The layers, in run order, are: `herald`, `gateway`, `entitlement`,
`commonplace`, `cairn`, `ledger`, `journey`, `reap`.

Two values come from the environment, never the committed target file:

```sh
export CWB_ADMIN_TOKEN=$(kubectl -n cwb get secret herald-secrets -o jsonpath='{.data.admin_token}' | base64 -d)
# Admin provisioning is admin-DIRECT, off the gateway. On the k3s node, point it
# at herald's ClusterIP — the node routes it via kube-proxy, and it is far more
# stable under a full run than `kubectl port-forward` (a saturated port-forward
# was the only source of full-run flakes):
export CWB_HERALD_ADMIN_URL=http://$(kubectl -n cwb get svc herald -o jsonpath='{.spec.clusterIP}'):8099
export CWB_RUN_ID=$(date +%s)     # deterministic per-run org naming
# Off-node (no ClusterIP route), fall back to a port-forward to localhost:8099.
# CWB_HTTP_TIMEOUT (seconds, default 90) tunes the client timeout for slow targets.
```

Every conformance **assertion** goes through the gateway as a true external
client; only fixture provisioning uses the direct admin path, so herald's admin
API never has to be exposed at the boundary.

Targets are env-agnostic — the same test code runs against live dMon k3s today
and an ephemeral CI cluster later, selected purely by `-target`. **All layers
are green** against dMon: `herald`, `gateway`, `entitlement`, `commonplace`,
`cairn`, `ledger`, `journey`, with `reap` cleaning up afterwards. The `journey`
layer is client-composed — it proves the pillars compose
under one identity, with the unbuilt cairn↔ledger integration / human review /
server-merge performed client-side and logged as simulated (see the design doc
§5f). The `cairn` + `journey` layers drive the real `git` binary, so they skip
when `git` is absent.
