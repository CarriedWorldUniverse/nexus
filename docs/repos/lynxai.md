<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`lynxai`](https://github.com/CarriedWorldUniverse/lynxai)'s live `README.md`.
    Edit the README in the repo, not this page.

# lynxai

> Self-hostable, AI-native headless browser. The access layer for AI agents in tools where the only door is a human's browser session.

[![License: AGPL-3.0-or-later](https://img.shields.io/badge/License-AGPL%20v3%2B-blue.svg)](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/LICENSE)

`lynxai` is a free alternative to hosted browser infrastructure (Browserbase, etc.) for AI agents. It runs as a small HTTP server you self-host, with an encrypted credential vault for the sites your agent needs to access, and LLM-driven extraction so agents get clean JSON instead of HTML.

## Why

Most AI agent integrations stop at "what has an API or MCP." The human developer has a much larger surface — every SaaS tool they log into. lynxai opens that surface to the agent: store the credentials once, fetch and extract on demand.

The bootstrapping case is the most interesting: an agent uses lynxai's future `drive` endpoint to obtain an API key from a vendor's UI, lynxai stores the key, and from then on the agent calls the vendor's API directly. The expensive browser-driven bootstrap runs once; cheap API calls run forever after.

See [`docs/superpowers/specs/2026-05-22-lynxai-v1-design.md`](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/superpowers/specs/2026-05-22-lynxai-v1-design.md) for the full design.

## Quickstart (Docker)

```bash
docker run -d -p 7878:7878 \
  -e LYNXAI_LLM_API_KEY=sk-...  \
  -v lynxai-data:/data \
  ghcr.io/carriedworlduniverse/lynxai:latest
```

The default LLM provider is DeepSeek (cheap, OpenAI-API-compatible). Drop in a DeepSeek API key and you're running.

## Quickstart (binary)

```bash
go install github.com/CarriedWorldUniverse/lynxai/cmd/lynxai@latest
export LYNXAI_LLM_API_KEY=sk-...
lynxai serve --addr 127.0.0.1:7878
```

You'll need Chromium installed on PATH (or available in the default chromedp lookup locations).

## API (v1)

Two endpoints do the work:

```bash
# Fetch — page as cleaned markdown
curl -X POST http://localhost:7878/fetch \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'

# Extract — LLM-driven structured extraction
curl -X POST http://localhost:7878/extract \
  -H 'Content-Type: application/json' \
  -d '{
    "url": "https://news.ycombinator.com",
    "schema": {
      "type": "object",
      "properties": {
        "stories": {
          "type": "array",
          "items": {"type":"object","properties":{"title":{"type":"string"},"url":{"type":"string"}}}
        }
      }
    }
  }'
```

Plus credential CRUD:

```bash
# Store a bearer token, scoped to a host
curl -X POST http://localhost:7878/credentials \
  -d '{"name":"github-mine","kind":"bearer","host":"api.github.com","bundle":{"host":"api.github.com","token":"ghp_..."}}'

# Use it
curl -X POST http://localhost:7878/fetch \
  -d '{"url":"https://api.github.com/user","credential":{"name":"github-mine"}}'
```

Supported credential kinds (v1): `basic`, `bearer`, `cookies`, `form`.

## Security

- The API has **no built-in authentication.** Bind to loopback (default) or put it behind your own reverse proxy.
- Credentials are stored encrypted at rest (AES-256-GCM, HKDF-derived from a `master.key` file with 0600 perms).
- Bundle data never leaves the server: clients reference credentials by name only on `/fetch` and `/extract`.
- Every credential use writes an audit row (name, request URL, outcome) — bundle contents are never logged.

## Documentation

For self-hosters and integrators:

- [Quickstart](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/usage/quickstart.md) — install, first request, first extract
- [API Reference](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/usage/api.md) — all endpoints, schemas, error codes
- [Credentials](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/usage/credentials.md) — the four credential kinds, bundle shapes, audit log
- [Operations](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/usage/operations.md) — flags, env vars, backup, security stance, limits

For contributors / curious readers:

- [Design Spec](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/superpowers/specs/2026-05-22-lynxai-v1-design.md)
- [v1 Implementation Plan](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/superpowers/plans/2026-05-22-lynxai-v1-implementation.md)

## License

AGPL-3.0-or-later. See [`LICENSE`](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/LICENSE).

If you fork lynxai and run it as a network-accessible service, you must publish your source. See [the license rationale in the spec](https://github.com/CarriedWorldUniverse/lynxai/blob/HEAD/docs/superpowers/specs/2026-05-22-lynxai-v1-design.md#license-rationale-agpl-30-or-later).

## Related

- [`bridle`](https://github.com/CarriedWorldUniverse/bridle) — the Go LLM harness lynxai uses for the `extract` endpoint
- [lynx](https://lynx.invisible-island.net/) — the 25-year-old text browser whose `-dump` mode is lynxai's design ancestor. We maintain a separate fork for upstream contributions (bugs/security patches).
