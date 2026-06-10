# Roundtable: napping presence, mediation, and aspect-owned fan-out

**Status:** spec (operator-directed; NEX-568 + NEX-571 — the Fable-week focus)
**Date:** 2026-06-11
**Driver:** shadow (with the operator)

## Problem

Three related losses, named by the operator across 2026-06-10/11:

1. **Presence died with dispatch-native.** Plumb/anvil became ticket-vending
   processes that only exist while holding work. They cannot be addressed,
   convened, or talked to. The multi-aspect consensus conversation — several
   named minds arguing a problem from different lenses until convergence —
   is structurally impossible today.
2. **The conversation blocks on the work.** A long orchestration turn holds
   the floor for 10–15 minutes; the operator's thoughts during that window
   get shelved. The conversation plane and the work plane are fused.
3. **Fan-out lost its soul and its audit.** The original dispatch design had
   aspects spawning their own background workers — carrying the parent
   aspect's personality and configuration, with the work recorded in threads.
   What shipped is anonymous builders taking tickets. Meanwhile claude-code /
   codex native fan-out has the *capability* (cheap parallel sub-agents) but
   none of the *observability*: sub-thread inputs/outputs vanish into a
   consolidated answer.

## Requirements (operator contract)

- **R1 — Mediated conversation.** The operator talks 1:1 to shadow (agora).
  The team thread runs *beside* the operator, never *at* them. Shadow
  mediates: digests on demand, and the group's questions deduped and batched
  into decision-points, each carrying the context needed to answer it.
  Never a question firehose; never deciding from five posts behind.
- **R2 — Presence without always-on.** Aspects are *addressable-but-napping*:
  identity, inbox, and session persist; an @mention/DM wakes the pod in
  seconds; quiet aspects idle back to zero. Presence is the name answering,
  not a pod burning watts.
- **R3 — Non-blocking turns.** No aspect turn holds the floor beyond ~30s.
  Longer work becomes background fan-out with a comms callback; results
  re-enter the thread as messages and get folded into later short turns,
  interleaved with the live conversation. The operator's mid-work thoughts
  are triaged immediately (answer / ticket / steer the running job).
- **R4 — Aspect-owned fan-out, audited.** An aspect can spawn sub-workers
  that carry *its own* soul/configuration, under a derived identity whose
  scope is never wider than the parent's (the cairn transitive-permission
  model). Every sub-worker's brief (input), live trace (observe stream),
  and result (output) is addressable in one place: threaded under the
  spawning turn's audit root. This is CC-grade fan-out plus the audit CC
  cannot give.

## Architecture

Five components, mapped to the existing platform seams.

### 1. Napping presence (broker + dispatch)

- **Roster** gains a `napping` status alongside live/stale/down: the aspect
  is registered-but-asleep — identity known, inbox accumulating, wakeable.
  Driven by a per-aspect **wake policy** in broker config:
  `always-on | wake-on-mention | dispatch-only`.
- **Wake controller (broker):** on chat delivery addressed to an aspect
  (DM topic, @mention, or convene brief) whose status is napping and policy
  is wake-on-mention, scale its Deployment 0→1 (new `dispatch.K8s.
  ScaleDeployment`; the gemma-vllm scaled-to-zero deployment is the cluster
  precedent). The triggering message needs no special handling: the existing
  `since_msg_id` replay delivers it when the aspect registers.
- **Idle reaper (broker):** no completed turns and an empty inbox for
  `idle_timeout` (default 15m) → scale to 0, roster → napping. Never reaps
  an aspect with an in-flight turn or live sub-workers.
- **Session continuity:** funnel session state persists on the aspect's PVC
  (maren's pattern) so a woken aspect is the same mind, not a blank.

### 2. Aspect-owned fan-out — "hands" (broker Runner + funnel)

This RECOVERS Hand Dispatch v0.1 (2026-04-30), which the named-agent model
(2026-06-08) displaced rather than complemented. The original language:
*"Hands are fresh-context instances of the dispatching aspect … when maren
dispatches, the slot boots with maren's NEXUS.md and SOUL.md and acts as
maren on a fresh-context turn. … Workers are interchangeable as slots, not
as personalities."* Result attribution, triage rules, cost, and domain
knowledge all inherit from the dispatcher. Both shapes now coexist:
`!dispatch <agent>` = a *different* named team member takes the work
(multi-lens); `spawn` = *your own hands* doing focused fan-out.

- A new dispatch shape next to the ticket-builder: **`spawn`** — requested
  BY an aspect mid-turn (tool/RPC on the aspect WS: `spawn.request{brief,
  count, config_overrides}`), not by chat parsing.
- The Runner creates worker Jobs that boot with the **parent's image,
  NEXUS.md/SOUL.md, and config** (per-work-type image seam already exists
  in JobConfig.Image) under a **derived identity** `<parent>.sub-N` (herald
  DeriveAgentKey lineage when herald-rooted boot lands; until then the
  broker mints the scoped session). Effective scope = parent ∩ grant —
  never wider than the parent (cairn transitive model). Per v0.1, a hand's
  reply is attributed to the parent aspect; the lineage tag (not a separate
  persona) is what distinguishes it in the audit trail.
- **Audit threading:** the spawning turn's message is the audit root (the
  proven !dispatch thread-root pattern). Each sub-worker's brief is posted
  under it on creation; its result (or failure) is posted under it on exit;
  its observe stream is tagged with the lineage so the trace pane / FeedView
  can follow any sub-worker live. One place answers "what did this fan-out
  do, with what inputs, producing what."
