# Roundtable P4 — Mediation Implementation Plan (draft)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Draft status:** finalize after P3's facilitator behavior has run live at least once — the decision-point format should be shaped by real convene transcripts, not invented. Tasks A/B are stable regardless; C is the part to revisit.

**Goal:** The operator experiences convene threads as digests and batched decision-points in their 1:1 with shadow — never raw multi-agent traffic (NEX-568; spec component 4; R1).

**Architecture:** Two small platform pieces + one behavioral piece. The broker gets per-subscriber thread delivery modes (full|digest) so operator surfaces stop receiving raw convene traffic; shadow (the mediator) gets the full stream and owns producing digests and decision-points as ordinary DMs. No broker AI (dispatch-native rule), no new surfaces (agora's 1:1 renders everything in v1).

---

### Task A: Thread delivery modes (broker)

**Files:** `nexus/broker/operator_subs.go` / `chat_send.go` region (operator fan-out), frames payloads; test beside.

- [ ] `subscribe.chat` payload gains optional `thread_modes: {"<topic>": "digest"|"full"|"mute"}` plus a default mode for convene-class topics (topic prefix `convene-` or the convene record marks the topic). Semantics: `digest` = the operator conn does NOT receive per-message chat.deliver for that topic; it receives only messages flagged as digest-grade (see Task B). `mute` = nothing. `full` = today.
- [ ] Backwards compatible: absent thread_modes = full everywhere (today's behavior, all existing tests must pass unchanged).
- [ ] TDD: mode routing per case; replay/catch-up honors modes (chat.list is a pull — leave it full; modes only shape push).

### Task B: digest-grade message marking

**Files:** chat payloads (a `Grade` field: `""|"digest"|"decision"`), HandleChatSend passthrough, agora/dashboard ignore-unknown-fields verified.

- [ ] The mediator marks its operator-facing posts (`grade: digest` for digests, `grade: decision` for decision-points) via the existing send path (MCP send_chat gains the optional param). Broker fan-out: a `digest`-mode subscriber receives only graded messages for that topic.
- [ ] TDD: graded fan-out matrix; ungraded messages in digest-mode topics suppressed for the operator conn but stored + visible in chat.list.

### Task C: mediator behavior (template + runbook; revisit post-P3)

- [ ] Extend the facilitator brief template (P3 Task C single source) with mediation duties when the facilitator is shadow and the convener is the operator: digest cadence (on convergence milestones or operator ask, not per-message), decision-point format — one DM per BATCH: numbered points, each with (question, why it's blocking, the context needed to answer, the participants' positions), explicit "answer inline by number" instruction.
- [ ] Answers route back: the mediator posts the operator's answers into the convene thread attributed `on behalf of operator`.
- [ ] agora niceties (decision-point styling) → NEX-572's redesign, not here.

### Task D: e2e acceptance

- [ ] Re-run the P3 acceptance convene with the operator's surface in digest mode: raw thread traffic does NOT appear in dm:shadow/agora; one digest at convergence; the chaos case produces one batched decision-point DM with context; answering by number unblocks the thread.

## Out of scope

Web-UI digest rendering (NEX-572); per-aspect (non-operator) digest modes; ML summarization anywhere in the broker.
