# cairn

[![License](https://img.shields.io/github/license/CarriedWorldUniverse/cairn)](https://github.com/CarriedWorldUniverse/cairn/blob/cairn/LICENSE)

Agent-native git platform. Long-term divergent fork of [Forgejo](https://forgejo.org/) v15.0.1.

**Source:** [github.com/CarriedWorldUniverse/cairn](https://github.com/CarriedWorldUniverse/cairn)

## What it is

Cairn is our own version of a git hosting platform, descended from Forgejo as a starting point. Where most forks aim to merge back upstream, cairn diverges deliberately — we own the fork and ship our own binaries. The Forgejo lineage is historical context, not a maintenance contract.

The "agent-native" framing is what makes it ours: features designed for AI peer collaboration on the same platform humans use, rather than retrofitted onto a human-only tool. v0.1 features live on the `cairn-markdown-*` and `ui-phase1` branches; the roadmap is captured in `docs/cairn/specs/` inside the repo.

## License

Cairn inherits Forgejo's **AGPL-3.0** license. This is the exception in CarriedWorldUniverse's stack — every other public repo is Apache-2.0.

## Status

- **Trunk:** `cairn` branch (branch-protected, PR-required, enforce_admins on).
- **Historical reference:** `forgejo` + `v15.0/forgejo` (locked, read-only).
- **Active feature branches:** `cairn-markdown-commit`, `cairn-markdown-file`, `cairn-markdown-pr-issue`, `ui`, `ui-phase1` — work in progress, will land via PR into `cairn`.
- **CI:** Forgejo upstream's `.forgejo/` workflows; a nexus-cw-specific overlay is on the roadmap (NEX-136).
- **Releases:** not yet cut. Goreleaser + tagged binaries planned for the EC2 deployment path per the AWS bootstrap spec.

## Where it runs (planned)

Per `docs/2026-05-01-aws-bootstrap-spec.md` in the nexus repo: cairn binaries (linux/arm64 for t4g.small) deploy to a single EC2 instance, joined to the operator's tailnet, with SQLite + Litestream-replicated WAL to S3. LFS / attachments / packages also S3-backed. Tailscale Funnel handles TLS for the public surface.

## Where to dig deeper

- [`docs/cairn/specs/`](https://github.com/CarriedWorldUniverse/cairn/tree/cairn/docs/cairn/specs) in-repo — design specs (identity layer, push verification, AI-native amendments, etc).
- [`docs/cairn/plans/`](https://github.com/CarriedWorldUniverse/cairn/tree/cairn/docs/cairn/plans) — implementation plans.
- NEX-136 in internal tracking — epic covering trunk hygiene, CI overlay, deployment story, agent-native roadmap.
