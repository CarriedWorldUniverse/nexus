# Storage abstraction + self-verifying persistence — design spec

**Status:** draft v2 (incorporates plumb's review findings)
**Author:** keel
**Reviewer:** plumb (review filed 2026-05-05)
**Filed for:** ticket #140 (Cutover P1)
**Triggered by:** agent-network broker silent-write incident, 2026-05-04 / 2026-05-05

## 1. Problem

The agent-network broker silently failed chat persistence for two days. `db.prepare(INSERT...).run()` returned a `lastInsertRowid` for messages that never reached disk on the timescale anyone noticed. The mode was undetectable from inside the broker because:

- **The page cache lied.** Writes populated the writer connection's in-memory page cache before they reached durable storage. The broker's same-connection read-back queries were satisfied from that cache and returned the row — but the WAL pages were never fsync'd to the WAL file, and the WAL was never checkpointed into the main DB.
- **Same-connection verification is no verification.** The broker's read-back assertion (`SELECT WHERE id = ?` on the same `db` handle that did the INSERT) hit the writer's page cache, not disk. It would have passed even if the disk write never happened — which is exactly what occurred.
- **External readers saw the truth.** Other processes opening the same DB file via fresh connections saw the last-checkpointed state (frozen at id 9628). The broker's API queries went through the broker's own connection, perpetuating the illusion to the dashboard.
- **The trigger was lock contention.** Two writers (broker + orchestrator) contended for the SQLite write lock. SQLite returned BUSY; the better-sqlite3 layer's `busy_timeout=5000ms` wait + the broker's lack of error-checking on the result combined to make the BUSY invisible at the application layer. (Note: `busy_timeout` itself does NOT return success-with-stale-state — it waits then returns BUSY as an error. The "success" came from the broker not surfacing the error path; the "stale state" came from the page-cache-visibility issue above. Earlier prose conflated these; v2 separates them.)

Two days of chat were lost. The broker IS the substrate for thread-context augmentation post-cutover (per operator, 2026-05-04 #9906) — silent persistence failure is the **worst possible mode** because it degrades agent capability without alarms.

## 2. Goals

Nexus's storage layer must:

1. Be **swappable** — SQLite is the v1 backend; the design must accommodate filesystem-backed, Postgres, KV, anything else without touching consumers.
2. Be **self-verifying** — every write proves it reached storage from a separate handle before reporting success.
3. Be **single-writer** — exactly one process holds the write capability per database; all other consumers route through it.
4. Surface health as a **first-class signal** — `/health` reflects disk-roundtrip status; silent write failure becomes visible within 60 seconds.
5. Have a **schema-as-contract** — the invariants every backend must uphold (monotonic IDs, server-stamped timestamps, FK semantics) are documented; CI exercises failure modes.

## 3. Architecture: the single-writer rule

```
┌─────────────────────────────────────────────────┐
│           Nexus process                         │
│                                                 │
│   ┌──────────────┐                              │
│   │ Storage      │ ← exclusive write handle    │
│   │ owner        │                              │
│   └──────┬───────┘                              │
│          │ in-memory Store interface           │
│          │                                      │
│   ┌──────┴───────────────────────┐             │
│   │ Consumers (in-process):       │             │
│   │  - broker WS handlers         │             │
│   │  - reaper                     │             │
│   │  - replayer                   │             │
│   │  - embedded frame             │             │
│   │  - admin REST                 │             │
│   └───────────────────────────────┘             │
│                                                 │
└─────────────────────────────────────────────────┘
                  ▲
                  │ RPC (future) — out-of-process
                  │ consumers route through the
                  │ Nexus process, not direct DB
```

**No second process opens the same DB for writes.** If a future component needs to write, it does so by RPC into the Nexus process; the Nexus process owns the storage handle.

**Lock acquisition (concrete):**

For SQLite backend, `PRAGMA locking_mode=EXCLUSIVE` is insufficient — it only takes the lock on first write, not at open, so a second process can succeed at startup and only fail much later at first write. Instead:

1. **PID-stamped lockfile** at `<data-dir>/.nexus.lock`. Contents: `{"pid": 12345, "started_at": "2026-05-05T...", "host": "darksoft"}`.
2. **At startup:** if the lockfile exists, read the PID + host. If host matches and PID exists and the running process's executable name matches "nexus" (sanity-check), refuse to start with a clear error pointing at the live PID. Otherwise the lock is stale (crash recovery): log a warning, overwrite, proceed.
3. **PID re-validation on stale-detection:** Windows allows PID reuse; before treating a stale lock as recoverable, also check `started_at` is older than the OS boot time, OR confirm the existing PID's executable doesn't match. Either condition implies the original holder is gone.
4. **Lock release:** SIGTERM handler removes the lockfile cleanly. On SIGKILL or crash, the lockfile is left behind and the next start does the stale-detection above.
5. **Mid-startup crash:** if Nexus crashes between "lockfile written" and "ready to serve", the next start sees a stale lock (PID gone) and recovers cleanly per (3).

For Postgres backend (future): row in a `nexus_writer_lock` table with `(host, pid, lease_until)` columns; lease renewed on a heartbeat; stale leases reaped via `lease_until < now()`.

Lock acquisition is the responsibility of `nexus/storage/lock.go`; backends import it and call `Acquire(dataDir)` before opening their writer handle.

## 4. The Store interface contract

The **Store contract** is a *pattern* each domain repo follows, not a literal shared Go interface. Domain stores have heterogeneous shapes (`chat.Store` deals with messages, `usage.Store` with token records, `sessions.Projection` with session entries) — forcing a uniform `InputRow`/`PersistedRow` would be awkward and lossy. Instead, every domain store independently honors the same contract methods:

- `Insert` — verified write, returns persisted form
- `Get` / domain-specific reads — read-after-write consistent
- `Ping` — health probe with disk-roundtrip

Plus the contract requirements below.

**Reference shape (each domain adapts):**

```go
type Store interface {  // illustrative; chat.Store, usage.Store etc. each define their own
    // Insert persists a row. The implementation MUST verify the row
    // reached durable storage by reading it back from a SEPARATE
    // handle (not the write handle's page cache) AND comparing the
    // read-back content against what was inserted (not just id
    // presence) before returning. Mismatch returns ErrSilentWriteFailure.
    // The returned values are read-back values, not write-call values —
    // never trust the underlying lib's claimed lastInsertRowid without
    // re-reading from a separate connection.
    Insert(ctx context.Context, row InputRow) (PersistedRow, error)

    // Reads — implementations may share a read pool but must guarantee
    // read-after-write consistency following an Insert from the same
    // Nexus process.
    Get(ctx context.Context, id ID) (PersistedRow, error)

    // Ping verifies the storage backend is healthy AND can write end-to-end.
    // Implementation: insert a row in a dedicated probe table, commit
    // through the writer connection, fresh-read it back via a separate
    // handle, compare contents, delete the probe row. Returns the
    // round-trip latency on success.
    //
    // Called by the reaper goroutine every health-pulse-interval
    // (default 30s). Surfaced in /health as `storage.write_ok` +
    // `storage.last_write_ms`. If three consecutive Pings fail,
    // /health reports `storage.degraded` and the reaper logs loud.
    Ping(ctx context.Context) (latency time.Duration, err error)
}
```

### Contract requirements (every backend must satisfy)

**1. Durability barrier on Insert.**
The `Insert` call MUST NOT return success until the write has reached durable storage — not just the writer's page cache, not just the WAL pages, but stable storage that survives process crash. This is the central property the agent-network broker violated.

For SQLite backend: configure `journal_mode=WAL` + `synchronous=FULL`. (NORMAL is not sufficient — it allows WAL frames to be unfsync'd at commit time. FULL fsyncs on every commit.) Alternative: `synchronous=NORMAL` with a forced `PRAGMA wal_checkpoint(TRUNCATE)` after every commit, but FULL is simpler.

For filesystem backend: `O_DSYNC` open flag or `fsync()` per write before returning.

For Postgres backend: `synchronous_commit=on` (default).

The contract is: **process crash immediately after Insert returns nil error MUST NOT lose the row.** Backends that can't guarantee this aren't admissible.

**2. Separate-handle verification.**
The verification read MUST use a connection distinct from the writer's connection. For Go's `database/sql`, this means a separate `*sql.DB` (separate driver state, separate pool, separate page cache view). Sharing a single `*sql.DB` between writer and verifier is insufficient — the page cache is per-`*sql.DB`. CI guard: a unit test asserts the writer and verifier are not the same Go object.

**3. Content verification, not just presence.**
The verification doesn't just check `WHERE id = ?` returns a row. It compares the read-back row's critical fields (CreatedAt, sender/owner, content hash) against the values the caller wanted to insert. Mismatch is also `ErrSilentWriteFailure` (or sibling sentinel). This catches corruption and concurrent-writer cases (which shouldn't happen given §3, but defense-in-depth).

**4. Read-after-write consistency from the same Nexus process.**
After `Insert` returns, any `Get` or domain-read called within the same Nexus process sees the row. This is automatic if the durability barrier is honored AND the read pool sees the same backend; explicitly state it because it's load-bearing for reaper / replayer / dispatcher logic.

**5. Multi-row transactions.**
When a domain operation persists N rows atomically (e.g. recipient fan-out in the future, or a chat message + its FTS index entry), the contract is:
- All rows commit, or none do (atomicity is the backend's job)
- Verification reads back at least one representative row from the transaction, plus a second row chosen by deterministic rule (e.g. lowest id in the batch + highest id), to catch both "transaction didn't commit" and "transaction partially committed" modes
- For a single-row Insert (the common case), this degenerates to one verification read

Today nexus's chat path is single-row per call; multi-row is forward-looking. The contract specifies it now so backends know.

### Migration of existing read-back

Today's `chat.SQLStore.Insert` (chat.go:177-194) does a SELECT after INSERT but uses **the same `*sql.DB`** — which would have been bitten by the agent-network bug if SQLite were configured the same way. Part 2 of the migration (§9) refactors this to the writer + verifier two-pool pattern.

## 5. Read-back-from-separate-handle in detail

**Why this matters specifically:**

The agent-network bug:
```
broker:   db.prepare(INSERT).run()   → lastInsertRowid=9631
broker:   db.prepare(SELECT id WHERE id=9631).get()   → {id: 9631}   ← lies
external: db.prepare(SELECT MAX(id)).get()             → {max: 9628}  ← truth
```

The broker's SELECT was satisfied from its own connection's page cache. The row existed in the connection's view of the WAL — but the WAL pages had not been fsync'd. An external reader opening a fresh handle saw the on-disk state, which was correct but stale.

**The fix:**

Insert verification must use a connection that doesn't share the writer's page cache, AND must compare read-back content against the input. For Go's `database/sql`:

```go
// SQLStore holds two connection pools: writer (single conn,
// exclusive use) and verifier (small pool, used only for
// read-back assertions after Insert). Distinct *sql.DB instances —
// not shared connection objects — because Go's *sql.DB owns its
// own pool and page-cache view.
type SQLStore struct {
    writer   *sql.DB  // MaxOpenConns=1, MaxIdleConns=1
    verifier *sql.DB  // MaxOpenConns=2, separate connection pool
}

func (s *SQLStore) Insert(ctx context.Context, row InputRow) (PersistedRow, error) {
    res, err := s.writer.ExecContext(ctx, "INSERT ...", ...)
    if err != nil {
        return PersistedRow{}, err  // BUSY, constraint violation, etc. surface here
    }
    id, _ := res.LastInsertId()

    // Verify: separate pool, separate connection, fresh page cache view.
    // Compare critical fields against input — not just id-presence.
    var got PersistedRow
    err = s.verifier.QueryRowContext(ctx,
        "SELECT id, from_agent, content, created_at FROM ... WHERE id = ?", id).
        Scan(&got.ID, &got.From, &got.Content, &got.CreatedAt)
    if err == sql.ErrNoRows {
        // Row not found. Write was claimed-success but never persisted.
        return PersistedRow{}, fmt.Errorf("%w: id=%d", ErrSilentWriteFailure, id)
    }
    if err != nil {
        return PersistedRow{}, fmt.Errorf("verify after insert: %w", err)
    }
    // Content comparison: if a concurrent writer or corruption produced
    // a different row at this id, surface that as a mismatch.
    if got.From != row.From || got.Content != row.Content {
        return PersistedRow{}, fmt.Errorf("%w: read-back content differs from input (id=%d)", ErrWriteVerificationMismatch, id)
    }
    return got, nil
}
```

**Sentinels:**

- `ErrSilentWriteFailure` — Insert claimed success, row not present on read-back. Write didn't land.
- `ErrWriteVerificationMismatch` — row present at the claimed id but content differs from input. Concurrent writer (single-writer rule violated), corruption, or driver bug.

Callers detect these specifically (`errors.Is`) and surface them: the broker's HTTP layer returns 500 with a distinguishable error code, not 200 with a fake id. The funnel's `ChatGateway.SendChat` propagates the error so the model sees a tool failure instead of believing the message was delivered.

## 6. Health pulse

The reaper goroutine (cmd/nexus/main.go) gains a storage probe alongside its existing aspect-staleness sweep:

```go
const storageHealthPulseInterval = 30 * time.Second
const storageHealthPulseTimeout  = 5 * time.Second
const storageDegradedAfter        = 3 // consecutive failures

// Storage health state. Use a guarded time.Time rather than atomic
// UnixMilli — NTP corrections / sleep-wake can move the wall clock
// backwards, producing nonsense ages. /health reports age via
// time.Since(state.LastStorageOK), which uses the monotonic clock.
type healthState struct {
    mu                   sync.RWMutex
    lastStorageOK        time.Time
    lastStorageLatencyMs int64
    consecutiveFailures  int
    lastError            string
}

func storageHealthLoop(ctx context.Context, s Store, st *healthState, log *slog.Logger) {
    t := time.NewTicker(storageHealthPulseInterval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            ctx2, cancel := context.WithTimeout(ctx, storageHealthPulseTimeout)
            latency, err := s.Ping(ctx2)
            cancel()
            st.mu.Lock()
            if err != nil {
                st.consecutiveFailures++
                st.lastError = err.Error()
                f := st.consecutiveFailures
                st.mu.Unlock()
                log.Error("storage health pulse failed",
                    "err", err, "consecutive", f)
                if f == storageDegradedAfter {
                    log.Error("STORAGE DEGRADED — write path not landing on disk",
                        "consecutive_failures", f)
                }
            } else {
                st.consecutiveFailures = 0
                st.lastStorageOK = time.Now()
                st.lastStorageLatencyMs = latency.Milliseconds()
                st.lastError = ""
                st.mu.Unlock()
            }
        }
    }
}
```

`/health` reports:

```json
{
  "status": "ok",
  "storage": {
    "write_ok": true,
    "last_write_ms": 12,
    "last_check_age_s": 8,
    "consecutive_failures": 0
  }
}
```

When degraded:

```json
{
  "status": "degraded",
  "storage": {
    "write_ok": false,
    "last_check_age_s": 92,
    "consecutive_failures": 5,
    "last_error": "verify after insert: sql: no rows in result set"
  }
}
```

External monitors (operator dashboard, Tailscale-side health checks) can poll `/health` and alarm on `status != "ok"`.

## 6.5 Degraded behavior — what happens when storage fails

The health pulse turns silent failure into observable failure (good). But a status flag isn't a policy — without explicit behavior, clients keep writing into the void at the application layer (the same failure mode the spec is designed against, displaced one level up). This section specifies what the broker does while degraded.

**Per-Insert behavior (always, regardless of health state):**

- Insert continues to attempt for every incoming write. The verifier-pool read-back catches failures per-write; surface `ErrSilentWriteFailure` / `ErrWriteVerificationMismatch` to the caller. The broker's HTTP and WS layers translate these to 500 / `chat.send` error frames so callers know the message wasn't durable.
- The model (Frame funnel) sees a tool failure on `send_chat` when the verifier mismatch fires. Tool failures are first-class — the model can react (retry, surface to operator) instead of believing a phantom success.

**Per-pulse behavior (reaper-driven):**

- After 3 consecutive Ping failures (`storageDegradedAfter`), `/health` flips from `"ok"` to `"degraded"` with the storage block populated.
- The reaper logs `STORAGE DEGRADED` once on entry to degraded state and once per minute while degraded (avoid log spam).
- External monitors (operator dashboard, future Tailscale-side health checks) poll `/health` and alarm on `status != "ok"`.

**Operator-tunable behavior (defaults shown):**

| Knob | Default | Effect when degraded |
|------|---------|----------------------|
| `storage.degraded.refuse_new_chat` | `false` | When true: broker WS + HTTP `chat.send` handlers refuse new sends with a clear error. Operator turns this on if data integrity matters more than uptime. |
| `storage.degraded.pause_routing` | `false` | When true: broker stops fanning out chat.deliver (no new state churn). Live aspects stop seeing each other's messages. Useful for diagnostic windows. |
| `storage.degraded.exit_on_threshold` | `false` | When true: after N (default 30) consecutive Ping failures, the Nexus process exits 1. Process supervisor (#135) restarts it. Last-resort recovery. |
| `storage.degraded.max_consecutive_before_exit` | `30` | Pulse count for `exit_on_threshold`. At 30s pulse interval, this is 15 minutes of confirmed degradation before self-quarantine. |

All knobs default to false: the spec's *minimum* degraded behavior is "report it; don't change behavior". Operator opts into more aggressive responses based on how much they trust nexus's recovery vs how much they care about not generating phantom-success messages.

**Why opt-in vs default-aggressive:** during a transient backend hiccup (disk-full, brief filesystem slowness), refusing-new-chat creates user-visible degradation that may be worse than the underlying issue. Operator's call.

**Recovery:**
- After a successful Ping, `consecutiveFailures` resets to 0 immediately, `/health` flips back to `"ok"` next pulse.
- If `refuse_new_chat` was set, it un-refuses automatically on health recovery.
- If the process exited via `exit_on_threshold`, the supervisor's restart is the recovery path.

**Cross-reference:** this complements ticket #135 (process supervision). Supervision restarts dead processes; degraded-behavior is what a still-running-but-unhealthy process does.

## 7. Schema-as-contract

A storage backend that wants to host nexus chat must uphold:

| Invariant | Why |
|-----------|-----|
| Monotonic, gapless message IDs | reply_to chains and Lock 6 since-cursor replay assume id ordering reflects creation order |
| Server-stamped CreatedAt | Aspects can't lie about when they sent something |
| FK on reply_to (set-NULL on delete) | Thread-walk works even after parent deletion |
| All writes durable on Insert return | No "phantom rowid" — this is the central lesson |
| Atomic transactions for multi-row writes | Recipient fan-out + persistence in one shot |
| Read-after-write consistency | A consumer reading right after Insert sees the row |

Backends that can't uphold an invariant aren't admissible (e.g. eventually-consistent KV without strong-consistency mode is excluded for v1).

## 8. CI failure-mode coverage

Inject failure modes; verify the system either refuses-to-start, refuses-to-accept-writes, or alerts in /health within 60s. Silent success is a regression.

| Scenario | Expected behavior |
|----------|-------------------|
| Read-only filesystem | Refuse-to-start with clear error |
| Disk full mid-write | Insert returns error, /health degraded within 30s |
| Path doesn't exist | Refuse-to-start |
| Path resolves to a different file mid-run (symlink swap) | Detected via Ping; /health degraded |
| WAL file missing/corrupted | Detected at startup probe; refuse-to-serve |
| Second writer attempts to attach | Second writer refuses-to-start (exclusive lock) |
| Insert returns rowid but row isn't readable | Insert returns ErrSilentWriteFailure — no false success |

Test surface: `nexus/storage/failuremode_test.go`. Each scenario is a tabletest with a setup function that puts the store in the failure state and an assertion on the store's behavior.

## 9. Migration path from current nexus state

Current state (post-2026-05-04 unify-chat-send merge):
- ✅ `chat.Store` is an interface; `chat.SQLStore` is the SQLite impl
- ✅ `usage.Store` is an interface; `usage.SQLStore` is the SQLite impl
- ✅ `chat.SQLStore.Insert` already does a same-handle read-back (becomes the foundation; needs to migrate to separate-handle)
- ❌ `sessions.Projection` is a concrete struct holding `*sql.DB` — needs interface seam
- ❌ Schema bootstrap (storage/schema.sql) is SQLite-specific SQL hard-wired into the SQLStore — needs split: storage-as-interface vs sqlite-specific-bootstrap
- ❌ No Ping method on any Store — needs adding to the interface
- ❌ No health pulse loop — needs adding to cmd/nexus/main.go reaper

Implementation parts (one PR each, stacking):

**Part 1 — Store interface: Ping + ErrSilentWriteFailure.**
Add `Ping(ctx) (latency, err)` to `chat.Store` and `usage.Store`. Add the `ErrSilentWriteFailure` sentinel to a new `nexus/storage/errors.go`. SQLStore implementations get a probe-table + read-back pattern.

**Part 2 — Two-pool SQLStore: separate writer + verifier connections.**
Refactor `chat.SQLStore` and `usage.SQLStore` to hold a writer pool (MaxOpenConns=1) and a verifier pool. Update Insert to verify via the verifier pool. Run integration tests under load.

**Part 3 — sessions.Projection becomes an interface.**
`type Projection interface { ... }`, `type SQLProjection struct { ... }` impl. Same Ping + verifier pattern.

**Part 4 — Health pulse loop in reaper.**
cmd/nexus/main.go gets `storageHealthLoop` running alongside the existing aspect reaper. /health endpoint extended with the storage block.

**Part 5 — Single-writer enforcement at startup.**
SQLite store grabs an exclusive advisory lock at open; refuses to proceed if another holder exists. Lockfile sidecar for portability across backends.

**Part 6 — CI failure-mode tests.**
The matrix from §8.

**Part 7 — Schema bootstrap split.**
`nexus/storage/schema.sql` stays SQLite-specific; `nexus/storage/contract.md` documents the invariants any backend impl must uphold. Bootstrap moves into `nexus/storage/sqlite/` package; a future `nexus/storage/files/` could implement the contract differently.

Cutover-blocking parts: 1, 2, 4 (the persistence-correctness floor). Parts 3, 5, 6, 7 land post-cutover but pre-game-driving.

## 10. Resolved decisions (originally open questions)

Plumb's review (2026-05-05) provided defaults; recording here as the working spec until operator overrides.

1. **Single-writer scope: applies to everything Nexus owns.** Chat, sessions, usage, knowledge, files. Loosening per-store invites the failure mode back via a different door. Knowledge with FTS still benefits from durability guarantees — FTS triggers fire from real-row INSERT, so the same verification path covers them.

2. **Backend-swap timing: SQLite-only for v1.** Document markdown-as-canonical as a future possibility (operator #9909), but the contract gets exercised by one impl first — risk of mis-spec drops once the first backend is shaken out. Second backend (file or otherwise) is post-cutover, post-game-driving work.

3. **Separate writer process: in-process for v1, split-out deferred to distributed-Nexus.** Single-writer-within-Nexus-process is enough for portable cutover. Distributed Nexus (memory: `project_distributed_nexus_endgame.md`) splits storage into its own daemon; that's a separate spec when it lands.

4. **`busy_timeout=0` (fail-fast).** Waiting-and-pretending is exactly what the incident exposed. With single-writer enforced (§3), there should be no contention to wait for — if there is, that's a single-writer rule violation worth surfacing immediately.

5. **Probe table: INSERT/DELETE, not UPDATE-on-fixed-row.** The probe must exercise the same code path as real Inserts to catch the same class of bug; UPDATE doesn't go through the same allocation/indexing path. SQLite handles id reuse fine; probe table's id space is irrelevant. Run periodic VACUUM (or autovacuum incremental) if churn ever becomes measurable.

## 11. Out-of-scope (acknowledged future work)

- **Schema migrations.** The very thing this spec is about (writes that don't land) applies to schema-change writes too. ALTER TABLE / CREATE INDEX during a deploy under contention can fail silently in the same family of ways. Future spec needs to address: pre-migration health gate, transactional-DDL where the backend supports it, post-migration verification. Not v1.
- **Verifier-tested-itself.** A unit test asserting `writer != verifier connection` (and that the verifier sees a fresh page-cache view) guards against accidental regression where someone "simplifies" to one pool. Lands as part of part 2's tests.
- **Multi-process write fan-out.** If Nexus ever splits storage into a separate daemon, the in-process Store interface becomes a stub that RPCs to the daemon. The contract above generalizes — a daemon's Insert verifies via its own separate handle internally — but RPC marshalling and the cross-process transaction model are a separate spec.

## 12. References

- agent-network broker silent-write incident — chat thread 2026-05-04 / 2026-05-05 (#9897-#9938)
- plumb's review of v1: [`2026-05-05-storage-abstraction-spec-review-plumb.md`](./2026-05-05-storage-abstraction-spec-review-plumb.md)
- ticket #140 (Cutover P1: storage abstraction + self-verifying persistence spec)
- memory: `project_chat_is_context_substrate.md` (chat persistence is critical-path)
- memory: `project_cutover_quality_over_speed.md` (cutover gates harder game-driving)
- nexus/chat/chat.go:177-194 — current read-back-after-insert pattern (foundation; needs migration to two-pool per Part 2)
