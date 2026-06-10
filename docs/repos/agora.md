<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/agora/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`agora`](https://github.com/CarriedWorldUniverse/agora)'s live `README.md`.
    Edit the README in the repo, not this page.

# agora

[![CI](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/agora?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/agora/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/agora)](https://github.com/CarriedWorldUniverse/agora/blob/HEAD/LICENSE)

An interactive operator-facing CLI for one-to-one conversations with always-on nexus agents.

`agora` opens a single full-screen DM thread with one agent. It holds a persistent operator WebSocket connection to nexus, loads chat history, renders pushed updates in real time, and sends operator messages using the same `dm:<agent>` convention as the dashboard.

Run it with an agent name and, when the broker is not in auth-bypass mode, a pre-minted operator JWT:

```sh
agora -agent maren -token "$AGORA_TOKEN"
```

## Status

Built and in use. The one-to-one conversation shape is live, with a client-side
heartbeat and visible connection state (#31), a turn-rhythm chat feel (#32), an
on-demand trace pane on `ctrl+t` (#33), and mouse-wheel scrolling of the session
(#34). See [`docs/spec.md`](https://github.com/CarriedWorldUniverse/agora/blob/HEAD/docs/spec.md) for the design.

## Architecture (one paragraph)

agora is a Bubble Tea TUI over `internal/opclient`. The client probes broker auth mode, connects to `/connect`, subscribes to chat and observe pushes, loads `chat.list` history, sends `chat.send` DM messages, and persists a cursor under the state directory for reconnect catch-up.

## What this is not

- **Not a replacement for claude-code.** claude-code stays the right tool for code-editing work. agora is the right tool for chat-driven coordination work. Side-by-side, not vs.
- **Not a vessel.** [`vessel`](https://github.com/CarriedWorldUniverse/vessel) is the avatar-and-voice front-end. agora is the terminal-resident text front-end. Different shapes, same direction (operator interfaces to the cluster).
- **Not an autonomous agent host.** Agents run elsewhere. agora is specifically for operator-attended conversations with those agents.

## Family

- [`nexus`](https://github.com/CarriedWorldUniverse/nexus) — the cluster substrate: broker, Frame, dispatcher, knowledge, chat, roster.
- [`cairn`](https://github.com/CarriedWorldUniverse/cairn) — repo hosting (native go-git).
- [`vessel`](https://github.com/CarriedWorldUniverse/vessel) — Tauri avatar + voice front-end to the cluster.

## License

Apache-2.0. See [LICENSE](https://github.com/CarriedWorldUniverse/agora/blob/HEAD/LICENSE).
