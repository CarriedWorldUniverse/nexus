# Nexus

[![CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/nexus.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/nexus)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/nexus)](https://github.com/CarriedWorldUniverse/nexus/blob/main/LICENSE)

**A coordination layer for running multiple AI agents as a coherent team.**

Nexus is a central broker process plus a lightweight per-agent runtime. Agents (called **aspects**) register on boot, communicate through a shared message bus, and invoke each other's stateless capabilities ("hands") over that same bus. All context is owned by Nexus, not by the underlying AI provider — sessions live as tree-structured JSONL files, compaction is proactive, and rewind is a non-destructive tree operation.

---

## What's inside

- **[Architecture](architecture.md)** — the bird's-eye view: broker, aspects, funnel, bridle, hands, observability.
- **[Repos](repos/index.md)** — each component (8 repos, 8 binaries from this repo alone) with live build status and release artifacts per OS.
- **[Policies](policies/index.md)** — code standards, work routing across aspects, the trunk-based git workflow.
- **[Specs](specs/index.md)** — the design corpus. 30+ docs covering frame role, funnel/compaction, hand dispatch, observability, storage abstraction, more.

## Quick start

If you're new and just want to see the moving parts:

1. Read **[Architecture](architecture.md)** — covers what an aspect is, how the broker routes messages, where the funnel sits between bridle (per-turn deliberation) and provider (the model).
2. Browse **[Repos](repos/index.md)** — each repo card has its role + how it talks to nexus.
3. Skim a spec — try **[aspect-funnel architecture](2026-05-02-aspect-funnel-architecture.md)** or **[hand dispatch v0.1](2026-04-30-hand-dispatch-v0_1.md)** for the substance of the runtime.

## Status

Nexus is **operational** in single-operator mode. Every public repo has CI on linux + macOS (some + windows), tagged releases via goreleaser, branch-protected main with required status checks. AI aspect identities, multi-operator support, and several feature gaps tracked in [Jira (NEX-* tickets)](https://carriedworlduniverse.atlassian.net) are the open frontier.

The codebase is publicly readable but the running cluster is single-operator + tailnet-gated. There is no public broker.

## Conventions

This documentation site is built from the markdown in `docs/` via [MkDocs Material](https://squidfunk.github.io/mkdocs-material/), deployed to GitHub Pages on every push to `main`. Edit any page by clicking the pencil icon in its top-right — that opens the source `.md` in GitHub's editor and walks you through a PR (per the [git workflow policy](policies/git-workflow.md)).
