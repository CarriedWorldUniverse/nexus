<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/nexus-platform/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`nexus-platform`](https://github.com/CarriedWorldUniverse/nexus-platform)'s live `README.md`.
    Edit the README in the repo, not this page.

# nexus-platform

Umbrella distribution repo for the [nexus](https://github.com/CarriedWorldUniverse/nexus)
deployable bundle.

This repo does NOT contain nexus source code. It pins specific versions of each
component (nexus, ledger, bridle, plus the MCPs / agentfunnel / agora that ship
from the nexus repo), runs cross-component integration tests against those
pins, and assembles single-archive bundles per OS/arch that operators (or
AI agents on locked-down machines) can install in one step.

## Layout

```
nexus-platform/
├── bundle.toml              # pinned component versions — the source of truth
├── Makefile                 # fetch + assemble + test + sign
├── install.sh / install.ps1 # operator entry point (packed into each archive)
├── README.md                # this file
├── LICENSE                  # Apache 2.0
├── cmd/
│   └── bundlectl/           # bundle.toml validation + assembly CLI
├── internal/
│   └── bundle/              # resolve / fetch / assemble / diff / manifest
├── templates/
│   └── sample.mcp.json      # MCP wiring template emitted by install
├── tests/
│   └── integration/         # cross-component bundle smoke test
└── .github/workflows/
    └── ci.yml               # PR-time integration test (NEX-288)
```

## Design

Each component repo (nexus, ledger, bridle, ...) keeps its own independent
versioning + release cadence. This avoids the code-churn cost of a monorepo
while still giving operators a coherent "which versions work together" answer
via `bundle.toml`.

When a new bundle is needed:
1. Update `bundle.toml` with the desired component versions
2. PR — CI runs integration tests against the new pins
3. On merge + `bundle-v*` tag, the release workflow:
   a. Fetches binaries from each component's GitHub release (no source)
   b. Runs the full integration smoke against the fetched binaries
   c. Assembles single archive per OS/arch
   d. Publishes the GitHub release with archives + checksums + changelog

Operators download one archive, run `./install.sh`, get a working network.

## Platform architecture (where the bundle deploys)

The bundle distributes the components; this is the topology they run in. Full design: [`docs/2026-05-30-platform-topology.md`](https://github.com/CarriedWorldUniverse/nexus-platform/blob/HEAD/docs/2026-05-30-platform-topology.md).

- **Three modes, one software:** standalone/embedded (auth bypassed, services co-located — what dMon runs today), self-hosted platform, and we-host. Modes 2 and 3 are identical software, different operator.
- **In-cluster vs native split:** in the hosted shape, **nexus stays native** (the client side — broker + Keel + aspects) and the **product services run on k3s** (herald, cairn, ledger, commonplace). On dMon, k3s is used for boundary *isolation* (ClusterIP forces native nexus through the ingress with real auth), not for orchestration.
- **Substrate:** single-node k3s (on dMon now, a small ARM EC2 as the eventual target — EKS is the destination, not the start). Same Deployment + Service manifests survive the lift.
- **Edge:** interchange is the single ingress — a public request edge plus a push relay (clients with no public ports dial out and hold a connection; the relay pushes events down it).

### Data layer

There is **no shared database**. Services are herald-gated and never read each other's tables, so each service owns its own store:

- **Per-service SQLite** is the system of record (herald, ledger, commonplace/FTS5+sqlite-vec, cairn) — it's what's already built, and Postgres's multi-client win doesn't apply with no shared dataset. **Postgres is not the default** (an earlier draft's "Postgres in-cluster" is retracted).
- **Durability via litestream → S3:** each SQLite file is continuously replicated to S3 for point-in-time recovery, with zero DB-server ops.
- **Object storage on S3** for cairn repos/LFS/artifacts (dovetails with porter as a casket-encrypted backing layer).
- **Redis = cache + relay pub/sub, not storage:** hot slow-changing lookups, and cross-pod fan-out for the interchange relay once it scales past one pod. Not v1-critical.
- **Upgrade path is per-service, not "migrate all to RDS":** Turso/libSQL for the SQLite services, Neon only where a service genuinely needs Postgres semantics.

## Status

Under active development per
[NEX-281 epic](https://carriedworlduniverse.atlassian.net/browse/NEX-281):

- [x] NEX-283 — bundle.toml schema + `bundlectl` validation tooling
- [ ] NEX-284 — cross-component integration test harness
- [x] NEX-285 — bundle assembly (resolve + fetch + assemble + diff + manifest)
- [x] NEX-286 — install.sh / install.ps1
- [ ] NEX-287 — bundle README + first-time-operator guide
- [ ] NEX-288 — release workflow (PR-time CI in place; on-tag assemble + publish pending)

## License

Apache 2.0 — same as the underlying nexus components. Permissive use for
personal + commercial deployments. See [LICENSE](https://github.com/CarriedWorldUniverse/nexus-platform/blob/HEAD/LICENSE).
