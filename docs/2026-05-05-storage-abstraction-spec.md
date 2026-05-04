# Storage abstraction + self-verifying persistence — design spec

**Status:** draft v1, awaiting operator review
**Author:** keel
**Filed for:** ticket #140 (Cutover P1)
**Triggered by:** agent-network broker silent-write incident, 2026-05-04 / 2026-05-05

## 1. Problem

The agent-network broker silently failed chat persistence for two days. `db.prepare(INSERT...).run()` returned a `lastInsertRowid` for messages that never reached disk. The mode was undetectable from inside the broker because:

- SQLite + WAL mode + `busy_timeout=5000` swallows `SQLITE_BUSY` by waiting then returning "success" with stale state in the better-sqlite3 layer
- The broker's read-back queries hit the same connection's page cache, so they "saw" the rows that didn't exist on disk
- External readers (other connections, post-restart processes) saw the on-disk state frozen at the last successful checkpoint
- The dashboard's API queries went through the broker's own connection, perpetuating the illusion

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

This is enforceable at startup: the Nexus storage initializer takes an exclusive lock (SQLite `PRAGMA locking_mode = EXCLUSIVE` for SQLite backend; lockfile for FS-backed; row in a coordination table for Postgres). Second-launch sees the lock and refuses to start.

## 4. The Store interface contract

Every backend implements:

```go
type Store interface {
    // Insert persists a row. The implementation MUST verify the row
    // reached durable storage by reading it back from a SEPARATE
    // handle (not the write handle's page cache) before returning.
    // The returned ID and CreatedAt are read-back values, not
    // write-call values — never trust the underlying lib's claimed
    // lastInsertRowid without re-reading.
    Insert(ctx context.Context, row InputRow) (PersistedRow, error)

    // Get / List / Query — read methods. Implementations may share
    // a read pool but must guarantee read-after-write consistency
    // following an Insert from the same Nexus process.
    Get(ctx context.Context, id ID) (PersistedRow, error)
    List(ctx context.Context, q Query) ([]PersistedRow, error)

    // Ping verifies the storage backend is healthy AND can write.
    // Implementation: insert a row in a dedicated probe table,
    // commit, fresh-read it back via a separate handle, delete.
    // Returns the round-trip latency on success.
    //
    // Called by the reaper goroutine every health-pulse-interval
    // (default 30s). Surfaced in /health as `storage.write_ok` +
    // `storage.last_write_ms`. If three consecutive Pings fail,
    // /health reports `storage.degraded` and the reaper logs loud.
    Ping(ctx context.Context) (latency time.Duration, err error)
}
```

**Insert's verification is non-negotiable.** Today's `chat.SQLStore.Insert` already does a SELECT after INSERT (chat.go:177-194) — that pattern becomes mandatory across all stores and is documented as a Store contract requirement.

The verification SELECT MUST use a connection distinct from the INSERT's connection where the underlying driver allows. For SQLite with better-sqlite3, this means a connection-pool with separate read connections; for `database/sql` it's already the case (Go's driver pools).

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

Insert verification must use a connection that doesn't share the writer's page cache. For Go's `database/sql`:

```go
// SQLStore holds two connection pools: writer (single conn,
// exclusive use) and verifier (small pool, used only for
// read-back assertions after Insert).
type SQLStore struct {
    writer   *sql.DB  // MaxOpenConns=1, MaxIdleConns=1
    verifier *sql.DB  // MaxOpenConns=2, separate connection pool
}

func (s *SQLStore) Insert(ctx context.Context, row InputRow) (PersistedRow, error) {
    res, err := s.writer.ExecContext(ctx, "INSERT ...", ...)
    if err != nil {
        return PersistedRow{}, err
    }
    id, _ := res.LastInsertId()

    // Verify: separate pool, separate connection, fresh page cache.
    var got PersistedRow
    err = s.verifier.QueryRowContext(ctx, "SELECT ... WHERE id = ?", id).
        Scan(&got.ID, &got.CreatedAt, ...)
    if err == sql.ErrNoRows {
        return PersistedRow{}, ErrSilentWriteFailure(id)
    }
    if err != nil {
        return PersistedRow{}, fmt.Errorf("verify after insert: %w", err)
    }
    return got, nil
}
```

`ErrSilentWriteFailure` is a sentinel — callers can detect it specifically and surface it (the broker's HTTP layer should return 500 with a distinguishable error code, not 200 with a fake id).

## 6. Health pulse

The reaper goroutine (cmd/nexus/main.go) gains a storage probe alongside its existing aspect-staleness sweep:

```go
const storageHealthPulseInterval = 30 * time.Second
const storageHealthPulseTimeout  = 5 * time.Second
const storageDegradedAfter        = 3 // consecutive failures

// Storage health state, atomic-readable from /health handler.
var (
    lastStorageOK         atomic.Int64    // unix ms
    lastStorageLatencyMs  atomic.Int64
    consecutiveFailures   atomic.Int32
)

func storageHealthLoop(ctx context.Context, s Store, log *slog.Logger) {
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
            if err != nil {
                f := consecutiveFailures.Add(1)
                log.Error("storage health pulse failed",
                    "err", err, "consecutive", f)
                if f == storageDegradedAfter {
                    log.Error("STORAGE DEGRADED — write path not landing on disk",
                        "consecutive_failures", f)
                }
            } else {
                consecutiveFailures.Store(0)
                lastStorageOK.Store(time.Now().UnixMilli())
                lastStorageLatencyMs.Store(latency.Milliseconds())
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

## 10. Open questions for operator review

1. **Single-writer scope** — does this apply only to the chat/sessions DB, or also to the knowledge store, files store, etc.? Current shape is "everything Nexus owns", but knowledge has FTS and might tolerate looser semantics.
2. **Backend swap timing** — markdown-as-canonical (operator #9909) was raised as future possibility; do we want the first non-SQLite impl to be a real artifact, or just keep it as a documented possibility?
3. **Separate writer process** — current spec assumes single-writer-WITHIN-Nexus-process. The natural extension is single-writer-process-period (storage owner is its own daemon). For a portable cutover we keep it in-process; for distributed Nexus (memory `project_distributed_nexus_endgame.md`) it splits out. Confirm this trajectory.
4. **busy_timeout** — current SQLStore inherits SQLite's default. Spec implies we should set `busy_timeout=0` (fail-fast on contention rather than wait-and-pretend). Operator nod that fail-fast is the right tradeoff.
5. **Probe table cleanup** — Ping inserts then deletes. Over years of probes that's a lot of churn. Use a fixed sentinel row (UPDATE rather than INSERT/DELETE) to avoid id-space pollution? Or accept the churn?

## 11. References

- agent-network broker silent-write incident — chat thread 2026-05-04 (#9897-#9908)
- ticket #140 (Cutover P1: storage abstraction + self-verifying persistence spec)
- memory: `project_chat_is_context_substrate.md` (chat persistence is critical-path)
- memory: `project_cutover_quality_over_speed.md` (cutover gates harder game-driving)
- nexus/chat/chat.go:177-194 — current read-back-after-insert pattern (foundation)
