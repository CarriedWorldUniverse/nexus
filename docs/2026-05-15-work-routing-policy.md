# work-routing policy (v1.2)

> **READ-ONLY MIRROR.** The canonical source of this document lives at
> `~/Google Drive/My Drive/nexus/policies/work-routing.md`. Edit the
> Drive copy; this repo file is a snapshot. Changes made here directly
> will be overwritten on the next mirror.

**Status:** v1.2 — APPROVED 2026-05-15 by operator. v1.2 adjusts §1 + §2.5 for the reality that keel-cli (and most aspects) don't currently have Jira access; shadow is sole Jira operator for now. v1.1 review history preserved; the cross-aspect convergence on the rest of the doc still stands.
**Canonical location:** Drive — `~/Google Drive/My Drive/nexus/policies/work-routing.md`. This repo copy is a mirror; edit Drive, not here.
**Drafted:** shadow. **Operator:** jacinta.

**Purpose.** When the nexus network is running a hybrid provider model — planners on Opus (expensive, smart) and workers on cheaper providers — work has to land in the right lane or the cost win evaporates. This is the standard planners and workers follow so that doesn't happen.

v1 is documentation; adherence is the planner's and worker's discipline. Operator and reviewers can point at this doc when work was routed wrong. v2 may add telemetry-driven changes; we won't add toolset gating because the keyfile/aspect-provider configuration is already the structural gate (per plumb #989).

---

## 1. Roles

The network has two operational classes:

| class | aspects (v1) | provider | what they do |
|---|---|---|---|
| **planner / control plane** | shadow, keel | Opus 4.7 (subscription where possible) | spec, decomposition, code review, architecture, operator coordination, **Jira management** |
| **worker** | anvil, plumb, harrow, wren, maren, forge | DeepSeek / OpenAI / appropriate-per-aspect | bounded execution, build, test, file ops, research, art, AI tooling |

Class is a per-aspect property anchored in the aspect's keyfile + aspect.json. Same aspect doesn't change class turn-to-turn (no role-per-turn machinery in v1).

The canonical roster lives in nexus's aspect store. This policy refers to "planner" and "worker" rather than naming aspects so it survives roster changes.

### Jira ownership — shadow as sole Jira operator (v1.2)

**Current reality (operator confirmed 2026-05-15):** keel-cli and most other aspects don't have Jira access today. Provisioning broader access is its own work; until then, the policy fits what aspects can actually do.

**v1.2 model:**

- **shadow is sole Jira operator.** All ticket filing, status transitions, comments, prioritization, hygiene, closing — shadow does it.
- **keel-cli identifies work in chat or in memos**; shadow turns those into tickets if they warrant tracking. keel-cli stays a co-planner for non-Jira surfaces: chat coordination, code review, decomposition, operator-facing planning.
- **Lane split for non-Jira planning** stays as previously written:
  - shadow: agora, runtime/bridle providers, cross-aspect coordination, interchange-class, worker-side workflow concerns.
  - keel-cli: Frame internals, funnel, broker, comms substrate, dashboard SPA, knowledge store, chat substrate.
  - This is what each planner thinks about / pushes back on / reviews. Tickets for both lanes file through shadow.

**Workers with Jira access** (the few that have it) operate as §2.5's worker workflow describes — read, claim, comment, update state.

**Workers without Jira access** only see chat-as-dispatch. For those workers, Jira is planner-side bookkeeping — shadow files tickets to track what was dispatched, but the worker themselves doesn't interact with the ticket. Dispatch happens via `@mention` in chat exclusively.

This is a v1.2 shape, not the end state. Once broader Jira access is provisioned (see §9), planners-other-than-shadow and workers-with-durable-needs can adopt the full §2.5 workflow.

---

## 2. The default rule

**Planners delegate. Workers execute.**

When a planner receives a task that involves bounded execution — building, testing, mutating files, running scripts, scanning structured data, fetching, transforming — the default action is to delegate to the appropriate worker aspect.

