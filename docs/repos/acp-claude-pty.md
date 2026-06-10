<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/acp-claude-pty/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`acp-claude-pty`](https://github.com/CarriedWorldUniverse/acp-claude-pty)'s live `README.md`.
    Edit the README in the repo, not this page.

# acp-claude-pty

[![CI](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/acp-claude-pty?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/acp-claude-pty/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/acp-claude-pty.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/acp-claude-pty)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/acp-claude-pty)](https://github.com/CarriedWorldUniverse/acp-claude-pty/blob/HEAD/LICENSE)

A PTY driver and ACP server for the Claude Code CLI, in one Go binary.

`acp-claude-pty` spawns `claude` (the official interactive CLI) inside a
pseudo-terminal, holds the REPL across many turns, and exposes the surface
over the [Agent Client Protocol](https://agentclientprotocol.com) on stdio.
Callers — typically `bridle.Provider` implementations in the Nexus stack,
or any ACP client — speak ACP and never see the PTY directly.

Apache 2.0. Status: v1 in development (NEX-84). macOS and Linux supported;
Windows is gated behind ConPTY v2.

---

## Why this exists

`claude` has no stable scriptable IPC. The only honest way to drive it
programmatically is to drive the interactive REPL through a pseudo-terminal
and parse what it prints. This binary does that and nothing more — no
protocol opinions below the ACP wire.

## Surface in 10 minutes

```
caller (ACP client)  <-- stdio -->  acp-claude-pty  <-- PTY -->  claude
```

### Run it

```
acp-claude-pty --cwd /path/to/spawn-dir [--command claude] [--log run.log] [--accept-workspace-trust=false]
```

- `--cwd` (required) — the directory `claude` is launched in. Place
  `CLAUDE.md`, `.mcp.json`, `.claude/settings.json`, and any caller-supplied
  files here. The `internal/spawndir` package can materialize them for you.
- `--command` (default `claude`) — the claude binary to spawn.
- `--accept-workspace-trust` (default `true`) — auto-accept claude's
  folder-trust dialog on launch. The caller controls `--cwd`, so trust is
  implied; a fresh spawn dir would otherwise wedge on the dialog. Set
  `false` to leave it for the caller. See "Subscription billing".
- `--log` (optional) — capture every PTY byte (pre-ANSI-strip) for
  debugging.

The binary speaks ACP on stdin/stdout. Connect any ACP client to it.

### Lifecycle

Per the NEX-83 lifecycle lockdown:

- One `acp-claude-pty` process holds one persistent `claude` REPL globally.
- `Start` pays the spawn+init cost once (~5.5s cold per plumb's probe-2).
- Subsequent turns are warm (~2.7s on a haiku-class turn).
- **Restart is caller-driven.** Use it when you want thread-isolated work;
  it costs ~3s (spawn+init) and resets the REPL state.
- Stop sends SIGTERM, waits `StopGrace` (default 2s) for the resume-hint
  tail, then escalates to SIGKILL if needed, and closes the PTY fd last.
  Closing the fd first would deliver a TTY hangup that surfaces as SIGKILL
  on macOS and burns the graceful tail.

### Slash commands

The binary is a **dumb pass-through on INPUT**: callers send slash command
text (`/compact`, `/clear`, `/exit`, `/model …`, `/effort …`) via ACP
`prompt` and the binary types it into the PTY. Output-side, the parser
emits typed events (`CompactStart`/`CompactEnd`, `ModelChanged`,
`EffortChanged`, `SessionExiting`) interleaved in wire-order with normal
output lines.

### Errors

Driver errors are typed:

| Kind | Meaning |
|------|---------|
| `crash` | spawn failed, or PTY read errored unexpectedly |
| `hang` | no output for `HangTimeout` (default 60s) |
| `prompt-timeout` | prompt did not return within `PromptTimeout` (default 5min) |
| `graceful-eof` | claude exited cleanly (zero status) |
| `aborted-by-sigterm` | claude received SIGTERM; resume hint flushed (exit 143) |
| `killed` | term_sig=9, no graceful tail |
| `model-invalid` | first-Send error: claude reported the model is unavailable |

`ErrModelInvalid` is a **first-Send error, not a Start error** — claude
launches its TUI regardless of model validity and only surfaces the failure
when a turn is attempted.

## Subscription billing (interactive only)

This adapter exists to hold an **interactive** Claude session, which stays on the
Pro/Max subscription. From **2026-06-15** Anthropic bills headless `claude -p` /
Agent-SDK usage against a separate API-rate credit pool — so two guards keep the
spawned session on the subscription rather than silently falling onto API
billing:

- **`-p`/`--print` is rejected** at `New()` — headless mode is Agent-SDK-billed
  and emits no REPL prompt for the detector anyway.
- **API-billing env vars are stripped** before exec — `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_USE_BEDROCK`,
  `CLAUDE_CODE_USE_VERTEX` — so an inherited API key can't flip the interactive
  session onto API billing. Set `Options.PreserveAPIEnv` to opt out (when API
  billing is intended).

## Layout

```
cmd/acp-claude-pty/   main entrypoint (ACP server over stdio)
cmd/mockclaude/       fake-claude binary for integration tests
internal/acpserver/   ACP frame <-> PTY operation mapping
internal/pty/         PTY driver (spawn, I/O loop, prompt detector, errors)
internal/spawndir/    spawn-directory materialization
internal/mockclaude/  fake-claude library (used by cmd/mockclaude)
internal/version/     build-version reporting (--version)
```

## Testing

```
go test ./...                  # all platforms
go test -tags=integration ...  # (TBD) live-binary integration tests
```

The mock-claude binary (`cmd/mockclaude`) is built and driven by the
unix integration tests in `internal/pty/`; it reproduces the OSC title-bar
busy/idle transitions, scripted prefix-matched responses, and the
exit-mode variations (clean / SIGTERM-with-resume-hint / model-invalid)
that the real claude TUI exhibits.

## Detector reference

Prompt-return detection is byte-stream-incremental and operates on the
raw (pre-ANSI-strip) stream. Default `PromptDetector` parses OSC title-bar
sequences (`\x1b]0;<glyph><text>\x07` or ST-terminated) and classifies the
leading rune of each title:

- leading rune in the braille block (U+2800–U+28FF) → **busy**
- leading rune U+2733 (`✳`) → **idle**

The turn fires on the first busy→idle transition. Idle-before-any-busy is
ignored (it corresponds to the pre-input prompt). The detector is
swappable via `pty.Options.PromptDetector`.

## License

Apache 2.0. See `LICENSE`.
