# Review: Storage abstraction + self-verifying persistence spec

**Reviewer:** Plumb
**Document under review:** [`2026-05-05-storage-abstraction-spec.md`](./2026-05-05-storage-abstraction-spec.md)
**Spec author:** Keel
**Review filed:** 2026-05-05

---

The spec's bones are good — single-writer + separate-handle verification + first-class health pulse is the right architecture for the bug class it's solving. These are refinements / gaps, not "redo it."

## What's strong (worth naming so they stay)

- Concrete motivation tied to a real 2-day data-loss incident.
- Single-writer as a load-bearing principle with startup-time enforcement.
- Self-verifying writes via separate connection pool — exactly the right answer for the page-cache-visibility class of bug.
- Health pulse with `consecutive_failures` and `/health` degraded state — turning silent failure into a first-class signal is the entire point.
- Schema-as-contract — only sane way to plan for backend-swap.
- Migration broken into landable parts with cutover-blocking vs post-cutover sequenced.

## Findings

### [HIGH] Bug-attribution in §1 is technically incorrect — and that matters

§1 says SQLite + WAL + `busy_timeout=5000` "swallows SQLITE_BUSY by waiting then returning 'success' with stale state." That's not how `busy_timeout` works — it waits N ms then returns SQLITE_BUSY (an error). It doesn't return success-with-stale-state.

The actual failure was almost certainly: WAL pages weren't fsync'd, the writer's connection saw them in its page cache, external readers saw the last checkpointed file. The chosen fix (separate-pool verifier) is **correct regardless** because it's a stronger property — but the prose explanation will mislead future readers diagnosing similar bugs.

**Fix:** rewrite §1's mechanism description in terms of WAL durability + page-cache visibility, drop the busy_timeout claim. The headline "page cache lied to us" is the real lesson.

### [HIGH] Separate handle alone isn't sufficient — durability needs explicit specification

The two-pool pattern is necessary but not sufficient. Two `*sql.DB` opening the same SQLite file can both see WAL state that hasn't been fsync'd, depending on `synchronous` mode and checkpoint state. The spec doesn't mandate `synchronous=FULL` or address fsync ordering.

**Fix:** add to §4 contract: "writes are durable on Insert return" must be backed by an explicit durability barrier — for SQLite that's `journal_mode=WAL` + `synchronous=FULL` (or NORMAL with explicit checkpoint after every write). State the requirement abstractly so non-SQLite backends know what they're satisfying.

### [HIGH] "What happens when degraded?" isn't specified

The health pulse turns silent failure into observable failure — good. But what's the broker's behavior while degraded?

- Do new Inserts continue to be attempted (and individually fail)?
- Are WS connections paused / refused?
- Is there a backpressure mechanism?
- Does the broker self-quarantine?

This is the same failure mode being designed against, just one level up. Without an explicit policy, "degraded" is just a status flag — clients can keep writing into the void.

**Fix:** add §6.5 "Degraded behavior" specifying: Insert continues to attempt and surface ErrSilentWriteFailure; broker WS handlers MAY refuse new chat after N consecutive failures (operator-tunable); /health degraded triggers external alarm. Don't leave it implicit.

### [MEDIUM] ErrSilentWriteFailure semantics are underspecified

§5 verifies by id presence (`if err == sql.ErrNoRows`). That catches "row never landed" but not "row landed with wrong values" (corruption, concurrent writer in a single-writer-violated case, etc.).

**Fix:** verifier should re-read the row's content and compare critical fields (CreatedAt, key columns) against what was inserted. Mismatch → also `ErrSilentWriteFailure` (or a sibling error). Contract is then "verified to be the row I just wrote", not just "a row exists with this id."

### [MEDIUM] Single-writer lock implementation isn't concrete

§3 says "exclusive lock at startup" with examples per backend. But:

- SQLite `PRAGMA locking_mode=EXCLUSIVE` only takes the lock on first write, not at open — second process could open successfully and only fail much later.
- Lockfile sidecar — what about stale lockfiles from ungraceful crash? PID-based detection? `flock` style?
- If Nexus crashes mid-startup with the lock held, the next start fails until manually cleared.

**Fix:** §5 (Part 5 in §9) needs a concrete lock-acquisition procedure including stale-detection and crash-recovery, or explicitly defer with a comment that Part 5 implementation will spec it.

### [MEDIUM] Multi-row transaction verification isn't addressed

§7 invariant "atomic transactions for multi-row writes" + §5 verification example only covers single-row. If recipient fan-out commits N rows in a transaction, the spec doesn't say how the verification works:

- Verify each row? (N round-trips after a single COMMIT)
- Verify the transaction's outcome by querying an id range?
- Verify a representative subset?

**Fix:** specify how multi-row commits are verified, or constrain the API to single-row Inserts only and require callers to chain.

### [MEDIUM] Multi-store pattern vs single Store interface

§4 shows `type Store interface { Insert / Get / List / Ping }` as if it's one shared interface. §9 then references `chat.Store`, `usage.Store`, `sessions.Projection` as separate things each adding Ping.

**Fix:** clarify whether §4 is a *pattern* each domain repo follows, or a literal shared interface that all domain stores embed. Latter forces uniform `InputRow`/`PersistedRow` across domains which is awkward; former is more honest.

### [LOW] Health-pulse clock handling

§6 uses `atomic.Int64` of UnixMilli for `lastStorageOK`. NTP corrections / sleep-wake can move the clock backwards, producing nonsense `last_check_age_s`. Minor but worth fixing.

**Fix:** use `time.Now()` returning `time.Time`, store as a guarded `time.Time` (mutex-protected; not hot-path), report age via `time.Since`.

### [LOW] Probe-table cleanup (§10 #5 — answered)

Keep INSERT/DELETE rather than UPDATE-on-fixed-row.

- Probe must exercise the same code path as real Inserts to catch the same class of bug. UPDATE doesn't go through the same allocation/indexing path.
- SQLite handles id reuse fine — probe table's id space is irrelevant.
- Run periodic VACUUM (or autovacuum incremental) if churn ever becomes measurable.

## Out-of-scope (worth acknowledging in spec)

- **Schema migrations** — the very thing this spec is about (writes that don't land) applies to schema changes too. Should be acknowledged as future work, not silently absent.
- **Tests for the verifier itself** — a unit test asserting `writer != verifier connection` guards against accidental regression where someone "simplifies" to one pool.

## Operator-question takes (§10)

1. **Single-writer scope** — apply to everything Nexus owns. Knowledge with FTS still benefits from durability guarantees; loosening per-store invites the failure mode back via a different door.
2. **Backend swap timing** — keep markdown-as-canonical documented but don't ship the second backend as part of P1. Risk of mis-spec is lower if the contract gets exercised by one impl first.
3. **Separate writer process** — agree with in-process for v1; defer split-out to distributed-Nexus work.
4. **`busy_timeout=0`** — yes, fail-fast. Waiting-and-pretending is exactly what the incident exposed.
5. **Probe table cleanup** — covered in [LOW] above; INSERT/DELETE.

## Summary

- 3 HIGH (bug-attribution prose; durability beyond two pools; degraded-state policy)
- 4 MEDIUM (verification rigor; lock specifics; multi-row commits; interface vs pattern)
- 2 LOW (clock; probe cleanup)
- Plus operator-question takes

Spec's architecture is the right answer; these are sharpening, not redirecting.
