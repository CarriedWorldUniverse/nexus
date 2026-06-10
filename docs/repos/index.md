# Repos

The CarriedWorldUniverse stack across ~22 public repos. Most have CI matrices, tag-driven releases, branch-protected trunk with required status checks, and an Apache-2.0 license (`cairn` and `lynxai` are AGPL-3.0).

!!! note "These pages mirror each repo's README"
    Every page under **Repos** is **generated from that repo's live `README.md`** at docs-build time by [`scripts/sync-repo-readmes.sh`](https://github.com/CarriedWorldUniverse/nexus/blob/main/scripts/sync-repo-readmes.sh). The repo's README is the single source of truth — **edit the README in the repo, not the page here.** If a repo's README can't be fetched at build time, the last-committed copy is served as a fallback.

## The agent runtime

| Repo | Role | Lang |
|---|---|---|
| [nexus](nexus.md) | Broker + dashboard + aspect runtime + dispatch fabric (the heart) | Go |
| [bridle](bridle.md) | Per-turn deliberation library; direct-API and headless-CLI providers | Go |
| [agora](agora.md) | Operator's terminal TUI — live 1:1 with one agent | Go |
| [acp-claude-pty](acp-claude-pty.md) | PTY driver + ACP server for the Claude CLI | Go |

## The CWB platform

| Repo | Role | Lang |
|---|---|---|
| [interchange](interchange.md) | Public boundary gateway for the gRPC pillars + E2E relay | Go |
| [herald](herald.md) | Identity service for humans and agents; OIDC + casket-rooted | Go |
| [ledger](ledger.md) | Aspect-first issue tracker / work + audit pillar | Go |
| [commonplace](commonplace.md) | Knowledge pillar — agent-accessible semantic store | Go |
| [cairn](cairn.md) | Agent-native git platform — native go-git core (AGPL-3.0) | Go |
| [custodian](custodian.md) | External-credential vault — herald-keyed, brokered use (design) | Go |

## CWB clients + protocol

| Repo | Role | Lang |
|---|---|---|
| [cw](cw.md) | Platform CLI for humans + agents, anchored on the interchange edge | Go |
| [cwb-client](cwb-client.md) | Reusable Go client for the pillars, extracted from `cw` | Go |
| [cwb-proto](cwb-proto.md) | Shared protobuf/gRPC wire contracts for the mesh | proto/Go |
| [cwb-conformance](cwb-conformance.md) | External end-to-end conformance suite | Go |

## Distribution + products

| Repo | Role | Lang |
|---|---|---|
| [nexus-platform](nexus-platform.md) | Umbrella distribution repo for the deployable bundle | Go |
| [porter](porter.md) | casket-encrypted cloud-storage-as-a-filesystem (design) | Go |
| [lynxai](lynxai.md) | Self-hostable AI-native headless browser (AGPL-3.0) | Go |
| [vessel](vessel.md) | Avatar + voice shell for LLM backends | Swift/TS |

## Crypto

| Repo | Role | Lang |
|---|---|---|
| [casket-go](casket-go.md) | Ed25519 + AEAD channel identity | Go |
| [casket-ts](casket-ts.md) | Same, Node / Cloudflare Workers, wire-compatible | TypeScript |
| [casket-dotnet](casket-dotnet.md) | Same, .NET, wire-compatible | C# |

---

## CI + releases (live)

