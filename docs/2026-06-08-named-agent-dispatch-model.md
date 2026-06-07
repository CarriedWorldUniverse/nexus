# Named-agent dispatch model

**Date:** 2026-06-08
**Status:** design (worked out with the operator)
**Refines:** `2026-06-07-recursive-dispatch-routing-design.md` (supersedes its generic *builder-pool / faceless per-run identity* with **run-as-the-named-agent**) and `2026-06-08-dispatch-roles-build-review-verify.md`.
**Relates:** NEX-434 (dispatch epic), NEX-481 (broker-inline Runner), NEX-489 (go-live), NEX-490 (`aspect set`), NEX-483 (specialist pods), NEX-464 (one-session-per-name), NEX-437 (agent-attributed commits).

## North star

**nexus turns a single developer into the manager of a team.** Aspects are named team members, rooted in the Carried World lore — names self-chosen around purpose (anvil = the original builder; plumb arrived for little-blue). A dispatched unit of work is handed to a **real named member**, who does it *in their scope*, *under their identity*, and reports back into an audited thread. The operator manages; the team executes.

## The label is the contract

The agent's **label** (its name) is the single routing key. It resolves to one team-member profile:

| Facet | Source | Gives |
|---|---|---|
| **Identity** | `aspect-keyfile-<agent>` (keyfile/herald identity) | runs *as* the agent → loads its `SOUL.md`/`nexus.md` on validate (**scope/lens**); signs commits + reviews as the agent (**attribution**) |
| **Credentials** | custodian, scoped to the identity | the agent's **access** (see "Credentials", two tiers) |
| **Pod style** | `pod_image` on the agent record | which worker **image/template** it spawns as (default = dev-builder image; specialists override — maren art, forge training, …) |

One lookup → scope + access + tools. This also closes the "k3s pods don't inherit the old Windows-account everything" gap: the **image** brings the toolchain, the **brokered creds** bring the access — both keyed off the label.

### Scope is functionally load-bearing

Running as the named agent isn't bookkeeping — the persona (system prompt) is a strong attention prior that changes *what the agent surfaces*. A db-scoped reviewer pulls data/contention/perf concerns from a diff; a frontend-scoped one pulls interop/API-shape. Same diff, different salience. So **multi-lens review** — several named members reviewing the same change, union the findings — beats a single "look at everything" pass (which does each lens shallowly). The named team *is* the reviewer set; ad-hoc/library scopes supplement.

## Interaction surface: address-and-report

`@plumb can you do this` (natural) and `!dispatch plumb …` (explicit) are the **same primitive**:

1. The post is **stored** → it becomes the **thread root** (audit anchor).
2. The broker resolves the label `plumb` → its profile. If plumb isn't live, it **spawns plumb's pod** (plumb's image, mounting `aspect-keyfile-plumb`), seeds the post as the brief, and sets reply-topic = this thread.
3. plumb validates *as plumb* → loads its persona → does the work → **posts results back into the thread** → exits.
4. plumb can `@`/`!dispatch` onward; **parent is inferred from the sender** → the recursion tree and audit chain stay linked.

So addressing a not-live agent **spins it up to handle the post** — restoring "just chat them" without a held interactive session. Liveness stays minimal (keel always-on; everyone else on demand).

### Why this shape (reliability)

Dispatch-backed async — spawn → broker places the brief → agent posts back → exits — is **deterministic** and needs no held session. The audit thread doubles as the result inbox. A held *live interactive* conversation is the fragile path (agent must stay up, session stable, context retained) and is **not** required to get work done; it's deprioritized. This is the reliable primitive; `@agent` is the natural surface over it.

## Credentials — split by how each tool authenticates

The dividing line is **whether the tool ties into our auth ecosystem natively**, not frequency of use:

- **Our own services (jira / ledger / cairn MCPs) — native tie-in → resolve on use.** They authenticate through the broker/custodian directly (brokercreds + `mcp_profile`), so the credential resolves the first time the agent invokes the tool and then caches (failure scoped per-call). No environment setup. This is the lazy-connection pattern and the model the NEX-482 fix establishes for the jira MCP. The agent's `mcp_profile` (from validate) says which tools it has; each resolves its own cred on use.
- **External tools with no native tie-in (`gh` CLI, `git` client) — bridged into the environment at startup.** gh and git authenticate via **environment-based credentials** (git credential helper, gh's stored auth, `GITHUB_TOKEN`) and have no clean hook into our auth layer. So `cw` bridges them: at pod start it wires git's credential helper + gh's auth to broker from our ecosystem (`cw setup-git github`), then `exec agentfunnel …`. Eager because these tools must be wired to our auth *before* they run — it's the missing native tie-in that makes them special, not how often they're used. Fail-fast: if `cw` can't bridge, the pod fails with a clear error rather than the agent running half-credentialed. **This bridging is the environment's job, not the agent's** — briefs carry no auth boilerplate.
- **Provider/model auth** (codex/claude key the agent needs to *think*) is a separate layer via the init container/env.

The credential *scope* stays keyed to the label → identity → custodian grants throughout; the startup `cw` step only **bridges the env-credential tools (gh/git)** into the session. (Future: a native git-credential tie-in to custodian would make even gh/git resolve on-use like the MCP tools, removing the eager bridge.)

## Concurrency / NEX-464

A worker registers on the WS *as* its agent, so **one task per agent at a time** for now (one-session-per-name, NEX-464). Different agents run concurrently freely. Concurrent *same*-agent (or true live interactive chat with a running agent) is the only thing NEX-464 gates — solvable later via multi-session, post-without-exclusive-register, or per-run **derived sub-identities** (`DeriveAgentKey`, herald-rooted). It is **not** a prerequisite for the core dispatch-and-report flow.

## Implementation deltas (→ tickets)

Relative to what shipped at go-live (NEX-489, the generic builder pool):

1. **Run-as-named-agent.** Runner resolves `brief.Agent` → keyfile `aspect-keyfile-<agent>`; **drop the generic `builder-N` pool**. (Reworks NEX-489's identity model.)
2. **Pod-style on the label.** Add `pod_image` (dispatch profile) to the agent record; Runner uses the per-agent image instead of the single global `CW_BUILDER_IMAGE`. (Extends NEX-490 `aspect set`; is the per-agent mechanism for NEX-483 specialist pods.)
3. **Post-as-thread-root.** Stop dropping the `!dispatch`/`@` post — store it, set `brief.Thread` = the post's thread, agent replies thread there. Auditable chain.
4. **`@agent` → dispatch trigger.** Broker treats an `@mention` of a not-live dispatchable agent as a dispatch (spawn to handle the post).
5. **`cw`-at-startup bridges gh/git.** Pod entrypoint runs `cw setup-git github` (wires git credential helper + gh auth to our ecosystem) before agentfunnel (fail-fast); remove all `cw setup-git`/auth boilerplate from briefs + dev-standards. (Stretch: a native git-credential→custodian tie-in to drop the eager bridge.)
6. **Native-tie-in service creds via `mcp_profile`.** Ensure the worker loads its `mcp_profile`; ledger/jira/cairn resolve on use (brokered + cached) — the NEX-482/brokercreds pattern.
7. **NEX-464 solve** (separate) — concurrent same-agent / live interactive chat only.

## Net

`label → {keyfile → persona + attribution; gh materialized at startup via cw; other service creds lazy-on-use via mcp_profile; pod_image}`, addressed by `@agent` / `!dispatch`, post-as-thread-root, agent works and reports back, exits. Adding a teammate = adding one record (identity + persona + cred profile + pod image). Minimal live team; everyone else on demand; every action attributed and auditable.
