# casket-ts

[![CI](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-ts?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-ts/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/casket-ts)](https://github.com/CarriedWorldUniverse/casket-ts/blob/main/LICENSE)

Authenticated encryption + Ed25519 channel identity for TypeScript / Node.js / Cloudflare Workers.

**Source:** [github.com/CarriedWorldUniverse/casket-ts](https://github.com/CarriedWorldUniverse/casket-ts)

## Sibling implementations

- [casket-go](casket-go.md) — Go (used by interchange)
- [casket-dotnet](casket-dotnet.md) — .NET

Wire-compatible across all three.

## What it covers

- AES-256-GCM and ChaCha20-Poly1305 with Argon2id key derivation
- Ed25519 channel identity
- Runs in Node and Cloudflare Workers — no native bindings

## Install

```sh
# Not yet published to npm; consume directly from the repo:
npm install github:CarriedWorldUniverse/casket-ts
```

(npm publishing on tag is a follow-up — see release workflow.)
