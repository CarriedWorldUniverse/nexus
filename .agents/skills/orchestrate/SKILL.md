---
name: orchestrate
description: Drain the ledger work queue ONCE — decompose ready goals, dispatch ready leaf tasks, review landed PRs, auto-merge green/low-risk, escalate the rest. Stateless; all state in ledger.
when_to_use: When woken (by shadow-runner heartbeat/event) to advance autonomous work. One drain, then exit.
---

# orchestrate — one drain of shadow's autonomous loop (NEX-642)

You are shadow, woken to drain the ledger work queue ONCE, then exit. You hold no
memory of prior drains — **all state is in ledger**. Re-read truth; never assume.

## Procedure

1. **Snapshot the ready set.** Call ledger `ListReady` for shadow's orchestration
   queue. This set is already skill/category/dependency-aware (no blocked, no
   missing-DoR, no already-claimed units — NEX-645/646). Treat the snapshot as
   fixed for this drain; newly-created/changed issues are handled the NEXT drain.

2. **For each ready unit, classify and act (one unit at a time):**
   - **Goal / epic (no dispatchable leaf children yet)** → DECOMPOSE: break it into
     leaf sub-issues, each created in ledger with `parent_key` set, a clear
     `summary`, a `definition_of_done`, and `skills` tags for routing. Transition
     the goal to `In Progress`. Do NOT dispatch the children this drain — they
     enter the ready set and are picked up next wake.
   - **Leaf task (ready, dispatchable)** → DISPATCH to a builder via the dispatch
     skill (`!dispatch <builder>%<provider> repo=<r> ticket=<ledger-key> …`), using
     its `skills` to pick the builder. **VERIFY ACCEPTANCE** — the broker log shows
     `builder job created` + `Submit returned err=<nil>` + the pod Running (NOT the
     send_chat "ok"). On confirmed acceptance, **immediately transition the unit to
     `In Progress`** (claimed) so it leaves the ready set — THIS is the
     double-dispatch guard. If acceptance fails → escalate (see Gates).

3. **Reconcile dispatched units** (already `In Progress` with a builder run): check
   whether their PR has landed (gh / the run record). If a PR is up → REVIEW it.
   (Interim until NEX-655: you track run→PR state yourself via the broker run log /
   gh; once 655 lands, read the lifecycle from ledger.)

4. **Review → merge-or-escalate (the Gates):**
   - **AUTO-MERGE** only when ALL hold: CI green · single-ticket scope · your review
     found no blocking issue · NOT cross-cutting (no deploy, proto/contract,
     auth/identity, multi-repo, or scope change). Then squash-merge, delete branch,
     transition the ledger unit to `Done`.
   - **ESCALATE + PARK** otherwise (cross-cutting / deploy / proto / auth / scope /
     CI-red / review-found issue / ANY doubt): leave the PR open, transition the unit
     to `Blocked`, log a distinct line `orchestrate: ESCALATION <key> <reason>`, and
     ping the operator (comms). Do NOT merge. Do NOT retry — it waits for the
     operator. **Deploys ALWAYS escalate.**
   - **Builder failed/stalled** (run failed/stalled per NEX-653/654): transition the
     unit back to ready and redispatch-with-feedback ONCE; on a second failure,
     escalate.

5. **Groom (cheap, optional):** close ledger units whose PRs already merged. Nothing
   else.

6. **Exit** when the snapshot is handled, OR if you hit a rate-limit / repeated error
   (stop cleanly — the next heartbeat resumes; partial progress is durable in
   ledger). Report a one-line summary of what you did this drain.

## Hard rules
- One ticket per builder; builders run in parallel; never bundle tickets in a dispatch.
- Transition-on-dispatch is mandatory (the double-dispatch guard).
- When in doubt about a merge, ESCALATE — never merge on uncertainty.
- You are stateless: if something isn't in ledger / git / the run log, it didn't happen.
