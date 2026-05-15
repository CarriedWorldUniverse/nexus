# casket-go

[![CI](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-go?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-go/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/casket-go.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/casket-go)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/casket-go)](https://github.com/CarriedWorldUniverse/casket-go/blob/main/LICENSE)

Go port of `casket-ts` channel.ts — Ed25519 + ECDH channel identity for Frame-to-Frame relay.

**Source:** [github.com/CarriedWorldUniverse/casket-go](https://github.com/CarriedWorldUniverse/casket-go)

## Sibling implementations

- [casket-ts](casket-ts.md) — Node.js + Cloudflare Workers
- [casket-dotnet](casket-dotnet.md) — .NET

All three speak the same wire format. The Go port is what [interchange](interchange.md) consumes for its E2E layer.

## Install

```go
import "github.com/CarriedWorldUniverse/casket-go"
```

`go get github.com/CarriedWorldUniverse/casket-go@v0.1.0` — pulled from the Go module proxy.

## What it covers

- AES-256-GCM and ChaCha20-Poly1305 with Argon2id key derivation
- Ed25519 channel identity
- Wire-compatible with the TS + .NET ports
