# casket-dotnet

[![CI](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-dotnet?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-dotnet/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/casket-dotnet)](https://github.com/CarriedWorldUniverse/casket-dotnet/blob/main/LICENSE)

Authenticated encryption + Ed25519 channel identity for .NET.

**Source:** [github.com/CarriedWorldUniverse/casket-dotnet](https://github.com/CarriedWorldUniverse/casket-dotnet)

## Sibling implementations

- [casket-go](casket-go.md) — Go
- [casket-ts](casket-ts.md) — Node.js + Cloudflare Workers

Wire-compatible across all three.

## What it covers

- AES-256-GCM and ChaCha20-Poly1305 with Argon2id key derivation
- Ed25519 channel identity
- Targets .NET 8.0 / 9.0 / 10.0 (per CI matrix)

## Install

NuGet publishing on tag is a follow-up. For now consume directly from the source / built NuGet package in releases.