| Repo | Build | Latest |
|---|---|---|
| nexus | [![nexus CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml) | [![nexus release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases) |
| bridle | [![bridle CI](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml) | [![bridle release](https://img.shields.io/github/v/release/CarriedWorldUniverse/bridle?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/bridle/releases) |
| agora | [![agora CI](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml) | [![agora release](https://img.shields.io/github/v/release/CarriedWorldUniverse/agora?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/agora/releases) |
| acp-claude-pty | [![acp-claude-pty CI](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml) | [![acp-claude-pty release](https://img.shields.io/github/v/release/CarriedWorldUniverse/acp-claude-pty?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/acp-claude-pty/releases) |
| interchange | [![interchange CI](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml) | [![interchange release](https://img.shields.io/github/v/release/CarriedWorldUniverse/interchange?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/interchange/releases) |
| herald | [![herald CI](https://github.com/CarriedWorldUniverse/herald/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/herald/actions/workflows/ci.yml) | [![herald release](https://img.shields.io/github/v/release/CarriedWorldUniverse/herald?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/herald/releases) |
| ledger | [![ledger CI](https://github.com/CarriedWorldUniverse/ledger/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/ledger/actions/workflows/ci.yml) | [![ledger release](https://img.shields.io/github/v/release/CarriedWorldUniverse/ledger?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/ledger/releases) |
| commonplace | [![commonplace CI](https://github.com/CarriedWorldUniverse/commonplace/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/commonplace/actions/workflows/ci.yml) | [![commonplace release](https://img.shields.io/github/v/release/CarriedWorldUniverse/commonplace?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/commonplace/releases) |
| cairn | [![cairn license](https://img.shields.io/github/license/CarriedWorldUniverse/cairn)](https://github.com/CarriedWorldUniverse/cairn/blob/cairn/LICENSE) | [![cairn release](https://img.shields.io/github/v/release/CarriedWorldUniverse/cairn?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/cairn/releases) |
| custodian | [![custodian CI](https://github.com/CarriedWorldUniverse/custodian/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/custodian/actions/workflows/ci.yml) | [![custodian release](https://img.shields.io/github/v/release/CarriedWorldUniverse/custodian?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/custodian/releases) |
| cw | [![cw CI](https://github.com/CarriedWorldUniverse/cw/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/cw/actions/workflows/ci.yml) | [![cw release](https://img.shields.io/github/v/release/CarriedWorldUniverse/cw?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/cw/releases) |
| cwb-client | [![cwb-client CI](https://github.com/CarriedWorldUniverse/cwb-client/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/cwb-client/actions/workflows/ci.yml) | [![cwb-client release](https://img.shields.io/github/v/release/CarriedWorldUniverse/cwb-client?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/cwb-client/releases) |
| cwb-proto | [![cwb-proto CI](https://github.com/CarriedWorldUniverse/cwb-proto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/cwb-proto/actions/workflows/ci.yml) | [![cwb-proto release](https://img.shields.io/github/v/release/CarriedWorldUniverse/cwb-proto?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/cwb-proto/releases) |
| cwb-conformance | [![cwb-conformance CI](https://github.com/CarriedWorldUniverse/cwb-conformance/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/cwb-conformance/actions/workflows/ci.yml) | [![cwb-conformance release](https://img.shields.io/github/v/release/CarriedWorldUniverse/cwb-conformance?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/cwb-conformance/releases) |
| nexus-platform | [![nexus-platform CI](https://github.com/CarriedWorldUniverse/nexus-platform/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus-platform/actions/workflows/ci.yml) | [![nexus-platform release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus-platform?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus-platform/releases) |
| porter | [![porter CI](https://github.com/CarriedWorldUniverse/porter/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/porter/actions/workflows/ci.yml) | [![porter release](https://img.shields.io/github/v/release/CarriedWorldUniverse/porter?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/porter/releases) |
| lynxai | [![lynxai CI](https://github.com/CarriedWorldUniverse/lynxai/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/lynxai/actions/workflows/ci.yml) | [![lynxai release](https://img.shields.io/github/v/release/CarriedWorldUniverse/lynxai?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/lynxai/releases) |
| vessel | [![vessel CI](https://github.com/CarriedWorldUniverse/vessel/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/vessel/actions/workflows/ci.yml) | [![vessel release](https://img.shields.io/github/v/release/CarriedWorldUniverse/vessel?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/vessel/releases) |
| casket-go | [![casket-go CI](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml) | [![casket-go release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-go?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-go/releases) |
| casket-ts | [![casket-ts CI](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml) | [![casket-ts release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-ts?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-ts/releases) |
| casket-dotnet | [![casket-dotnet CI](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml) | [![casket-dotnet release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-dotnet?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-dotnet/releases) |

---

Per-repo details: pick a name from the left nav. Each page is the repo's own README.
