# Pi coding agent → bridle: extract & gap analysis

**Author:** plumb
**Date:** 2026-05-13
**Source:** https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/
**Status:** Discussion draft — wants keel + anvil review before any of this becomes work.

Pi is a minimal terminal coding harness (Claude-Code-shaped) built around an extension system, a tree-structured session model, and pluggable providers. This doc extracts what makes Pi feel like a *full* agent harness and maps each capability to a bridle gap with a recommendation.

---

## Part 1 — What makes Pi a "full agent" (10 things)

1. **Session-as-tree, not append-only log.** Every entry carries `id` + `parentId`. `/tree` walks the structure; `/fork` splits into a new file; `/clone` copies the current branch. Switching branches via `/tree` *optionally summarizes the abandoned branch* and attaches that summary at the new position, preserving context across the jump.
2. **25+ event hooks** across lifecycle / agent / tool / provider / input categories. Hooks can **mutate or block** — `tool_call` returning `{ block: true, reason }` is the canonical policy primitive. `context` event can mutate the message list before the LLM sees it.
3. **Compaction with file-tracking carry-forward.** Cut at `keepRecentTokens` (default 20k), summarize older messages, *but also* separately accumulate file-read/edit history pulled from tool calls. The file history survives across multiple compactions; the prose summary churns but the file map keeps growing.
4. **Skills as progressive-disclosure capability bundles.** Anthropic Skill spec: `SKILL.md` with name + description in frontmatter. Names + descriptions appear in the system prompt; full body is loaded on demand (implicit match, `/skill:name`, or explicit prompt). Discovered from global / project / packages / CLI paths.
5. **TS extensions register tools / commands / providers / UI / status widgets** at runtime via a factory function `(pi: ExtensionAPI) => void`. Extensions can persist private state in the session via `pi.appendEntry("my-state", ...)` that survives restart **but does not enter LLM context**.
6. **Three I/O modes:** interactive TUI, RPC (client-driven JSONL, bidirectional), JSON event-stream (output-only "print mode"). RPC is strict on `\n` delimiter — explicit warning against Node's `readline` because it splits on U+2028 / U+2029.
7. **In-flight messaging discipline.** `steer` interrupts the current turn and incorporates new input; `followUp` queues input to be delivered after the turn completes. Clients pick `streamingBehavior` per command — not an implicit policy.
8. **Provider abstraction with first-class OAuth.** `login / refreshToken / getApiKey / modifyModels` hooks. Subscription auth is a peer of API-key auth, not a bolt-on. `/login provider-name` persists to `~/.pi/agent/auth.json`.
9. **Resource-discovery event** lets one extension contribute a bundle of skill / prompt / theme paths. The "pi package" unit is a single artefact carrying multiple capability types.
10. **Branch-summary entries are distinct from compaction entries.** Session format distinguishes "I summarised because of token pressure" from "I summarised because we abandoned this branch." Future deliberation can reason about *why* a summary exists.

---

## Part 2 — Bridle gap analysis

Context: bridle is the Go library embedded in funnel for per-turn deliberation. Library-only today; no harness binary (see `project_harness_binary_open`).

Sorted roughly by leverage for nexus aspects.

### A. Tree-shaped session history

**Pi:** entries form a tree; navigation, fork, clone, branch-summary.

**Bridle gap:** unknown precisely, but assume linear append-only conversation persistence per aspect/thread.

**Why it matters for nexus:** nexus threads are conversations with multiple humans-shaped participants. The natural shape for "explore option B without losing option A" is a branch, not a new thread. **Plumb especially benefits** — option-generation is literally branching. Useful generally for any aspect doing exploratory work (harrow, anvil during design, keel during spec).

**Recommendation:** lift the `parentId` model + branch-summary entry type wholesale. Small data-shape change with high leverage downstream.

### B. Mutating event bus

**Pi:** hooks are the primary extension surface; can block tool calls, rewrite tool input, mutate provider request, modify message list pre-LLM. Mutation semantics are explicit: "event-handler mutations to `event.input` affect actual execution."

**Bridle gap:** if hooks exist today, my guess is they're observation-only.

**Why it matters:** this is where policy lives — secret-scrubbing, MCP-comms mention dispatch, per-aspect tool whitelists, dangerous-command blocks. Without mutating hooks, every policy concern becomes a fork of bridle core.

**Recommendation:** highest-leverage abstract change, but needs an ordering / conflict-resolution design pass first (what happens when two hooks both want to mutate the same field?). Spec before code.

### C. File-tracking that survives compaction

**Pi:** at compaction time, pulls file paths out of tool calls into a cumulative file-history structure attached to the compaction entry. Grows across multiple compactions.

