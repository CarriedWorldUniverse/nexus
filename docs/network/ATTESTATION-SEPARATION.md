# Attestation separation of duties (cairn #99 design pass)

**Status:** DECIDED (Option B), 2026-07-09 · **Author:** shadow · **Decides:** how the broker gates attest cairn pull checks without the gated code being able to forge them. Blocks production enablement of the pull-checks wiring (nexus #468, shipped dark).

## The invariant

> **The attesting process must run where the gated code cannot read its credential, and must verify the artifact rather than trust the worker's report of it.**

Two clauses, both required. Recording from a different pod but relaying a worker-asserted verdict fails the second clause (still self-report). Verifying the artifact but from inside the worker pod fails the first (the model can forge/replace before attesting).

## Why the current wiring violates it

The pull-checks recorder (nexus #468) runs inside `agentfunnel -builder`, which executes **in the worker pod** alongside the model-driven agent that can run shell. So:
- **First clause fails**: `CW_PULL_TLS_*` env-forwards the broker-gate mesh identity into the worker pod; a shell can read the key and present `cwb-subject=broker-gate` (with `CW_PULL_DEV_INSECURE`, no key needed at all).
- The gates themselves *do* satisfy the second clause today (they read the pushed diff, per the gate-hardening thesis) — but that's moot while the first clause is open.

## The two enabling facts (from recon)

1. **No worker→orchestrator verdict channel exists.** `nexus/orchestrator/wake.go`: `OnJobDoneHook` carries only `Ticket + OK bool` — "a stand-in… no richer worker-reported verdict channel in this codebase yet." `RecordJobResult` is exported for exactly this future channel.
2. **The gates are artifact-reading, hence relocatable.** `pr-exists`/`pr-substantial`/`acceptance-judge`/`test-evidence` all verify the pushed PR/diff (GitHub or cairn), not the worker's narrative. Nothing about them *needs* to run in the worker pod — they run there today only because that's where the completion loop lives.

## Options

### A — Relocate the authoritative gates to the orchestrator (post-hoc)
The worker's job shrinks to: do the work, push the branch, open the PR, claim done. The **orchestrator** (broker pod, no model) then independently runs the gates against the pushed artifact and records the cairn checks with `broker-gate` identity. `MergePull` enforces. Subsumes the broker's original task_done-trusts-self-report top-gap by construction.
- ✓ Invariant fully satisfied (both clauses).
- ✓ Smallest new channel — the orchestrator already has the ticket + branch convention (`builder/<ticket>`) to locate the PR; it needs no new verdict channel, it re-derives.
- ✗ Loses in-session reprompt: a failing gate becomes a **re-dispatch** (fresh run, lost context), not a cheap in-loop "fix it now" nudge. Iteration cost rises.

### B — Two-tier: worker gates advise, orchestrator gate attests (RECOMMENDED)
Keep the worker-side gates as **fast, advisory, in-session reprompts** (cheap iteration — the worker fixes before finishing), but they record NOTHING to cairn and are never trusted. The **orchestrator independently re-runs the authoritative gates** against the pushed artifact and is the **sole recorder** of cairn checks (`broker-gate`, broker pod). The dark pull-checks wiring MOVES from worker to orchestrator.
- ✓ Invariant fully satisfied; worker gates keep cheap in-loop iteration.
- ✓ The `checks:attest` scope (original #99 idea) now genuinely works — only the orchestrator holds it; belt-and-suspenders on the cairn side too.
- ✗ Gates run twice (advisory + authoritative). More code; the authoritative pass is the source of truth, the advisory one is a UX/iteration aid.

### C — Verdict-relay only (REJECTED, documented so the reason is on record)
Build the richer worker→orchestrator verdict channel; the orchestrator records what the worker *reports*. Smallest change — but the worker still asserts the verdict, the orchestrator just relays it. **Fails the second clause.** This is the trap: moving *where* a self-reported verdict is recorded is not separation of duties.

## Decision

**Option B — chosen (operator, 2026-07-09).** It's the only one that keeps the cheap in-session iteration loop (a real productivity property, proven across the NET-* arc) while making the *attestation* trustworthy and credential-safe. The worker gate becomes what it should be — a hint to the worker — and the orchestrator gate becomes the authority. Composed with a `checks:attest` scope on the cairn side, the trust boundary is enforced on both ends.

## Build shape (if B is chosen)
1. **Orchestrator-side gate runner** (broker pod): given a completed work-item (ticket + repo + branch), fetch the PR/diff and run pr-exists/pr-substantial/acceptance-judge/test-evidence against it — reuse the existing gate funcs, moved/shared out of the worker-only path.
2. **Move the pull-checks recorder** from `agentfunnel -builder` to the orchestrator; drop `CW_PULL_TLS_*` from `acceptanceGateEnvKeys` (worker no longer needs it — closes the env-forwarding widening).
3. **cairn `checks:attest` scope** (cwb-proto + cairn server): `RecordPullCheck` requires `checks:attest`, not `repo:write`; grant it only to the orchestrator's identity. (This is the cairn-side half; can land independently.)
4. Worker gates stay as advisory reprompts, recording nothing.
5. The worker→orchestrator "richer verdict" channel (`wake.go`'s noted gap) is **optional** for B — the orchestrator re-derives from the artifact — but building it lets the advisory verdict ride up for observability. Nice-to-have, not a blocker.

## Open sub-questions for the build
- Does the orchestrator gate run synchronously on job-done, or as a drain-pass step? (Latency vs simplicity.)
- Re-dispatch policy when the orchestrator gate fails a run the worker's advisory gate passed (divergence = a bug in one of them; log loudly).
- The acceptance-judge LLM call orchestrator-side = broker-pod token spend; which brain? (Cheap judge tier per MODEL-SELECTOR.)
