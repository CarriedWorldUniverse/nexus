<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/casket-ts/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`casket-ts`](https://github.com/CarriedWorldUniverse/casket-ts)'s live `README.md`.
    Edit the README in the repo, not this page.

# casket-ts

[![CI](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-ts?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-ts/releases)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/casket-ts)](https://github.com/CarriedWorldUniverse/casket-ts/blob/HEAD/LICENSE)

Small, easy-to-use authenticated encryption library for TypeScript / Node.js / Cloudflare Workers.

- **AES-256-GCM** and **ChaCha20-Poly1305** with Argon2id key derivation
- **Channel** module: Ed25519 identity + dual-curve ECDH (P-256 / X25519) for E2E encryption — pair-relay channels and at-rest envelopes
- Cross-compatible channel wire format with [casket-go](https://github.com/CarriedWorldUniverse/casket-go) (Go) and [casket-dotnet](https://github.com/CarriedWorldUniverse/casket-dotnet) (.NET)
- Runs on Node.js (>=18) and Cloudflare Workers

## Install

```
npm install @nexus-cw/casket
```

## Usage

```ts
import { sealWithPassword, unsealWithPassword } from '@nexus-cw/casket';

const token = await sealWithPassword('secret message', 'correct horse battery staple');
const plaintext = await unsealWithPassword(token, 'correct horse battery staple');
```

Key-based sealing (synchronous, raw key) is also available:

```ts
import { generateKey, sealWithKey, unsealWithKey, keySourceFromBuffer } from '@nexus-cw/casket';

const key = generateKey();
const source = keySourceFromBuffer(Buffer.from(key, 'base64url'));
const token = sealWithKey('secret message', source);
const plaintext = unsealWithKey(token, source);
```

## Develop

```
npm install
npm run build
npm test
```

## License

Apache-2.0. See [LICENSE](https://github.com/CarriedWorldUniverse/casket-ts/blob/HEAD/LICENSE).
