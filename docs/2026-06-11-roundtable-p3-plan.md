# Roundtable P3 — Convene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `!convene plumb anvil — <problem>` pulls named (possibly napping) aspects into one thread, each with a lens, where they argue to consensus; a facilitator judges convergence and posts the summary (NEX-568; spec component 3).

**Architecture decision (locked):** the broker stays a lean hub — NO embedded AI (dispatch-native rule). Convergence judging is an *aspect behavior*: the convene names a **facilitator** (default = the convener; the operator's convenes default to shadow) whose funnel receives a facilitator brief and owns judging + the consensus summary + closing. The broker provides plumbing only: parse, thread root, participant briefs (whose @mentions make the wake controller fire emergently — no special wake code), a convene record, and close semantics.

**Branch:** `feat/convene` off main after P1+P2 merge (depends on the wake controller for sleeping participants and benefits from hands for facilitator legwork).

---

### Task A: !convene parse + convene record (broker)

**Files:** `nexus/broker/chat_send.go` (intercept beside !dispatch), new `nexus/broker/convene.go` + test, storage beside the runs table (read how RunsStore is wired — mirror a small `convenes` table: id, root_msg_id, facilitator, participants CSV, status open|converged|abandoned, created/closed timestamps).

- [ ] Parse `!convene <a> <b> [<c>…] — <problem>` (also accept `:` as the separator; aspects = whitespace/comma list; validate ≥2 known base aspects, no derived names). The post is stored and becomes the thread root (the !dispatch post-as-root pattern — reuse, don't fork).
- [ ] Insert the convene record (status open). Facilitator = the sender unless `facilitator=<name>` appears in the command; operator-sent convenes default facilitator to shadow.
- [ ] Per-participant brief posts INTO the thread, each `@<participant>`-mentioning so RecipientPolicy delivers and the wake controller wakes sleepers with zero new wake code: the brief = problem + that participant's lens (from `lens:<participant>=<text>` segments if present, else "your standing perspective") + thread rules (reply in-thread; argue your lens; converge honestly; keep turns short).
- [ ] Facilitator brief post (also in-thread, @facilitator): judge after each round — progressing / converged / stuck; on converged post `CONSENSUS:` summary then close; on stuck surface a decision-point to the operator's mediator channel (P4 will formalize; v1 = DM shadow) then close as abandoned if unresolved.
- [ ] TDD: parse cases (separators, unknown aspect rejection, derived-name rejection, facilitator override), record insert, brief post shapes + mention targets, root threading.

### Task B: convene.close RPC + lifecycle

**Files:** `nexus/frames/` (KindConveneClose "convene.close" + payload {ConveneID, Status, SummaryMsgID} — REGISTER IN IsKnown), handler in `nexus/broker/convene.go`.

- [ ] Only the facilitator (or an operator conn) may close; status → converged|abandoned; idempotent. Closing does nothing to participants — the idle reaper naturally naps them after quiet (verify no interference: a closed convene's participants with no other traffic reap on the normal timeout).
- [ ] `convenes.list` operator RPC (open + recent) for future watch surfaces; mirror runs.list's shape.
- [ ] TDD: authz (non-facilitator rejected), idempotency, list shape.

### Task C: facilitator behavior (runbook + personality, not code)

**Files:** `deploy/shadow/RUNBOOK.md` (facilitation section), and the convene facilitator brief template text in convene.go (Task A) — keep the behavioral contract in ONE place (the brief template) and reference it from the runbook.

- [ ] Brief template covers: lens discipline, round cadence (let every participant speak before judging), convergence test ("would each participant sign the summary?"), the CONSENSUS: post format (decision, rationale, dissents noted, follow-up tickets), stuck → decision-point escalation, then convene.close.
- [ ] No new Go beyond the template string; TDD = template renders with participants/lenses substituted.

### Task D: e2e acceptance (dMon, after deploy)

- [ ] With plumb+anvil napping: operator (or shadow) posts `!convene plumb anvil — should bridle adopt a registry for hand images?` (any real small design question). Assert: both pods wake; both post in-thread from their lenses; facilitator (shadow) posts CONSENSUS: and closes; convene record converged; participants nap after the idle timeout; the whole exchange reads coherently in the thread (FeedView).
- [ ] Chaos case: one participant never responds (scale its deployment to 0 mid-convene manually) → facilitator marks stuck → decision-point lands in dm:shadow → close abandoned works.

## Out of scope

Digest delivery mode + decision-point batching (P4 — the facilitator DMs shadow plainly in v1); convene of derived names; multi-facilitator; web-UI convene controls (NEX-572 redesign owns that later).
