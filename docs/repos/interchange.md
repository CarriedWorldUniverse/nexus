# interchange

[![CI](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/interchange?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/interchange/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/interchange.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/interchange)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/interchange)](https://github.com/CarriedWorldUniverse/interchange/blob/main/LICENSE)

Shared E2E-encrypted relay for Nexus Frame-to-Frame communication.

**Source:** [github.com/CarriedWorldUniverse/interchange](https://github.com/CarriedWorldUniverse/interchange)

## What it does

A small Go server that relays signed, end-to-end encrypted envelopes between paired Nexus instances. It cannot read message content — it only routes ciphertext between the two ends of a pair, gates pair establishment behind operator approval, and evicts old envelopes after a retention window.

Topology-opaque: each paired Nexus PUT/GETs its own mailbox; content is AEAD-encrypted end-to-end via [casket-go](casket-go.md).

## Binaries

| Binary | Role |
|---|---|
| `interchange` | The relay server |
| `db-inspect` | Admin tool for the SQLite state DB |

## Install

```sh
curl -L https://github.com/CarriedWorldUniverse/interchange/releases/download/v0.1.0/interchange_v0.1.0_linux_amd64.tar.gz | tar xz
./interchange --version
./interchange --id <owner-nexus-id>
```

Linux + macOS + Windows × amd64 + arm64.

## Storage

Pure-Go SQLite (`modernc.org/sqlite`) at `interchange.db` by default. No CGO required.

## Where to dig deeper

- [AWS bootstrap spec](../archive/2026-05-01-aws-bootstrap-spec.md) — production deployment shape
- [Nexus transport spec](../archive/2026-04-25-nexus-transport-spec.md)
