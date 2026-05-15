# Repos

The Nexus stack across 8 public CarriedWorldUniverse repos. Each has CI on every push, tag-driven releases via goreleaser (where applicable), branch-protected main, and a v0.1.0 baseline.

## At a glance

| Repo | Role | Language | Binaries |
|---|---|---|---|
| [nexus](nexus.md) | Broker + dashboard + 8 binaries | Go | nexus, agentfunnel, aspect, nexus-comms-mcp, nexus-imap-mcp, nexus-jira-mcp, nexus-watch, outpost |
| [bridle](bridle.md) | Per-turn deliberation library | Go | (library; stubfunnel for dev) |
| [agora](agora.md) | Operator TUI | Go | agora |
| [acp-claude-pty](acp-claude-pty.md) | PTY driver + ACP server for `claude` CLI | Go | acp-claude-pty |
| [interchange](interchange.md) | E2E-encrypted Frame-to-Frame relay | Go | interchange, db-inspect |
| [cairn](cairn.md) | Agent-native git platform; long-term divergent fork of Forgejo (AGPL-3.0) | Go | (binaries TBD) |
| [casket-go](casket-go.md) | Ed25519 + AEAD channel identity (Go) | Go | (library) |
| [casket-ts](casket-ts.md) | Same, Node / Cloudflare Workers | TypeScript | (library) |
| [casket-dotnet](casket-dotnet.md) | Same, .NET | C# | (library) |

## Release matrix

Per-repo release artifact coverage. Each cell links to the v0.1.0 archive on GitHub Releases. Matrix updated manually per release until the auto-update workflow lands.

| Repo | linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64 | windows/amd64 | windows/arm64 |
|---|---|---|---|---|---|---|
| **nexus** | ✓ (8 bin) | ✓ | ✓ | ✓ | ✓ (v0.1.1+) | ✓ (v0.1.1+) |
| **bridle** | (library, module proxy only) | | | | | |
| **agora** | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| **acp-claude-pty** | ✓ | ✓ | ✓ | ✓ | — | — |
| **interchange** | ✓ (2 bin) | ✓ | ✓ | ✓ | ✓ | ✓ |
| **casket-go** | (library, module proxy only) | | | | | |
| **casket-ts** | (library, npm proxy; nothing in releases today) | | | | | |
| **casket-dotnet** | (library, nuget proxy; nothing in releases today) | | | | | |

For binary repos: each row is a tarball at `https://github.com/CarriedWorldUniverse/<repo>/releases/download/v0.1.0/<binary>_<version>_<os>_<arch>.tar.gz`. Library repos are pulled by their language's standard package manager — there's nothing to download manually.

## CI status (live)

| Repo | Build status |
|---|---|
| nexus | [![nexus CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml) |
| bridle | [![bridle CI](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/bridle/actions/workflows/ci.yml) |
| agora | [![agora CI](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/agora/actions/workflows/ci.yml) |
| acp-claude-pty | [![acp-claude-pty CI](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/acp-claude-pty/actions/workflows/ci.yml) |
| interchange | [![interchange CI](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/interchange/actions/workflows/ci.yml) |
| casket-go | [![casket-go CI](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-go/actions/workflows/ci.yml) |
| casket-ts | [![casket-ts CI](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-ts/actions/workflows/ci.yml) |
| casket-dotnet | [![casket-dotnet CI](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/casket-dotnet/actions/workflows/ci.yml) |

## Latest releases

| Repo | Version |
|---|---|
| nexus | [![nexus release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases) |
| bridle | [![bridle release](https://img.shields.io/github/v/release/CarriedWorldUniverse/bridle?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/bridle/releases) |
| agora | [![agora release](https://img.shields.io/github/v/release/CarriedWorldUniverse/agora?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/agora/releases) |
| acp-claude-pty | [![acp-claude-pty release](https://img.shields.io/github/v/release/CarriedWorldUniverse/acp-claude-pty?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/acp-claude-pty/releases) |
| interchange | [![interchange release](https://img.shields.io/github/v/release/CarriedWorldUniverse/interchange?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/interchange/releases) |
| casket-go | [![casket-go release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-go?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-go/releases) |
| casket-ts | [![casket-ts release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-ts?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-ts/releases) |
| casket-dotnet | [![casket-dotnet release](https://img.shields.io/github/v/release/CarriedWorldUniverse/casket-dotnet?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/casket-dotnet/releases) |

---

Per-repo details: pick a name from the left nav or click a row.