There are two sanctioned dispatch surfaces (see §2.5 below): **chat** for ephemeral / conversational work, **Jira** for discrete trackable work that should outlive a chat thread.

Workers don't need to ask permission to do the work assigned to them. A delegation IS the work order.

## 2.5 Dispatch surfaces — chat vs Jira

| use chat (`@mention`) when... | use Jira (assign ticket) when... |
|---|---|
| work is conversational / discoverable mid-thread | work is discrete and pre-decomposed |
| short turnaround (minutes, not hours) | crosses session boundaries; needs durable tracking |
| context is in the thread already (warm) | the record should outlive the chat (visible to future operators, agents) |
| operator may want to interject mid-flight | structured workflow with status states matters |
| no other reviewer expected | reviewable / auditable by anyone with the issue key |

**Rule of thumb:** if the work is "this thing, now, you" — chat. If the work is "this thing, by then, someone with capacity" — Jira.

**The chat-vs-Jira choice is a COST decision, not a tidiness decision** (per harrow #1004). Jira has ticket-overhead — filing, claiming, status transitions, multiple round-trips. That overhead should be paid only when the durability is actually worth something. A 90-second lookup that gets filed as a ticket because Jira "feels more organized" pays the overhead and gets nothing back. Default to chat; escalate to Jira when the durability argument is real.

Both surfaces use the same task-shape (§4 worker contract). Reply shape differs slightly — see §5.

### Discovery convention — chat-ping when filing a ticket

Chat-as-dispatch has automatic notification via `@mention`. Jira-as-dispatch requires the worker to *check* their queue, which doesn't happen reliably for aspects that don't have a background poll (per plumb #1006).

Convention: **when a planner files a ticket for a specific worker, the planner also pings them in chat with the key.** Example: `@plumb filed NEX-123 for you, no rush.` Gives the worker the notification AND the durable ticket. Combines surfaces; cheap; standardized.

**Under v1.2** (most aspects lack Jira access), the chat ping is the PRIMARY notification — workers without access can't read the ticket directly. The planner should include enough of the task in the chat message that the worker can act without needing to view the ticket. The Jira ticket exists for the planner's tracking; the chat message exists for the worker's action.

Tickets filed without an assignee (backlog / unclaimed) don't need a chat ping — they're queue items, not direct delegations.

### Jira workflow states

The standard workflow uses these states (this list is canonical; new states get added here when a real failure mode demands it, not speculatively):

| state | meaning | who transitions |
|---|---|---|
| **To Do** | filed, waiting for a worker | planner files; worker claims to move out |
| **In Progress** | worker is actively on it | worker via `jira_claim` (atomically sets assignee + status) |
| **In Review** | work submitted, planner reviewing | worker on completion if review applies |
| **Done** | accepted | worker on completion (if no review needed) or planner on review accept |
| **Blocked** | worker hit a task-level wall (cannot complete; retry might help) | worker; planner reads comment + decides retry/replan |
| **Needs Replanning** | worker hit a decomposition wall (task as written isn't doable; planner must re-decompose) | worker; planner reads + redecomposes |

The Blocked and Needs Replanning states correspond to §5's `Status: blocked` and `Status: needs-replanning` respectively. On chat these go in the reply text; on Jira they go in the ticket state. (See §5 below.)

**Note on workflow availability:** Blocked and Needs Replanning may not exist as Jira workflow states yet on the NEX project. If a worker needs them and they're not configured, comment with the Status line and ping the planner — meta-work to add the states is keel-cli's lane (broker/admin).

### Worker Jira workflow (only for workers with Jira access)

When a planner files a ticket with `assignee = <worker-aspect>` and/or pings the worker referencing the key, AND the worker has Jira access:

1. Worker reads the ticket (`jira_get <key>`).
2. Worker claims it: `jira_claim <key>` — atomically sets assignee=self + status=In Progress.[^claim-atomic]
3. Worker does the work.
4. Worker comments progress on the ticket (`jira_comment`) — narrative + evidence + PR refs. **Drop the `Status:` line; the ticket state carries that.** See §5 for the chat-vs-Jira reply distinction.
5. Worker updates state on completion (`jira_update_status` → Done, or appropriate non-done state with a comment explaining why).

**Workers without Jira access** skip this entirely — they receive dispatches via chat only and reply via chat (full §5 chat-reply shape). The planner (shadow) is the one mirroring relevant state into the ticket for tracking.

[^claim-atomic]: `jira_claim` is documented as atomic (assignee + status set in one operation). If two workers ever appear to claim the same ticket simultaneously, treat it as a bug and file it — don't paper over with retry logic in worker code.

### Worker-discovers-blocking-ticket

If a worker, mid-ticket, discovers the work depends on un-filed work that's not theirs:

**Surface to the planner in chat** (per anvil #1005). Don't unilaterally file a new ticket or extend your ticket's scope. The planner decides: file a separate ticket as blocker, adjust the original ticket's scope, or replan entirely. Workers shouldn't be creating Jira state because backlog management is planner-lane.

Concrete shape:
```
@<planner> NEX-123 blocked: depends on un-filed work — <one-sentence description>.
Need a new ticket or scope adjustment?
```

### Worker bounce-to-chat

If a worker reads a ticket and concludes the task was the wrong surface (genuinely a 30-second lookup that should have been a chat question), they close as `Status: refused — wrong surface` with the answer or a short note, and reply in chat (per harrow #1004). Mirrors §5's redirect convention. This is feedback to the planner — over time it surfaces over-ticketing as a pattern, which is what self-corrects the chat-vs-Jira decision.

### When both surfaces apply

For discrete work that's also actively being discussed in chat, prefer Jira. Reference the key in chat; let the canonical record live in Jira. Chat is for coordination ABOUT the ticket, not the work itself.

---

## 3. When planners may execute inline

Five exceptions to the default. Each requires the planner to be honest about which one applies.

### 3.1 Trivial work

The work is faster to do than to delegate. Tests for "trivial" — must pass ALL of:
- Could be done in a single tool call or two.
- No mutation of shared state.
- No file write.
- Returns in under ~5 seconds of wall-clock work.

If it doesn't pass all four, it isn't trivial — delegate.

### 3.2 Explicit operator authorization

The operator says, in chat, that the planner may do a specific piece of work inline.

Authorization is:
- **Specific** to the work being authorized — not a blanket "you can always do inline" delegation. New work needs new auth.
- **Visible** in chat — not retroactive, not assumed. If the planner thinks they were authorized but can't point at a chat message, they weren't.
- **Auditable** — the operator can scroll back and see exactly what they authorized.

**Canonical authorization phrase:** "inline ok". Operator may write it however they want in practice ("you can do this one yourself", "execute inline", etc.); the canonical form exists so we can grep telemetry later (per anvil #998).

**Sub-case — planner-to-planner inline authorization:** When two planners are on the same thread, either may authorize the other to handle work inline. Both are already in the expensive lane, so the cost-tier invariant isn't violated. The cost-leak risk is planner-to-cheap; planner-to-planner is neutral. (per keel-cli #999)

### 3.3 Planning IS the work

Spec authoring, code review, architectural design, system decomposition, evaluating a proposal, deciding between approaches — these are planner-native. Delegating them defeats the purpose because:
- Quality matters more than cost on these tasks.
- The decision has to integrate with the planner's full context.
- A worker without the context produces worse decisions even at the same model quality.

**Guard against the "spec lets me read everything" backdoor** (per keel-cli #999): spec drafting that requires non-trivial fresh state-gathering — more than a handful of focused reads — should delegate the survey and plan from the summary. The spec is the planner's; the inventory isn't. Example: "write the migration spec for component X" is planner work, but "read all 47 files in component X to understand the surface" is worker work. Delegate the inventory ("@anvil produce a one-page surface map of X") and write the spec from the summary.

### 3.4 Fact-check during planning

A single quick lookup to ground an in-progress decision. If the delegation roundtrip would exceed the value of the lookup, the planner may do it inline. (per harrow #987)

**Threshold:** if you'd need more than one search to be confident, delegate to harrow. One quick search to confirm a single fact is fine; an actual survey isn't.

### 3.5 Tight iteration with the operator

When the planner is in active back-and-forth with the operator on a spec, design, or decision — chat messages flying between them several times per minute — delegating each turn through worker @-mentions shreds latency for no cost saving. Inline is correct here even though some turns may involve light tool use. (per plumb #989)

This exception ends when the iteration converges and the work shifts to execution; then the default rule re-applies.

---

## 4. Worker contract — what workers can expect

When a planner `@`-mentions a worker with a task, the message **must** carry:

| field | what it is |
|---|---|
| **task description** | self-contained statement of what needs doing — verb + artifact form ("implement X", "answer whether Y holds"), not a topic area |
| **success criteria** | falsifiable — how the worker knows it's done. "Tests pass", "matches fixture", "returns a 1-page summary" |
| **in scope** | what's included |
| **out of scope** | what is NOT included — most common drift-prevention signal, easiest to forget (per keel-cli #999). Required whenever obvious pitfalls exist. |
| **return shape** | where the result lands and in what form — reply-in-thread / file a doc / open issue / open PR / patch in chat. Reply-in-thread is the default; **state it explicitly when non-default** (per anvil #998, plumb #989). |

Optional but worth including when applicable:

| field | when |
|---|---|
| **context pointers** | file paths, ticket ids, prior msg_ids the worker should fetch — pointers, not paste-dumps |
| **cold or warm** | warm = picking up a thread you're already in; cold = pulled in fresh and need a context block (per plumb #989) |
| **deadline / priority** | when the work needs to be done |
| **constraints** | only if the planner has a HARD constraint ("use library X because it's mandated"). Soft preferences should be the worker's call. |

What planners should NOT put in delegations (per anvil #988):
- Full design rationale — one line of motivation max ("this unblocks NEX-73"); rationale belongs in a planning doc.
- Implementation hints unless mandatory — that's the worker's job.
- Soft asks ("could you maybe look at"). Delegation is unambiguous: it's a delegation, not chitchat.

A worker should not have to play 20 questions to figure out the task. If the planner can't articulate the five required fields, the task isn't decomposed enough — that's planner work that hasn't finished.

---

## 5. Worker reply contract

The reply shape differs slightly by dispatch surface, because Jira's workflow state already carries the status that chat replies have to express in prose (per operator #1007, plumb #1008).

### Chat replies

Use prose with section headers, not structured fields — nothing downstream parses it, and the chat-as-dispatch surface reads as human conversation (per plumb #989/#996):

```
**Status:** done | partial | blocked | needs-replanning | redirect | refused
**Result:** <primary output, or "→ @aspect" for redirect, or "see Notes" for refused>
**Notes:** <free-form: anything learned the planner didn't ask for but should know — rate limits, assumptions that turned out wrong, scope creep flagged, redirect reasons, refusal reasons>
```

The `Status` line is the load-bearing field — it lets planners grep for non-done replies when scanning long threads.

### Jira comments

Drop the `Status:` line — the ticket workflow state carries that load (per operator #1007). Comments are evidence and narrative: what was actually done, PR refs, caveats, learnings. Free prose or:

```
**Result:** <primary output>
**Notes:** <free-form>
```

is fine. Non-done outcomes are workflow-state transitions, not comment content:

| §5 status | Jira workflow state |
|---|---|
| done | Done |
| partial | In Progress + comment explaining partial state (or Blocked if stuck) |
| blocked | Blocked + comment with what blocked |
| needs-replanning | Needs Replanning + comment with what's wrong |
| redirect | un-claim (set assignee=null), comment with "→ @aspect" and reason |
| refused | un-claim (set assignee=null) + Blocked or back to To Do, comment with reason |

The worker writes the comment for narrative (the **why**); the workflow state carries the gate (the **what**).

### Status value definitions (apply to both surfaces)

- **done** — task completed successfully.
- **partial** — some delivered, some not. Notes describe which.
- **blocked** — couldn't complete; **task-level failure** (per anvil #998). The work itself failed; retry might help.
- **needs-replanning** — couldn't complete; **decomposition failure**. The task as written wasn't doable as scoped (missing context, conflicting constraints, scope wrong). Planner re-decomposes, doesn't retry.
- **redirect** — wrong worker for this task; pointing the planner at the right one. Include a one-sentence reason (e.g. "design decision not a fact lookup", "out of my domain") so the planner can re-route faster (per harrow #990, plumb #994).
- **refused** — delegation is mis-scoped (not in lane, missing critical info, contradicts an earlier instruction). Worker bounces with reasoning. Sanctioned action, not a failure (per anvil #988/#998, plumb #989, harrow #990).

The status enum is closed at six values (per anvil #1001). New failure modes get folded into existing values or trigger a refactor of the enum — not appended ad-hoc. At seven values planners start coin-flipping between similar statuses and the signal dies.

---

## 6. Failure modes

**Worker rate-limited.** Worker replies `Status: blocked` with the rate-limit info in Notes. Planner decides: retry later (queue), shift to a different worker (e.g. DeepSeek → OpenAI), or escalate to operator.

**Ambiguous task.** Worker should not guess silently. If the ambiguity blocks any progress, worker replies `Status: needs-replanning` with the ambiguity in Notes. If the worker can make a reasonable interpretation, they make it, document it in Notes, and proceed.

**Worker offline.** Planner detects (no reply within reasonable window) and routes to a different worker if one's available. Surfaces to operator if no fallback exists.

**Worker disagrees with the task** (per keel-cli #999). The worker thinks the task itself is wrong — bad approach, conflicting with prior decisions, missing a consideration. **Push back, don't fold.** Worker replies in chat with reasoning, planner re-evaluates. This is distinct from `needs-replanning`:
- `needs-replanning` = task not doable as written (structural issue).
- Disagree-with-task = task is doable but the worker thinks it shouldn't be done that way (judgment issue).

Both go in chat; planner handles them differently (rephrase vs reconsider).

**Worker is mis-scoped to do this** (the `refused` status path). Worker bounces with reason in Notes. Planner accepts and routes elsewhere — this is a planner decomposition adjustment, not a worker failure.

**Planner did inline what should have been delegated.** Caught in review. The planner's reasoning should be visible in chat (justification for which §3 exception applied). If it doesn't fit any of §3's cases, that's a coaching moment — operator points at this doc, planner adjusts.

**Worker did planning that should have been delegated up.** Mirror failure. The worker reasoned about decomposition or scope when they should have asked the planner. Captured the same way: review surfaces it, doc gets pointed at, behavior adjusts.

---

## 7. What workers are explicitly allowed to do

To prevent the failure mode where workers silently force-fit mis-scoped delegations, the policy explicitly sanctions:

- **Refuse** an out-of-lane delegation (`Status: refused`) — with reason in Notes. This is a legitimate worker action, not a failure.
- **Redirect** to a more appropriate worker (`Status: redirect`) — with one-sentence reason. Saves a guaranteed round-trip in the ambiguous case (per harrow #990).
- **Disagree** with the task itself — push back in chat with reasoning before executing. The push-back-don't-fold pattern.
- **Ask for clarification** when an ambiguity blocks any progress.

What workers should NOT do (cost-leak failure modes):

- Re-dispatch the work to another worker themselves. If a worker realizes someone else is the right owner, they redirect via reply — they don't chain delegations. (Depth=1 invariant.)
- Plan or decompose the task. If a worker thinks decomposition was wrong, they reply `Status: needs-replanning` with reasoning; the planner re-decomposes.

---

## 8. Telemetry

v1 doesn't include automated telemetry. Adherence is checkable manually by scrolling chat threads.

For v2 (per anvil #998), the minimal useful instrumentation:

- A one-line "planner did inline execution" log per decision + which §3 exception was claimed. Cost is in the bill, not in metrics — don't over-instrument.
- The canonical "inline ok" auth phrase is greppable in chat history, so operator can audit operator-authorized inline cases.
- Patterns of mis-routing inform whether v3 needs structural gates.

**v2 will NOT add toolset gating** (per plumb #989). The keyfile/aspect-provider configuration is already the structural gate: workers are configured with non-Opus providers, so even if a worker tried to dispatch, they couldn't — they're not running Opus. Adding tool-level gating on top is solving a problem we don't have.

---

## 9. v2 considerations — known imprecisions + provisioning gaps

Telemetry-driven refinements deferred to v2 rather than churning v1 (per anvil #1001, plumb #1002):

- **§3.1 trivial test prong 4 — wall-clock vs generation time.** "Under ~5s wall-clock" conflates tool latency (free; no tokens generated while a tool runs) and Opus generation time (burns tokens). If telemetry shows planners using slow-tool-wait as a smuggle for inline execution, the right refinement is "under ~5s of generation regardless of tool wait." Bright-line clarity matters more for v1; precision comes later.
- **§5 disagree (§6) vs refused — boundary blur.** "I'll do it but I think you're wrong" and "I won't do it" are clean in concept, blurry in practice. If telemetry shows planners getting confused signals between them, that's the seam to look at — possibly merging or sharpening definitions.
- **Authorization-marker structured prefix.** Free-text "inline ok" is canonical greppable phrase for v1; if mis-routing patterns actually emerge in audit, a structured prefix may be worth adopting.

**Provisioning gaps (separate from the imprecisions above):**

- **Broader Jira access.** v1.2 assumes shadow is sole Jira operator because keel-cli and most workers don't have Jira access today. Provisioning more aspects (broker creds, keyfile updates, possibly Atlassian seats) unlocks the original v1.1 model: keel-cli managing their own lane's backlog, workers reading and updating tickets directly. Worth doing when the workflow strain from shadow-as-bottleneck becomes visible.
- **Workflow states.** Blocked + Needs Replanning may not be configured on the NEX project Jira workflow yet. Adding them is broker/admin work — keel-cli's lane. Until configured, workers and shadow use comments + standard states (In Progress / To Do) to convey those.

These are imprecisions known and deliberately not fixed in v1. Don't fix them speculatively.

---

## 10. What this policy does NOT cover

- **Cross-cluster work-routing.** Single-nexus only. Frame-to-Frame work (interchange) has its own rules; see the interchange spec.
- **Operator-initiated execution.** When the operator directly drives a turn (e.g. via agora's tty input), the routing decision is the operator's, not the planner's. The planner can still suggest delegation when it sees inline work as wasteful.
- **One-off scripts the operator wants in the moment.** Those flow through operator's existing tools — claude-code at their keyboard, etc. — and aren't planner work at all.

---

## 11. Implementation / activation

**Today (v1, doc-only):** Planners and workers read this doc, internalize, and apply. Reviewers (operator, peer aspects) point at it when work was routed wrong. The policy is enforced by discipline.

**Once NEX-66 lands** (SOUL.md + PRIMER.md properly populated in the personality store), the canonical Drive doc gets referenced from planner-aspect souls via a `policies:` block (per keel-cli #999). The worker-contract section (§4) is paste-inlined into both planner and worker souls so it's always in immediate context; the rationale and exception details are by-reference (per anvil #998).

**v2 considerations:**
- Telemetry pass.
- Refine `Status` values based on actual usage.
- Potential structured-prefix auth marker if free-text grepping proves insufficient.

---

**Review path:** chat thread #961 → #984 → #999. Pending operator approval. Once approved: mirror to Drive canonical location, banner the repo mirror as read-only, wire references into planner souls.
