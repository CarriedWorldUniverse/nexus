# bridle

[![CI](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/bridle?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/bridle/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/bridle.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/bridle)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/bridle)](https://github.com/CarriedWorldUniverse/bridle/blob/main/LICENSE)

Per-turn deliberation library. Drives a single LLM turn with one tool surface, emitting a structured event stream.

> A bridle controls and directs without being the horse — sits beneath the funnel, governs what the model produces, provider-agnostic.

**Source:** [github.com/CarriedWorldUniverse/bridle](https://github.com/CarriedWorldUniverse/bridle)

## Providers included

| Provider | Description |
|---|---|
| `claudecode` | Subprocess CLI wrapper around `claude -p`. The dominant path. |
| `claude` | Direct Anthropic Messages API. |
| `ollama` | Local Ollama server (open-weights models). |
| `claudepty` | ACP-driven `claude` REPL over [acp-claude-pty](acp-claude-pty.md) subprocess. |
| `gemini-cli` / `gemini-api` | Gemini provider via CLI or direct API. |
| `bedrock` | AWS Bedrock Converse API. |

## Install (as a library)

```go
import bridle "github.com/CarriedWorldUniverse/bridle"
```

`go get github.com/CarriedWorldUniverse/bridle@v0.1.0` — pulled from the Go module proxy.

## Consumers

- [nexus/frame/funnel](nexus.md) — the load-bearing consumer; funnel wraps bridle.
- [agora](agora.md) — uses bridle's claudecode provider directly for the operator TUI's deliberation loop.

## Local dev

`stubfunnel` is a small CLI in this repo that exercises bridle against real providers without involving the funnel. Useful for provider development.

```sh
go run ./stubfunnel/ -provider claudecode -prompt "what time is it?"
```

`stubfunnel --version` reports the build's version string (git-describe in dev, clean tag in release).

## Where to dig deeper

- [Provider adapter spec](../archive/2026-04-24-provider-adapter-spec.md)
- [Funnel/bridle caching spec](../archive/2026-05-03-funnel-bridle-caching-spec.md)
- [Pi extract / bridle gaps](../archive/2026-05-13-pi-extract-bridle-gaps.md) — design audit against Pi
