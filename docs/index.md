# Nexus

[![CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/nexus.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/nexus)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/nexus)](https://github.com/CarriedWorldUniverse/nexus/blob/main/LICENSE)

**A coordination layer for running multiple AI agents as a coherent team.**

We build the substrate for a small team of specialized AI personalities ("aspects") to work together on real engineering — through shared chat, work routing, a deliberation funnel that owns context across compaction, identity rooted in CWB/Herald, and a dispatch fabric that runs agents as on-demand cloud pods. The operator stays in the loop as one peer on the bus, not as a manager wrangling threads.

---

## The two halves

The stack has two cooperating halves:

- **The agent runtime** — the broker (`nexus`), the per-aspect deliberation funnel, the `bridle` provider library, the `agora` operator TUI, and a **dispatch fabric** that runs aspects as on-demand cloud pods on a single-node k3s cluster.
- **The CWB platform** — the identity and authority plane the agents stand on: `herald` (identity), `ledger` (work/audit), `commonplace` (knowledge), `cairn` (git), all as standalone gRPC-over-mTLS services behind `interchange` as the public boundary gateway, with a `custodian` brokering external secrets.

## What's inside

- **[Architecture](architecture.md)** — the bird's-eye view: broker, aspects, funnel, bridle, the dispatch fabric, and the CWB pillars.
- **[Repos](repos/index.md)** — every component across the ~22 org repos. These pages are **mirrored from each repo's live README** (single source of truth).
- **[Policies](policies/index.md)** — code standards, work routing across aspects, the trunk-based git workflow.
- **[Specs](specs/index.md)** — the design corpus, led by the current dispatch-native / herald-bootstrap / roundtable-era docs.

## How it fits together

```
                    ┌─────────────────────────────┐
                    │      nexus (broker)          │
                    │  chat · dashboard · obs ·    │
                    │       dispatch fabric        │
                    └──────────────┬──────────────┘
                                   │  WS
        ┌──────────────────────────┼──────────────────────────┐
        ▼                          ▼                          ▼
   ┌─────────┐               ┌──────────┐              ┌──────────┐
   │ aspects │               │  agora   │              │  funnel  │
   │ (cloud  │               │ (operator│              │ (per-    │
   │  pods)  │               │   TUI)   │              │ aspect)  │
   └─────────┘               └──────────┘              └──────────┘
        └──────────────────────────┴──────────────────────────┘
                                   │
                                   ▼
                            ┌──────────────┐
                            │   bridle     │  one turn, N providers
                            └──────────────┘
```

Aspects connect to nexus over WebSocket. Each wraps `bridle` (a single deliberation turn against any provider) inside a `funnel` (which owns the inbox, compaction, output filter, and observability). agora is the operator's seat at the same table.

## Quick start

If you're new and just want to see the moving parts:

1. Read **[Architecture](architecture.md)** — what an aspect is, how the broker routes messages, where the funnel sits between bridle and the model, how dispatch runs aspects as pods, and how the CWB pillars hang together.
2. Browse **[Repos](repos/index.md)** — each page is the repo's own README, with its role and how it talks to the rest of the stack.
3. Skim a current spec — try **[dispatch-native platform architecture](2026-06-08-dispatch-native-platform-architecture.md)** or **[herald-rooted agent bootstrap](2026-06-03-herald-rooted-agent-bootstrap-design.md)**.

## Status

Honest single-operator R&D — operational, moving fast, publicly readable, not a product. The running cluster is single-operator and tailnet-gated; there is no public broker. The codebase is open.

The agent runtime (broker, funnel, dispatch) and the CWB pillars (herald, ledger, commonplace, cairn) are deployed and meshed. Napping/wake-on-mention presence, aspect-owned worker "hands", credential custody, multi-agent deliberation (the roundtable), multi-operator support, and offsite storage are in flight. Individual repos vary from tagged-and-stable to fast-moving — check each repo's own README for where it sits. (NEX-* references in commits and docs are private-tracker issues; external readers see opaque identifiers.)

## Conventions

This documentation site is built from the markdown in `docs/` via [MkDocs Material](https://squidfunk.github.io/mkdocs-material/), deployed to GitHub Pages on every push to `main` that touches `docs/**`. The **Repos** pages are regenerated from each repo's live `README.md` at build time — edit the README in the repo, not the page here (see [repos/index.md](repos/index.md)). For everything else, click the pencil icon top-right to open the source `.md` and walk a PR (per the [git workflow policy](policies/git-workflow.md)).