- **Non-blocking by construction:** `spawn.request` returns immediately
  with the sub-worker handles; the aspect's turn ends; results arrive as
  ordinary thread messages that trigger ordinary (short) turns.

### 3. Convene (broker + funnel)

- `!convene <aspects> — <problem>` (chat-parsed like !dispatch, plus a
  `convene.request` RPC for aspects). Creates the convene thread root,
  wakes every named participant (component 1), and posts each a brief:
  the problem, its lens (from its personality or an explicit
  per-participant lens in the command), and the thread binding.
- Participants converse in the thread — the funnel already handles
  per-thread context (plumb-tier threading).
- A **convergence judge** (the DoD-judge machinery pointed at the thread)
  evaluates after each round: still progressing / converged / stuck. On
  converged it posts the consensus summary and releases participants
  (idle reaper takes them back to napping). On stuck it surfaces a
  decision-point to the mediator instead of looping forever.

### 4. Mediation (shadow's funnel role + thread plumbing)

- Threads get a per-subscriber delivery mode: **full** or **digest**. The
  operator's surfaces default to digest for convene threads; aspects in the
  thread get full.
- The mediator (shadow) receives full traffic and owns the operator-facing
  output: digests on cadence or on request ("where are they at?"), and
  **decision-points** — the group's open questions deduped across agents,
  batched, each packaged with the context required to answer it. Answers
  flow back into the thread attributed to the operator.
- v1 surface: decision-points and digests are formatted DMs in the existing
  dm:shadow conversation (agora renders them today). Dedicated agora UX is
  a later nicety, not a dependency.

### 5. Turn contract (funnel + runbook)

- Funnel gains a soft **turn budget** (config, default 30s): past it, the
  judge nudges the deliberation to wrap and continue via fan-out. Soft —
  never kills a turn mid-tool.
- The behavioral half lives in the aspect runbooks (shadow's RUNBOOK.md
  already carries it): decompose → spawn/dispatch → reply now → fold
  results in later.

## Non-goals (v1)

- Parallel turn lanes inside one funnel (short turns + fan-out first;
  revisit only if proven insufficient).
- agora multi-party UI (digest/decision-points ride the existing 1:1).
- Cross-cluster convene, voice, web-UI convene controls.
- Replacing ticket-builder dispatch — it stays for ticket work; spawn is a
  sibling shape, not a rewrite.

## Phasing (each independently shippable)

1. **P1 Napping presence:** roster status + wake policy + ScaleDeployment +
   wake controller + idle reaper. Restores plumb/anvil as addressable team
   members. Acceptance: DM a scaled-to-zero plumb from agora; reply arrives
   with no operator action; pod gone ~15m later; roster shows napping.
2. **P2 Aspect-owned fan-out:** spawn.request + derived-identity Jobs +
   audit threading + lineage-tagged observe. Acceptance: shadow spawns two
   sub-workers mid-conversation, keeps talking, results land threaded under
   the spawning turn; trace pane can follow each sub-worker.
3. **P3 Convene:** !convene + briefs + convergence judge + consensus post.
   Acceptance: convene plumb+anvil on a design question; they converge;
   summary posts; participants nap.
4. **P4 Mediation:** digest delivery mode + decision-point batching.
   Acceptance: a convene with deliberately conflicting lenses produces
   batched contextual decision-points in dm:shadow, not raw thread noise.

## Testing

Per phase: unit tests at each seam (roster transitions, wake-policy gating,
ScaleDeployment fake, spawn Job construction + identity scoping, audit-post
shapes, judge verdict handling, digest routing); an e2e on dMon per
acceptance line above. P1's wake path gets a chaos case: mention during
scale-up (no double-wake), reap-during-turn forbidden.

## Lineage

- Hand Dispatch v0.1 (2026-04-30, `2026-04-30-hand-dispatch-v0_1.md`,
  nex-443 tree): the original hands/soul-inheritance design — recovered by
  component 2.
- Named-agent dispatch model (2026-06-08): stays as-is for ticket work and
  multi-lens dispatch; spawn is its sibling, not its replacement.
- k3s work dispatch design (2026-06-05, archived): audit thread-root +
  non-blocking result delivery — both already implemented in the Runner
  (dispatch_msg_id, ParentRunID, async OnJobDone→thread); components 1–4
  build on those proven seams.

## Open questions (operator, when you're back)

- Wake policy defaults per aspect: plumb/anvil/harrow = wake-on-mention,
  keel = always-on, maren = wake-on-mention? (Assumed in P1 config.)
- Sub-worker cost ceiling: cap concurrent sub-workers per aspect (default 4?)
  and burn caps per convene?
- Should convene participants see each other's observe traces, or only the
  thread? (v1: thread only.)
