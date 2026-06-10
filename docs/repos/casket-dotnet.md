<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/casket-dotnet/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`casket-dotnet`](https://github.com/CarriedWorldUniverse/casket-dotnet)'s live `README.md`.
    Edit the README in the repo, not this page.

# Casket.Net

[![CI](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-dotnet?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-dotnet/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/casket-dotnet)](https://github.com/CarriedWorldUniverse/casket-dotnet/blob/HEAD/LICENSE)

Authenticated encryption and Ed25519 channel identity for .NET.

- **AES-256-GCM** and **ChaCha20-Poly1305** with Argon2id key derivation
- **Channel** module: Ed25519 identity + dual-curve ECDH (P-256 / X25519) for E2E encryption — pair-relay channels and at-rest envelopes
- Cross-compatible channel wire format with [`casket-ts`](https://github.com/CarriedWorldUniverse/casket-ts) (Node.js / Cloudflare Workers) and [`casket-go`](https://github.com/CarriedWorldUniverse/casket-go) (Go)
- Targets `netstandard2.1`, `net8.0`, `net9.0`, `net10.0`

## Install

```
dotnet add package Casket.Net
```

## License

MIT
