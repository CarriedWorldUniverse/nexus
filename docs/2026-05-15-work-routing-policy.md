# work-routing policy (v1)

**Status:** v1 — incorporated review from keel-cli (#999), anvil (#988/#998), plumb (#989/#994/#996), harrow (#987/#990). Pending operator approval.
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

### Jira ownership

Planners manage Jira (filing tickets, prioritization, hygiene, closing stale items). Workers operate on assigned tickets — claim, comment, update state — but don't drive the backlog.

**Open question (v1):** with two planners (shadow + keel-cli) running concurrently, which is primary Jira owner? Three possible shapes:
- **shadow primary** — shadow drives the backlog for nexus / OSS lanes; keel-cli files into it as needed.
- **keel-cli primary** — keel-cli drives; shadow files as needed.
- **split by lane** — each planner owns their lane's tickets (e.g. shadow owns agora/runtime, keel-cli owns Frame/nexus internals); cross-lane tickets go to whichever planner is closest.

Resolve before v1.1 of this doc.

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

Both surfaces use the same task-shape (§4 worker contract) and the same reply-shape (§5 reply contract). The difference is only WHERE the task lives.

### Worker Jira workflow (when delegated via Jira)

When a planner files a ticket with `assignee = <worker-aspect>` and/or pings the worker referencing the key:

1. Worker reads the ticket (`jira_get <key>`).
2. Worker claims it: `jira_claim <key>` — atomically sets assignee=self + status=In Progress.
3. Worker does the work.
4. Worker comments progress on the ticket (`jira_comment`) — same §5 reply shape: Status / Result / Notes lines. Multiple comments fine; the last one is canonical.
5. Worker updates state on completion (`jira_update_status` → Done, or back to To Do with `Status: blocked` comment, etc.).
6. If the worker `refused` or `redirect`s, they comment the reason and either un-claim (set status back to To Do, clear assignee) or reassign to the appropriate worker.

### Planner Jira workflow

- File tickets with the same five required fields as the chat contract (§4): task / success / in-scope / out-of-scope / return-shape.
- Either assign directly to the worker aspect (preferred for unambiguous routing) OR file and `@mention` in chat with the issue key (when routing is uncertain or the planner wants discussion before assigning).
- Review the ticket's last comment for the canonical Status before closing or replanning.

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

Workers reply in the same thread (so the planner sees the result in-context). Use prose with section headers, not structured fields — nothing downstream parses it, and the chat-as-dispatch surface reads as human conversation (per plumb #989/#996):

```
**Status:** done | partial | blocked | needs-replanning | redirect | refused
**Result:** <primary output, or "→ @aspect" for redirect, or "see Notes" for refused>
**Notes:** <free-form: anything learned the planner didn't ask for but should know — rate limits, assumptions that turned out wrong, scope creep flagged, redirect reasons, refusal reasons>
```

**Status values:**

- **done** — task completed successfully.
- **partial** — some delivered, some not. Notes describe which.
- **blocked** — couldn't complete; **task-level failure** (per anvil #998). The work itself failed; retry might help.
- **needs-replanning** — couldn't complete; **decomposition failure**. The task as written wasn't doable as scoped (missing context, conflicting constraints, scope wrong). Planner re-decomposes, doesn't retry.
- **redirect** — wrong worker for this task; pointing the planner at the right one. Include a one-sentence reason in Notes (e.g. "design decision not a fact lookup", "out of my domain") so the planner can re-route faster (per harrow #990, plumb #994).
- **refused** — delegation is mis-scoped (not in lane, missing critical info, contradicts an earlier instruction). Worker bounces with reasoning. Sanctioned action, not a failure (per anvil #988/#998, plumb #989, harrow #990).

The `Status` line is the load-bearing field — it lets planners grep for non-done replies when scanning long threads.

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

## 9. What this policy does NOT cover

- **Cross-cluster work-routing.** Single-nexus only. Frame-to-Frame work (interchange) has its own rules; see the interchange spec.
- **Operator-initiated execution.** When the operator directly drives a turn (e.g. via agora's tty input), the routing decision is the operator's, not the planner's. The planner can still suggest delegation when it sees inline work as wasteful.
- **One-off scripts the operator wants in the moment.** Those flow through operator's existing tools — claude-code at their keyboard, etc. — and aren't planner work at all.

---

## 10. Implementation / activation

**Today (v1, doc-only):** Planners and workers read this doc, internalize, and apply. Reviewers (operator, peer aspects) point at it when work was routed wrong. The policy is enforced by discipline.

**Once NEX-66 lands** (SOUL.md + PRIMER.md properly populated in the personality store), the canonical Drive doc gets referenced from planner-aspect souls via a `policies:` block (per keel-cli #999). The worker-contract section (§4) is paste-inlined into both planner and worker souls so it's always in immediate context; the rationale and exception details are by-reference (per anvil #998).

**v2 considerations:**
- Telemetry pass.
- Refine `Status` values based on actual usage.
- Potential structured-prefix auth marker if free-text grepping proves insufficient.

---

**Review path:** chat thread #961 → #984 → #999. Pending operator approval. Once approved: mirror to Drive canonical location, banner the repo mirror as read-only, wire references into planner souls.
