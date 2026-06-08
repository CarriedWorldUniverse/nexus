# agora

[![CI](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/agora?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/agora/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/agora)](https://github.com/CarriedWorldUniverse/agora/blob/main/LICENSE)

The operator-facing interactive TUI. Persistent-WS chat panel on top of bridle's claudecode engine.

**Source:** [github.com/CarriedWorldUniverse/agora](https://github.com/CarriedWorldUniverse/agora)

## What it does

- Holds a persistent WebSocket to nexus (push delivery, no polling).
- Renders chat from the cluster in real-time in a [bubbletea](https://github.com/charmbracelet/bubbletea) TUI.
- Lets the operator type into the conversation; runs the operator-aspect identity bound to the keyfile.
- Acts as a proactive notification channel via `notify_operator` (fenced-block parse, NEX-63).

Architecturally identical to any autonomous aspect — agora reuses the same machinery (bridle claudecode driver per-turn, funnel for inbox + filter + dispatch, per-aspect keyfile). The novel piece is the outer shell + the operator-channel routing rule.

## Install

```sh
# macOS, Apple Silicon
curl -L -o agora.tar.gz https://github.com/CarriedWorldUniverse/agora/releases/download/v0.1.0/agora_v0.1.0_darwin_arm64.tar.gz
tar xzf agora.tar.gz
./agora --keyfile <path-to-keyfile>
```

Linux + macOS + Windows × amd64 + arm64.

## Build from source

```sh
git clone https://github.com/CarriedWorldUniverse/agora.git
cd agora
make build
./bin/agora --version
```

## Flags worth knowing

| Flag | Purpose |
|---|---|
| `--keyfile <path>` | required; identity keyfile bound to operator aspect |
| `--cursor-dir <dir>` | chat cursor file location; defaults to keyfile parent (NEX-119 — clean swap between agora ↔ CC-comms-mcp) |
| `--claude <path>` | path to the `claude` binary (default: `claude` on PATH) |
| `--log-file <path>` | log destination (default: `/tmp/agora.log`) |
| `--version` | print version and exit |

## Where to dig deeper

- [Architecture overview](../architecture.md) — how agora fits as an operator-as-aspect
- [Operator as aspect (WS extension)](../archive/2026-05-04-operator-as-aspect-ws-extension.md)
