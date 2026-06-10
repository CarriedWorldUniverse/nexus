<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/commonplace/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`commonplace`](https://github.com/CarriedWorldUniverse/commonplace)'s live `README.md`.
    Edit the README in the repo, not this page.

# commonplace

The CWB **knowledge** pillar — a service where agents store, retrieve, and
**semantically search** knowledge: query by concept, get the appropriate
(similar-in-meaning) entries back.

Peer to herald (identity), cairn (git), and ledger (tracking). commonplace
serves **gRPC only, over mTLS** (`cmd/commonplace`), behind
[interchange-gateway](https://github.com/CarriedWorldUniverse/interchange) —
not the nexus bus. Identity is gateway-injected: interchange runs herald
verification and passes the verified caller as `cwb-*` gRPC metadata that
commonplace trusts over the mTLS hop. Any HTTP/JSON view of the API is
synthesized at the gateway edge; the service itself is gRPC.

The API is a `KnowledgeService` with two RPCs today — **Store** (embed + index
an entry) and **Search** (embed the query, fuse vector nearest-neighbour with
FTS5 keyword). On store, entries are embedded (AI-switchable model, local
ollama / `nomic-embed-text` by default) and indexed in SQLite alongside a
vector column; entries are kept **raw**, with no ingest-time summarization. The
recall path — a query-tailored synthesized brief over the top-k — ships as a
`KnowledgeService` RPC, not a `net/http` handler.

**Intent:** the MVP is the first deliberate layer of a *learning-memory
substrate for AI* — embeddings + vector retrieval now, designed to grow into
usage-weighted retrieval → concept-graph/learning-paths → (north-star)
connectome-inspired typed-node memory. Each rung ships value and compounds
toward the substrate. This is single-operator R&D; the pillar dogfoods itself.

## Status

**MVP built.** Store + Search ship as gRPC over mTLS; embeddings via the ollama
provider seam; vector + FTS5 hybrid retrieval. The original REST design in the
plan doc is marked **HISTORICAL** — the live shape is gRPC-only since the
Phase 1 migration. New retrieval features (e.g. recall synthesis) are added as
`KnowledgeService` RPCs in `grpcserver.go`.

## Config

`cmd/commonplace` is configured by environment: `COMMONPLACE_GRPC_ADDR`,
`COMMONPLACE_DB`, `COMMONPLACE_TLS_{CERT,KEY,CA}` (mTLS),
`COMMONPLACE_DEV_INSECURE` for local dev, and the `COMMONPLACE_EMBED_*` knobs
(provider / URL / model / dim). Deploy manifests live under [`deploy`](deploy).

## Design

- Spec: [`docs/2026-05-31-commonplace-mvp-spec.md`](https://github.com/CarriedWorldUniverse/commonplace/blob/HEAD/docs/2026-05-31-commonplace-mvp-spec.md)
- Plan (HISTORICAL — pre-gRPC): [`docs/2026-05-31-commonplace-mvp-plan.md`](https://github.com/CarriedWorldUniverse/commonplace/blob/HEAD/docs/2026-05-31-commonplace-mvp-plan.md)

## Stack position

`herald` (identity) · `cairn` (git) · `ledger` (tracker) · **`commonplace` (knowledge)** · `interchange` (gateway)
