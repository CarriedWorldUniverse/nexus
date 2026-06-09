# Agent Skills System — Dev Lifecycle Skill Set (Design)

**Date:** 2026-06-09
**Status:** Design — pending review

## Goal

Give every nexus agent a single, canonical, load-on-demand skill set encoding how we build here — the development lifecycle (spec → planning → development → review → merge → release) plus the cross-cutting concerns (house-style, security). One source of truth, runtime-appropriate delivery, so the codex builders, shadow (Claude), and the API-backed aspects (gemma/deepseek) all operate to the same protocol instead of relying on a dev-standards block pasted transiently into each dispatch brief.

## Why now

- **shadow** has superpowers (Claude Skill tool); the **builders** (codex) and **API aspects** (gemma/deepseek) carry no how-to-build discipline — it lives only in the per-dispatch boilerplate (`reference_dev_standards`).
- The aspect homes (`agents/<name>/` NEXUS/SOUL/PRIMER) establish identity + platform context but ~no build discipline.
- Best practice (2025–26) separates **Skills** (SKILL.md, progressive disclosure, native to codex-cli/Claude Code/Gemini CLI) from **MCP** (system access). Pinning skills behind MCP for *everyone* is the context-bloat anti-pattern; native SKILL.md is the canonical path where the runtime supports it.

## Encoding decision (resolved)

**One `SKILL.md` source of truth; two delivery paths matched to runtime capability.**

| Runtime | Delivery | Why |
|---|---|---|
| CLI agents — codex builders, claude-code shadow | **native SKILL.md** loading | the runtimes load Agent Skills natively (progressive disclosure, ~30–50 tokens/skill idle) |
| API agents — gemma 4, deepseek (via funnel) | **skills-MCP** (`search_skills`/`get_skill`) | no disk/native-skill loading; both models tool-call reliably (verified — see below) so they pull skills on demand |

