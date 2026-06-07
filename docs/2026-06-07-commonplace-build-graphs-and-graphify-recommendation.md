# Commonplace build graphs + Graphify — recommendation

**Date:** 2026-06-07
**Status:** recommendation / design seed
**Scope:** how to use Graphify-style knowledge graphs to help on-demand builder agents understand what they are building. This is not a visualization proposal.

## One-liner

Use Graphify as a useful reference and optional extractor, but make **Commonplace** the authoritative build-knowledge graph: a compact, provenance-aware working memory that tells agents what exists, why it exists, what is being built, what depends on it, and what changed.

## Problem

On-demand builders such as plumb/anvil need to start work with enough context to avoid rediscovering the same decisions every run. A normal repo checkout gives them files, but not the operational shape:

- what the operator is trying to build
- which decisions are already settled
- which components own which behavior
- which specs, issues, commits, and conversations explain the current design
- which files and services are likely relevant
- what open questions or constraints must not be lost

Graph visualization is not the important part. The value is a queryable **build graph** that can produce a concise build packet for an agent before it touches code.

## Recommendation

Build a Commonplace-native graph layer and ingest Graphify-style outputs as one input source.

Graphify should not own the Carried World knowledge model. It is a good reference implementation for:

- deterministic AST extraction before LLM semantic extraction
- graph-shaped `nodes`/`edges` output
- confidence labels such as `EXTRACTED`, `INFERRED`, and `AMBIGUOUS`
- cached/delta extraction
- query and affected-node traversal
- assistant skill delivery patterns across Codex, Claude Code, Cursor, Gemini CLI, and related tools

Commonplace should own:

- platform vocabulary
- provenance and authority
- cross-repo/system memory
- agent build packets
- long-lived facts and decisions
- conflict handling between code, docs, live cloud state, issues, and conversation history

## Target graph families

### 1. Repo graph

Code, docs, config, manifests, and module topology.

Example nodes:

- repo
- package
- module
- file
- function
- type
- config
- deployment manifest
- test

Example edges:

- imports
- calls
- defines
- tests
- configures
- deploys
- depends_on

Primary extractors: Graphify-style AST extraction, language-specific parsers, Git metadata, CI metadata.

### 2. Build graph

The graph an agent uses to understand what it is building.

Example nodes:

- objective
- requirement
- decision
- component
- interface
- task
- issue
- risk
- open_question
- implementation_slice
- build_state

Example edges:

- implements
- blocks
- supersedes
- derived_from
- decided_by
- owned_by
- changed_by
- tested_by
- documents

Primary extractors: specs, issue tracker, commits, PRs, conversation summaries, dispatch traces.

### 3. System graph

The Carried World platform map across repos and services.

Example nodes:

- service
- aspect
- model
- endpoint
- credential boundary
- auth authority
- runtime host
- namespace

Example edges:

- authenticates_with
- connects_to
- consumes
- exposes
- runs_on
- stores_in
- relays_to
- authorizes

Primary extractors: `carriedworld-cloud`, live k3s snapshots, Nexus roster, Herald identity, Custodian credential metadata, docs.

### 4. Agent graph

The working relationships among aspects, tools, permissions, dispatch paths, and character/role.

Example nodes:

- aspect
- role
- keyfile
- tool
- capability
- model_profile
- dispatch_policy
- conversation

Example edges:

- can_use
- may_request
- delegates_to
- reports_to
- speaks_for
- muted_by
- has_credential_boundary

Primary extractors: Nexus agent configs, Herald/Custodian metadata, dispatch traces, Vessel roster/state.

## Commonplace graph record

Commonplace records should preserve both the graph relation and the evidence behind it.

```text
node
  id
  type
  label
  summary
  source_refs[]
  authority
  confidence
  observed_at
  valid_from
  valid_to
  tags[]

edge
  id
  source
  target
  type
  summary
  source_refs[]
  authority
  confidence
  observed_at
  weight
```

### Authority

Authority is the answer to "why should an agent believe this?"

Suggested values:

- `observed` — live system or runtime state
- `extracted` — deterministic parser or API extraction
- `asserted` — operator or trusted spec statement
- `inferred` — LLM or heuristic inference
- `historical` — old state retained for continuity

### Confidence

Suggested values:

- `confirmed`
- `likely`
- `ambiguous`
- `stale`
- `contradicted`

Graphify's labels map naturally:

- `EXTRACTED` -> `authority=extracted`, `confidence=confirmed`
- `INFERRED` -> `authority=inferred`, `confidence=likely`
- `AMBIGUOUS` -> `authority=inferred`, `confidence=ambiguous`

## Source references

Every useful node or edge needs provenance.

Examples:

```text
repo: nexus
commit: abc123
file: runtime/dispatch/brief.go
line: 42
doc: docs/2026-06-07-recursive-dispatch-routing-design.md
issue: NEX-464
conversation: vessel-session-2026-06-07
cloud_snapshot: dmonextreme/k3s/2026-06-07T12:00Z
aspect: shadow
```

This is the main addition beyond Graphify. An agent must know whether a fact came from code, a spec, a live cluster, an issue, a user statement, or an AI inference.

## Agent build packet

The build graph should produce a compact packet for an on-demand builder before work starts.

```text
Build packet
  objective
  current_state
  relevant_decisions
  likely_files
  interfaces
  constraints
  dependencies
  related_prior_work
  open_questions
  risks
  suggested_first_checks
```