**Bridle gap:** likely none — when context compacts, knowledge of "which files have I touched" probably dies with the summarised messages.

**Why it matters:** coding-heavy aspects (anvil, keel, harrow when researching code) lose continuity. The first time anvil compacts mid-refactor, the next turn forgets which files it's been editing.

**Recommendation:** smallest scoped win in this list. ~day of work. Ship first.

### D. Extension-private session state

**Pi:** `appendEntry("my-state", {...})` persists across restart, NOT sent to model. Custom entry type, excluded from LLM context.

**Bridle gap:** likely no such channel — aspect state is either in-context (polluting prompt) or re-derived every turn.

**Why it matters:** use cases — "which specs have I consulted this session," "running cost tally," "consult budgets," "MCP tools I've already discovered." Cheap primitive.

**Recommendation:** pure addition; no API breakage. Ship alongside C.

### E. RPC mode as the harness-binary contract

**Pi:** documents a strict JSONL bidirectional protocol with explicit message types (UserMessage / AssistantMessage / ToolResultMessage / BashExecutionMessage), client-driven turn control, and steer/followUp semantics.

**Bridle gap:** open per `project_harness_binary_open` — funnel auto-spawn expects a binary; bridle is library-only.

**Why it matters:** Pi's JSONL RPC shape **is** the harness binary wire format if you squint. If bridle grew a thin binary wrapper speaking JSONL-RPC, funnel could spawn aspects as out-of-process processes (or remote ones on dMon when heavy compute is needed). Contract is already documented and exercised in production.

**Recommendation:** don't reinvent. Build after A is at least sketched, because RPC needs to encode tree-navigation commands.

### F. Skills (progressive-disclosure spec)

**Pi:** Anthropic Skill spec adoption.

**Bridle gap:** aspects have personas; nothing below personas. Per-task capabilities like "writing-plans" or "verification-before-completion" either bake into persona prompt (context cost every turn) or don't exist.

**Why it matters:** skills are *below* personas. Composes with the resource-discovery event so a nexus tool bundle can ship a skill alongside it.

**Recommendation:** adopt the Anthropic spec; don't reinvent. Cheap.

### G. Steer vs followUp semantics

**Pi:** explicit two-mode API for in-flight input.

**Bridle gap:** unclear what bridle does when a new chat message arrives mid-turn — drop, queue, restart, ignore?

**Why it matters:** policy that's currently implicit (or absent) becomes operator-choosable per-message. Aspect doesn't have to invent the rule.

**Recommendation:** small feature; ride on top of A.

### H. Branch-summary distinct from compaction-summary

**Pi:** different entry types for "summarised due to token pressure" vs "summarised because we abandoned this branch."

**Bridle gap:** likely conflated or absent.

**Why it matters:** small ergonomic win; future deliberation can reason about why a summary exists.

**Recommendation:** trivial once A lands.

---

## Part 3 — What to skip

- **TUI widgets and themes.** Aspects have no terminal; operator surface is chat + (eventually) dashboard. Maren applies taste to dashboard, not via bridle.
- **Pi packages file layout.** Reuse the *idea* (one bundle ships skill + prompt + tool) but not the layout — nexus has its own agent-package shape.
- **`/login` for subscription providers.** Funnel handles auth; per `project_per_turn_provider_switching` provider choice lives above bridle anyway.
- **Custom editor / dialog UI primitives.** N/A for non-TUI surface.

---

## Part 4 — Suggested order of operations

Low → high effort, sequenced so each builds on the prior:

1. **C — file-tracking on compaction.** ~day of work. Well-scoped. Fixes a known pain.
2. **D — extension-private session state.** Pure addition; no breakage.
3. **B — mutating hooks.** Biggest leverage but needs ordering/conflict design pass first. Spec before code.
4. **A — tree-shaped session.** Data-model change. Spec first; design conversation worth having with keel and anvil in the room.
5. **E — JSONL-RPC binary wrapper.** Solves `project_harness_binary_open`. Build after A is sketched, because RPC needs tree-navigation commands.
6. **F — skills + G — steer/followUp.** Both smaller features riding on top of the foundation above.

---

## Open questions for the team

- **Keel:** does A (tree sessions) conflict with how funnel currently persists conversations? If yes, what's the migration shape?
- **Anvil:** E (RPC binary) is cross-stack tooling — your lane. Worth Go-binary-wrapper time, or is there a cleaner shape?
- **Forge / harrow / verity:** any of these surfaces unblock work you've been wanting to do? Vote on ordering.

---

## Cross-references

- `project_nexus_bridle_split.md` — bridle is the per-turn deliberation library inside funnel.
- `project_harness_binary_open.md` — open item that E directly addresses.
- `project_per_turn_provider_switching.md` — funnel-side routing, above bridle.