**Empirical basis (don't assume — tested):** the deployed gemma is **Gemma 4 12B** (ollama). A live tool-calling probe returned a clean `search_skills({"query":"software testing"})` — so gemma uses the MCP path like deepseek; **no injection fallback is needed.** Codex CLI *can* drive custom providers but DeepSeek needs a Responses↔Chat gateway and small models are excluded from Codex's reliable-tool-calling list — so we do **not** run gemma/deepseek through a CLI; they stay API-via-funnel and get skills over MCP.

**Relationship to superpowers:** the 9 skills are the **canonical platform set**. We port superpowers' proven content (brainstorm-before-build, TDD, verify-before-complete, systematic-debugging) into them and add the platform specifics. superpowers is the upstream we mine, not a runtime dependency.

## The skill set (9 for v1)

Each skill = ported superpowers principle(s) + platform-specific protocol. Cross-cutting skills are referenced by the phase skills (e.g. `development` and `review` both say "load `security`"). `workflow-basics` is the entry/meta skill — loaded first; it's what the always-on pointer points at and it directs to the rest. It encodes the how-to-operate baseline that Claude Code/superpowers assume but the codex builders and gemma/deepseek aspects don't otherwise have.

| # | Skill | Ported from superpowers | Platform-specific |
|---|---|---|---|
| ★ | **workflow-basics** (entry/meta) | using-superpowers (skill-discovery discipline) + the operating loop | **load first.** How to find + use `nexus-skills` (search → load the phase skill, progressive disclosure); understand → plan → act → verify; decompose + track work, one thing at a time; read-before-edit, prefer the dedicated tool, don't act on grep alone (read the file:line); the grounding discipline — verify before claiming, **test don't assume**, no manufactured narrative, ground time/state; operator-as-peer, ask only when genuinely blocked |
| 1 | **spec** | brainstorming (design-before-build, clarify one-at-a-time, 2–3 approaches, write the doc) | operator-as-peer; spec doc location/format (`docs/`); skip the visual companion (CLI-first) |
| 2 | **planning** | writing-plans (bite-sized TDD tasks, exact paths, no placeholders, self-review) | decomposition into NEX tickets; plan doc location |
| 3 | **development** | TDD + systematic-debugging (test-first; test *design*; the flake rule — generous timeouts, NEX-519/537) | dispatch model; per-agent home; `cw setup-git`; single-ticket, branch-off-main, no dead code, rebase; **frontend → Playwright-verify; `go build`/`go test` green**; live-provider/L2 tests |
| 4 | **review** | requesting/receiving-code-review (confidence-based, adversarial-verify before reporting) | PR review + the code-reviewer; **enforce the `security` scan gate**; single-ticket; read file:line, don't act on grep |
| 5 | **merge** | finishing-a-development-branch | CI-green-before-merge (**incl. security scans**), squash + delete-branch, **never `--admin`**, flake-rerun, merge authority |
| 6 | **release** | verification-before-completion | build → install `/usr/local/bin/nexus` → rollout → dMon live test; env-health; dogfood; ticket → Done |
| 7 | **house-style** (cross-cutting) | elements-of-style writing-clearly-and-concisely | Go idioms; **no-build Preact+htm** dashboard (`window.__preact`); match surrounding comment density/naming; commit `Co-Authored-By` trailer; **prose/grammar pillar** (clear, correct, "don't call out removed things", no decorative filler) across specs/PRs/commits/comments/chat |
| 8 | **security** (cross-cutting) | (new — adversarial-verify lens) | **never print/commit secrets** (keyfile sealing, stdin); **brokered-not-raw creds** (herald/CWB, identity-derived authz, `mcp_profile` lazy); **escape LLM inputs** (`jira.go` `url.PathEscape`); capability-URL pattern; dual-use posture. **Scanning toolchain (gated at review + merge):** `govulncheck` (deps/reachable CVEs), `gosec` (SAST), `gitleaks`/`trufflehog` (secret-scan on diff), `osv-scanner` (deps); DOMPurify/XSS note for the dashboard; how to triage (reachable vs not, suppress-with-justification vs fix) |

**Deferred to v1.1:** `coordination` (comms/dispatch craft — thread discipline, `reply_to`, dispatch boilerplate, no-cancel). More orchestration than build; add once the 9 are proven.

## Components

### 1. SKILL.md store (source of truth)
- `skills/<name>/SKILL.md` in the repo (Agent Skills format). Frontmatter: `name`, `description` (the progressive-disclosure hook), `when-to-use`. Body: the protocol. Supporting files (scripts/refs) alongside if a skill needs them.
- Reviewable, diffable, version-controlled. Authored in plain, unambiguous prose (must be followable by gemma 4, not just frontier models).

### 2. `nexus-skills-mcp` server
- New `runtime/cmd/nexus-skills-mcp/` (main.go + tools.go), **mark3labs/mcp-go**, following the existing `nexus-*-mcp` pattern (issue/github/imap/jira/comms).
- Tools: `search_skills(query) → [{name, description}]` (progressive disclosure) and `get_skill(name) → SKILL.md body`. Reads the same `skills/` dir (embedded via `go:embed`, or mounted) — no second copy.
- Stateless, read-only; no creds needed beyond connection.

### 3. Native loading (CLI agents)
- codex builders + claude-code shadow load `skills/` natively. For codex, this is the codex-cli skills mechanism; for shadow, the same dir (or a plugin) it already uses for superpowers.

### 4. Discovery pointer (the only always-on cost)
- One line in the **central NEXUS.md** (assembled into every aspect's boot prompt): *"At the start of any task, load the `workflow-basics` skill from `nexus-skills`; it tells you how to find and use the rest."* The pointer names the entry skill, not each phase — `workflow-basics` carries the discovery discipline. Bodies stay load-on-demand.

### 5. Wiring + retirement
- Add `nexus-skills` to the aspects' `mcp_profile` (the API aspects) — `nexus/credentials/mcp_profile.go` + the admin endpoint pattern.
- Dispatch briefs stop pasting the dev-standards block; they say "load the `development` skill." Retire `reference_dev_standards`'s pasted block (the content moves into the `development`/`security`/`house-style` skills).

## Data flow

- **Builder (codex), dispatched a ticket:** native-loads `development` (→ which references `security`, `house-style`); works; self-loads `review` for self-review before opening the PR.
- **shadow (orchestrator):** loads `spec` → `planning` → (dispatch) → `review` → `merge` → `release` as it moves through the lifecycle.
- **API aspect (gemma/deepseek):** `search_skills` → `get_skill` over MCP when its work hits a phase.

## Testing

- **MCP server:** unit tests for `search_skills`/`get_skill` (mark3labs test harness, like the existing servers); `go build ./...` green.
- **Skill content:** lint frontmatter (name/description present); a smoke test that every `description` is non-empty and every cross-reference (`load security`) names a real skill.
- **Live tool-calling:** the gemma-4 + deepseek probes (gemma verified this session) — re-runnable as a conformance check.
- **No regression:** existing aspects boot with the one-line NEXUS.md pointer added.

## Decomposition (sub-projects, each its own plan)

1. **`nexus-skills-mcp` server + the SKILL.md format/store** (the runtime + 1–2 stub skills to prove the loop end-to-end across a CLI agent + an MCP tool-call).
2. **Author the 9 SKILL.md skills** (`workflow-basics` + 6 lifecycle + house-style + security; port superpowers + platform specifics; plain enough for gemma).
3. **Wire-in + retire:** `mcp_profile` add, the central NEXUS.md pointer, retire the pasted dev-standards, point dispatch briefs at the skill.
4. **(If not already present) the security scan toolchain in CI** — verify whether `govulncheck`/`gosec`/secret-scan run in nexus CI; if not, add them so the `merge` gate is real.

## Resolved decisions

1. **Load-on-demand, all agents** — not always-on baked content (token cost) nor shadow-only.
2. **One SKILL.md source; two delivery paths** — native for CLI agents, skills-MCP for API agents. No gemma-injection path (gemma 4 tool-calls — tested).
3. **Don't run gemma/deepseek through a CLI** — heavier, deepseek needs a gateway, no benefit over MCP.
4. **Canonical set** — port superpowers' proven content; superpowers is upstream, not a runtime dep.
5. **9 skills v1** (`workflow-basics` entry/meta + 6 lifecycle + house-style + security); `workflow-basics` is the entry point the always-on pointer names (how-to-operate baseline + the grounding discipline, ported from Claude Code/superpowers); vuln/SAST/secret scanning is the `security` toolchain enforced at review+merge; grammar/writing is an explicit `house-style` pillar; `coordination` deferred to v1.1.
6. **MCP built on mark3labs/mcp-go** following the in-repo `nexus-*-mcp` pattern.

## Open items to confirm at plan time (not blocking the spec)

- Exactly how codex-cli discovers a project `skills/` dir (path/config) — confirm against codex docs so native loading actually fires for the builders.
- Whether nexus CI already runs any security scanners (sub-project 4 scope).
- `mcp_profile` add-a-server mechanics (the admin endpoint shape).
