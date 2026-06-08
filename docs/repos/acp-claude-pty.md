# acp-claude-pty

[![CI](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/acp-claude-pty?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/acp-claude-pty/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/acp-claude-pty.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/acp-claude-pty)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/acp-claude-pty)](https://github.com/CarriedWorldUniverse/acp-claude-pty/blob/main/LICENSE)

A PTY driver and [Agent Client Protocol](https://agentclientprotocol.com) server for the `claude` CLI, in one Go binary.

**Source:** [github.com/CarriedWorldUniverse/acp-claude-pty](https://github.com/CarriedWorldUniverse/acp-claude-pty)

## Why it exists

`claude` has no stable scriptable IPC. The only honest way to drive it programmatically is to hold the interactive REPL through a pseudo-terminal and parse what it prints. This binary does that, and only that — no orchestration, no compaction, no policy. ACP is the wire format on top so callers don't see the PTY.

Used by bridle's `claudepty` provider as the per-turn workhorse.

## Install

```sh
curl -L https://github.com/CarriedWorldUniverse/acp-claude-pty/releases/download/v0.1.0/acp-claude-pty_v0.1.0_darwin_arm64.tar.gz | tar xz
./acp-claude-pty --version
```

Linux + macOS × amd64 + arm64. Windows is gated behind ConPTY v2 work.

## Key tech decisions

- Driver writes `\r` (CR) to commit, not `\n`. claude-code's TUI runs raw mode and only recognizes CR as Enter. Verified by [plumb's probe-8](../archive/2026-05-13-pi-extract-bridle-gaps.md): 3/3 wedge on LF, 3/3 land on CR.
- Driver pre-sets `ICRNL=off` on the PTY slave's termios before child exec — closes a boot-window race where the driver could write before the child finished `MakeRaw`.
- mockclaude test target with regression guards: `\n` alone doesn't commit, `\r\n` commits exactly once.

## Where to dig deeper

- NEX-83 epic (internal) — claude-pty multi-protocol adapter
- Internal `docs/` in the repo
