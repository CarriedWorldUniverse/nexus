# nexus code standards (v0)

**Status:** v0 draft. Operator-authorized 2026-05-15. Read whenever drafting code or designing components.
**Canonical location:** `~/Google Drive/My Drive/nexus/policies/code-standards.md`. Repo mirrors are read-only.
**Drafted:** shadow. **Audience:** every aspect that writes code (shadow, keel, keel-cli, anvil, plumb, forge, future).

This is a short doc of disciplines, not a style guide. Names, formatting, line length, idiomatic conventions per language community. What's listed here is design-shape stuff that keeps the codebase honest as it grows.

---

## 1. Errors as data, not control-flow

Return typed results that pattern-match cleanly. Don't `os.Exit(1)` on recoverable conditions; don't panic where caller could decide.

Bad:
```go
result, err := doWork()
if err != nil {
    os.Exit(1)  // caller has no decision
}
```

Good:
```go
type TurnResult struct {
    Status   TurnStatus // done | partial | blocked | needs-replanning
    Output   string
    Errors   []TurnError
}
result := doWork()
switch result.Status { ... }
```

Rationale: control-flow exits skip cleanup paths (see NEX-96: mailbox-stuck-on-code-1 bug — exact failure mode). Typed results force the caller to handle each branch and run cleanup uniformly.

Applies in spirit, not literally, to languages without sum types. The principle: every error path that affects state must be visible at the call site, not hidden inside `panic`/`os.Exit`/equivalent.

---

## 2. Surface, don't silent

When something unusual happens, log it, surface it, or fail loudly. Silent fallbacks become silent drifts.

The cred-store hard-fail-vs-soft-window debate (NEX-81) is the canonical case: silent fallback to keyfile creds when broker failed means aspects could drift between credential sources with no signal anything was wrong. Operator (and anvil) chose hard-fail. Apply the same logic anywhere "we'll just degrade quietly" is tempting.

Specific anti-patterns:
- Try/except that swallows exceptions without logging.
- Defaults that mask configuration errors ("if the env var's missing, use this fallback") — defaults should fail loud unless the fallback is documented as intentional.
- Retry-forever loops on errors that aren't transient.
- Catching every error type into a generic "something went wrong" log.

---

## 3. Idempotency on state-mutating boundaries

If an operation might be retried (network call, queue pop, restart-recovery), make it idempotent. Idempotency keys are cheap; redoing state changes is expensive.

