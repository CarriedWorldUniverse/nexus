# nexus

[![CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/nexus.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/nexus)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/nexus)](https://github.com/CarriedWorldUniverse/nexus/blob/main/LICENSE)

The coordination broker. Hosts the chat router, dashboard, observability hub, credential store, and the embedded Frame (`keel`). All aspects connect here over WS.

**Source:** [github.com/CarriedWorldUniverse/nexus](https://github.com/CarriedWorldUniverse/nexus)

## Binaries shipped

| Binary | Role |
|---|---|
| `nexus` | The broker + dashboard + embedded Frame |
| `agentfunnel` | Out-of-process funnel runtime; aspects spawn from this |
| `aspect` | Pre-funnel scaffold for the same job; legacy path |
| `nexus-comms-mcp` | Chat MCP server (used by shadow CC sessions) |
| `nexus-imap-mcp` | IMAP MCP (operator's nexus@ mailbox) |
| `nexus-jira-mcp` | Jira MCP (ticket flow) |
| `nexus-watch` | Live observability tail / event monitor |
| `outpost` | Remote relay endpoint |

## Install

Grab the v0.1.0 release for your platform:

```sh
# macOS, Apple Silicon
curl -L -o nexus.tar.gz https://github.com/CarriedWorldUniverse/nexus/releases/download/v0.1.0/nexus_v0.1.0_darwin_arm64.tar.gz
tar xzf nexus.tar.gz && ./nexus -help
```

Replace `nexus` in the URL with `agentfunnel` / `aspect` / `nexus-comms-mcp` / etc to grab a different binary. Linux + macOS × amd64 + arm64. No Windows builds (some binaries are unix-specific via PTY).

## Build from source

```sh
git clone https://github.com/CarriedWorldUniverse/nexus.git
cd nexus
make build       # builds all 8 binaries into bin/, version baked in via git-describe
make test
```

Requires Go 1.23+. No CGO (`CGO_ENABLED=0`).

## What it depends on

- [bridle](bridle.md) — funnel imports bridle for per-turn deliberation
- [casket-go](casket-go.md) — channel identity for interchange flows
- modernc.org/sqlite (pure-Go SQLite) for persistent state
- standard-library WebSocket via gorilla/websocket

## Where to dig deeper

- [Architecture overview](../architecture.md)
- [Aspect-funnel architecture spec](../2026-05-02-aspect-funnel-architecture.md) — the foundational doc
- [Frame role spec](../2026-04-28-frame-role-spec.md)
- [Storage abstraction spec](../2026-05-05-storage-abstraction-spec.md)