This packet should be generated from Commonplace, not from a raw `graph.json` dump. The agent receives a concise, scoped context and can query deeper when needed.

## Ingestion pipeline

```text
extractors
  Graphify-style repo extractor
  specs/docs extractor
  issue/ledger extractor
  git/cairn extractor
  cloud/k3s extractor
  nexus/aspect roster extractor
  dispatch trace extractor
        |
        v
normalizer
  maps external nodes/edges into Commonplace vocabulary
  attaches provenance
  resolves duplicates
  assigns authority/confidence
        |
        v
Commonplace graph store
  query
  build packet generation
  stale/conflict detection
  agent retrieval API
```

## Graphify use

Recommended use:

- keep a pinned local Graphify commit or package version for experiments
- run it against selected repos to evaluate output quality
- ingest its `graph.json` through a normalizer
- preserve Graphify's extracted/inferred/ambiguous signal
- discard or down-rank noisy edges that do not help agent work
- use its assistant skill patterns as a reference for Bridle/Codex integration

Do not:

- make Graphify the source of truth
- rely on stars/adoption as a quality signal
- install global hooks into production repos without review
- allow private code to be sent to arbitrary remote LLM providers
- expose Graphify output directly as Commonplace memory without provenance normalization

## Security and custody

This graph layer must respect Herald and Custodian boundaries:

- Commonplace may record that a credential exists and which capability it enables.
- Commonplace must not store secret material.
- Agent-visible packets should describe credential usage through Custodian, not reveal credentials.
- Graph nodes for credentials should point at Custodian handles, scopes, and policy, not values.
- Remote LLM extraction must be opt-in and clearly tagged as exfiltration-capable.

## Go binary viability

Rebuilding the useful part as a Go binary is viable and likely preferable for the Carried World platform, but the target should be a **Commonplace graph extractor**, not a full Graphify port.

Recommended binary:

```text
commonplace-graph-extract
  repo scan
  deterministic source/config extraction
  graph normalization
  provenance attachment
  Commonplace ingest API/client
  optional local-model semantic pass
```

Do not rebuild initially:

- `graph.html` visualization
- the full assistant-skill installer matrix
- video/audio transcription
- the full optional LLM provider matrix
- Neo4j/export formats
- broad media/document ingestion
- every language extractor Graphify supports

### Why Go fits

Go matches the surrounding platform shape. A single binary can run locally, in CI, or as a k3s Job/CronJob, authenticate through Herald/Nexus conventions, and write directly into Commonplace without Python environment management.

Go is a good fit for:

- deterministic batch extraction
- repo and Git/Cairn integration
- k3s/cloud-state extraction
- Commonplace API integration
- schema validation
- provenance enforcement
- static-ish deployment
- worker/job execution inside the local cloud

The main hard part is parser coverage. Graphify has broad Python tree-sitter package coverage. A Go rebuild should start narrow and cover the Carried World stack first:

- Go
- TypeScript/JavaScript
- Python
- Markdown
- YAML/JSON/TOML
- Dockerfile and shell basics
- Kubernetes manifests

That covers most Nexus, Vessel, Bridle, Cairn, Herald, Custodian, and carriedworld-cloud use. Additional languages can be added only when they appear in active repos.

### Local model use

The Go extractor should parse deterministically first and use the local model only where structure is insufficient:

- summarize docs/specs
- extract decisions and open questions
- connect concepts across files
- produce build-packet summaries
- mark ambiguous or inferred relations

Gemma can serve this path through the local cloud, but model output should enter Commonplace as `authority=inferred` unless confirmed by code, spec, operator assertion, or live state.

### Output target

The Go binary should emit Commonplace-native records, not Graphify-compatible JSON as the final form.

```json
{
  "snapshot": {
    "source": "repo",
    "repo": "nexus",
    "commit": "abc123",
    "extracted_at": "2026-06-07T00:00:00Z"
  },
  "nodes": [],
  "edges": [],
  "source_refs": []
}
```

Graphify output can still be ingested through an adapter for comparison and experiments, but the trusted pipeline should be native.

## First implementation slice

1. Define the Commonplace graph node/edge schema and source-reference model.
2. Build a small Go `commonplace-graph-extract` MVP for repo graphs.
3. Add deterministic extractors for Go, Markdown, YAML/JSON/TOML, and Kubernetes manifests.
4. Add a Graphify-output adapter only as an experimental comparison path.
5. Run both paths on `nexus`, `vessel`, and `carriedworld-cloud`.
6. Add a build-packet query for one target, such as "Vessel voice UI" or "recursive dispatch".
7. Compare the packet against what a builder actually needed.
8. Add issue/spec/conversation ingestion only after the repo graph proves useful.

## Success criteria

- A builder can start from a build packet and identify the right files/specs faster than from repo search alone.
- The packet includes decisions and constraints, not just code symbols.
- Every important assertion has a source reference.
- Stale or contradictory facts are visible instead of silently merged.
- The graph can answer "what is this agent building?" and "what should this agent not break?"

## Open questions

- Should Commonplace store graph records directly, or maintain graph views over a richer knowledge object store?
- How often should repo/cloud graphs be refreshed?
- Which graph facts should be visible to every agent, and which should be scoped by role/capability?
- Should build packets be generated by a local model first, with frontier fallback only for ambiguous synthesis?
- How should dispatch traces feed back into build graph state after each recursive build run?