- Inbox-pop should include a per-msg idempotency record (NEX-96's fix shape).
- Credential rotations should be safe under crash-mid-operation.
- Skill auto-creation should de-duplicate by content hash, not just by name.

Test the retry path explicitly. "Works on the first attempt" isn't proof of idempotency.

---

## 4. Closed enums at meaningful boundaries

When you add a typed enum (status, kind, reason, source), close it deliberately. Adding a value should require revising the enum, not appending ad-hoc.

Example: work-routing v1.2's Status enum closed at 6 (done/partial/blocked/needs-replanning/redirect/refused). At 7 the planner starts coin-flipping between similar values; signal dies. Each new value forces a re-think.

Applies to:
- Funnel ContextMode (global / thread / stateless — three is enough; bolting on a fourth needs a real case).
- bridle.ProviderID (claude-api / openai-api / etc. — each addition matters).
- Workflow states in Jira (To Do / In Progress / Done / Blocked / Needs Replanning).

When you're tempted to "just add one more value," check whether the existing enum should be refactored.

---

## 5. Wire-order matters; preserve it

When events come out of a stream — model chunks, tool calls, lifecycle pulses — keep them in observation order. Don't split into parallel channels that consumers have to interleave.

Anvil's NEX-84 design: same `<-chan Event` for `LineEvent`, `CompactStart`, `ModelChanged`, etc. Consumer pattern-matches on type. A `CompactStart` between two `LineEvent`s should appear *between* them, not in a separate "lifecycle events" sideband channel.

This applies to:
- bridle event sinks
- ACP outbound frame ordering
- chat thread reply ordering
- mailbox queue draining

If two events happened in order X then Y in the underlying system, the consumer should see them in order X then Y.

---

## 6. Config vs secrets — separate them

Config (project_key, default_folder, history_depth, operator_name) lives on the keyfile / aspect.json / per-host settings file. Secrets (API tokens, passwords, encryption keys) live in the credential store, broker-fetched on demand.

Crossing the boundary creates trouble:
- Config in the cred store: audit log spam every fetch ("aspect read project_key — recorded").
- Secrets in the keyfile: rotation requires re-minting + re-syncing every host.

Per NEX-74 epic decisions. When introducing a new field, classify first:
- Is it secret? → cred store.
- Is it per-aspect non-secret config? → keyfile / aspect.json.
- Is it per-deployment config? → nexus settings.

---

## 7. Decomposition discipline (for planners + dispatch)

When breaking a task into hands or sub-dispatches:
- Each subtask must be **self-contained** — hand can execute without follow-up clarification.
- Each subtask must have **falsifiable success criteria** — hand can answer "am I done?" without ambiguity.
- Subtasks should have **explicit scope boundaries** — what's in, what's out.
- Decompose into a **sequence** if multi-step work needs context-stitching, not a single sprawling dispatch.

Decomposition failure → workers return blocked → planner re-decomposes. Bad decomposition reads as "the worker keeps misunderstanding"; usually it's planner-fault, not worker-fault.

See: work-routing v1.2 §4 worker contract.

---

## 8. Test pyramid: unit at the base, live at the tip

Test pyramid by frequency:
- **Unit tests** (lots, fast, cheap) — pure-function logic.
- **Mock-integration** (workhorse) — controlled inputs against a fake of the external dependency.
- **Fixture-replay** (regression net) — recorded real outputs replayed against current code.
- **Live integration** (rare, gated) — real external dependency, manual or nightly.

Live integration tests are not the foundation. If your CI runs live tests on every PR you're paying for token waste and inheriting flakiness. Live runs catch real-world quirks that mocks can't; reserve them for that.

See: NEX-94 (acp-claude-pty test plan) for the canonical layering.

---

## 9. Specs from disk, not from memory

When a spec, policy, or design doc has been recently amended, **re-read it from disk** before acting. Don't trust session memory of "what's currently true."

Counter-example: the cairn v0.1 → v0.3 saga (chat history) where four turns of work landed on stale spec content because I trusted session memory after the spec was amended. Real damage; took re-filing tickets to correct.

This is a process discipline, not a code discipline, but applies adjacent to anything that touches policy enforcement in code (work-routing checks, ContextMode dispatch, etc.).

---

## 10. Document the WHY, not the WHAT

Code comments explain reasoning, not behavior. The code already shows what happens; the comment should explain why this path was chosen, what alternative was rejected and why, what's surprising about it.

Good:
```go
// Hardcoded source = "chat" because bridle.InboxItem.Source isn't
// in the upstream schema; would need a bridle PR + funnel coordination
// to propagate. Captured as NEX-89 if we ever need non-chat sources.
source := "chat"
```

Bad:
```go
// Set source to "chat".
source := "chat"
```

The first comment carries decision context for the next reader. The second adds nothing.

---

## 11. Open-source-ready by default

Anything new in our repos should be writable as if it's going public someday. Apache 2.0, no embedded secrets, no internal-only references that block external use, README explains the surface in 10 minutes.

This doesn't mean over-engineering for hypothetical external users. It means writing code that doesn't *embarrass* us if the repo flips public. The shape's the same; the discipline forces good defaults.

Examples already conforming:
- `acp-claude-pty` (Apache 2.0, standalone repo)
- `agora` (Apache 2.0, standalone repo)
- `bridle` (Apache 2.0)
- `nexus` (operator's call on open-sourcing; written as if it could be)

---

## v0 → v1

This is v0. Run with it for two weeks, surface what doesn't fit, refine to v1. Hold the line until something earns its place by being concrete:
- An example of how the standard would have prevented a real bug.
- An example of where the standard's blocking sensible work.

Both inputs go to operator + co-planners (shadow + keel-cli). v1 = next deliberate revision pass.

Open questions for v1:
- Versioning convention for shared types (semver vs date-tagged vs ad-hoc).
- Logging conventions (structured vs free-form, log levels, what gets traced vs warned vs errored).
- Concurrency primitives (mutex vs channel patterns for what kinds of state).
- Module boundaries (when to split into a new repo vs new package within an existing one).
