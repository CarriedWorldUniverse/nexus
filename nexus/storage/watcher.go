// File-replacement watcher and fresh-handle write verifier for nexus.db.
//
// Background:
//
// SQLite under any driver holds a long-lived file handle for the lifetime
// of the *sql.DB. If the file at that path is replaced on disk by another
// process (Windows: rename + recreate; POSIX: unlink + recreate) the handle
// stays valid but is now disconnected from the visible file. Writes go to
// the orphaned inode; reads from the same handle stay consistent (the
// process never sees the divergence). External readers — anything that
// opens the file fresh — see the pre-replacement state, frozen.
//
// Same-process read-back assertions cannot detect this: they read from the
// same handle that wrote, so the phantom answers consistently.
//
// We hit this in agent-network on 2026-05-04 / 2026-05-07: broker ran for
// 5 days writing to a phantom; visible runtime/comms.db froze; ~400 chat
// messages existed only in the phantom and were lost on broker restart.
//
// This package's defences (Crossing Parts 1 + 2):
//
//   1. WatchFileReplacement — stat-only goroutine, periodically compares
//      the path's FileInfo against a baseline captured at startup. Cheap
//      (no SQL, no fresh DB open); catches replacement within one tick
//      (default 15s).
//
//   2. WatchWriteDurability — fresh-handle goroutine, periodically opens
//      a separate sql.DB to the same path, reads MAX(id) from
//      chat_messages, compares against the live broker's same query.
//      If the live handle reports a higher max-id than the fresh handle
//      reads, the broker is in phantom mode — its writes haven't reached
//      the visible file. Slower (default 60s) and more expensive
//      (fresh DB connect per tick), but catches subtler write-loss modes
//      that file-replacement detection misses (WAL desync, partial-write
//      rollback, long-handle-with-FS-mismatch).
//
// The two run in parallel: file-replacement is the cheap fast path,
// write-durability is the deeper backstop. On either trip, both call
// the same onReplaced callback (typically signal.NotifyContext's stop)
// so the broker exits cleanly and the supervisor restarts with a fresh
// handle to whatever's currently at the path.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultWatchInterval is how often the file-replacement watcher checks
// the db path for divergence. Tuned to catch a phantom within a minute
// without burning syscalls — phantom mode is rare, the interval is the
// upper bound on detection latency.
const DefaultWatchInterval = 15 * time.Second

// DefaultVerifyInterval is how often the fresh-handle write verifier
// runs. Less frequent than the file-replacement watcher because it
// opens a fresh DB connection (more expensive) and the file-replacement
// watcher catches the load-bearing case faster. The verifier's value
// is catching subtler write-loss modes — WAL desync, partial-write
// rollback, long-handle-with-FS-mismatch — that don't manifest as
// file replacement.
const DefaultVerifyInterval = 60 * time.Second

// transientStatRetryThreshold is how many consecutive stat errors the
// watcher tolerates before treating the path as gone. On Windows
// specifically, antivirus scans and backup tools can transiently hold
// file paths and make os.Stat fail intermittently — a single failed
// tick is not strong enough evidence to shut down the broker. Two
// consecutive failures across an interval gap is.
const transientStatRetryThreshold = 2

// ErrFileReplaced is returned (via the watcher's error channel and via
// supervisor signalling) when the on-disk file at the watched path no
// longer matches the FileInfo captured at watcher start. Indicates the
// process is in phantom-handle mode and should be restarted.
var ErrFileReplaced = errors.New("storage: db file at watched path was replaced — process in phantom-handle mode")

// ErrWriteDurabilityFailed is returned when the live handle and a fresh
// handle disagree on the chat-message MAX(id) — the broker has written
// rows the visible file doesn't see. Same underlying failure mode as
// ErrFileReplaced (phantom handle) but caught via a different signal
// (cross-handle disagreement rather than file-info divergence).
// Indicates the process should be restarted.
var ErrWriteDurabilityFailed = errors.New("storage: live handle and fresh handle disagree on chat MAX(id) — broker writes not durable on disk")

// ResolvePath mirrors the path-resolution logic in Open without opening
// the database. Used by the watcher and any callers that need the
// resolved nexus.db file path independent of the *sql.DB handle.
//
// Resolution order:
//  1. dir argument (may be empty)
//  2. NEXUS_DATA_DIR env var
//  3. DefaultDataDir constant
//
// Returns the absolute-or-relative path to the nexus.db file inside the
// resolved directory. Does not create the directory.
func ResolvePath(dir string) string {
	if dir == "" {
		dir = os.Getenv("NEXUS_DATA_DIR")
	}
	if dir == "" {
		dir = DefaultDataDir
	}
	return filepath.Join(dir, DBFileName)
}

// WatchFileReplacement runs until ctx is cancelled. On startup it captures
// a FileInfo baseline for path; on each tick it re-stats the path and
// compares via os.SameFile. If they diverge, the watcher logs CRIT, calls
// onReplaced (if non-nil), and returns ErrFileReplaced.
//
// onReplaced is invoked exactly once before the watcher returns. Callers
// typically pass a context-cancel function so the broker shuts down
// gracefully on detection — the supervisor's auto-restart then opens the
// new visible file.
//
// If the path doesn't exist at startup the watcher returns an error
// without entering the tick loop (Open creates the file before the
// watcher starts in normal startup ordering, so absence here is a real
// configuration error).
//
// interval is the time between stat checks; pass 0 for DefaultWatchInterval.
//
// log may be nil; the watcher will skip logging in that case.
func WatchFileReplacement(ctx context.Context, path string, interval time.Duration, log *slog.Logger, onReplaced func()) error {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}

	baseline, err := os.Stat(path)
	if err != nil {
		return err
	}
	if log != nil {
		log.Info("storage watcher: baseline captured", "path", path, "interval", interval)
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	// Consecutive stat-error counter. Reset on any successful stat.
	// transientStatRetryThreshold consecutive failures are required
	// before we treat the file as gone — single-tick failures are
	// usually antivirus / backup tool path holds, not real replacements.
	statErrStreak := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			current, err := os.Stat(path)
			if err != nil {
				statErrStreak++
				if log != nil {
					log.Warn("storage watcher: stat failed", "path", path, "err", err, "consecutive_failures", statErrStreak)
				}
				if statErrStreak < transientStatRetryThreshold {
					// Transient — likely an AV scan or backup tool
					// briefly holding the path. Keep the baseline,
					// retry next tick.
					continue
				}
				// Sustained failure: file is genuinely gone (or
				// inaccessible long enough that the difference no
				// longer matters). Treat as replacement.
				if log != nil {
					log.Error("storage watcher: stat failed for sustained ticks — treating as replacement",
						"path", path, "last_err", err, "threshold", transientStatRetryThreshold)
				}
				if onReplaced != nil {
					onReplaced()
				}
				return ErrFileReplaced
			}
			statErrStreak = 0
			if !os.SameFile(baseline, current) {
				if log != nil {
					log.Error("storage watcher: file replacement detected — phantom-handle mode active",
						"path", path,
						"baseline_size", baseline.Size(),
						"current_size", current.Size(),
						"baseline_mod", baseline.ModTime(),
						"current_mod", current.ModTime(),
					)
				}
				if onReplaced != nil {
					onReplaced()
				}
				return ErrFileReplaced
			}
		}
	}
}

// WatchWriteDurability runs until ctx is cancelled. Every interval, it
// opens a fresh sql.DB connection to path (separate from the live
// broker's long-lived handle), reads MAX(id) FROM chat_messages, and
// compares against the same query run on the live handle. If the live
// handle reports a higher max-id than the fresh handle reads, the
// broker's writes have not reached the visible file — phantom-handle
// mode active. Logs CRIT, calls onReplaced (if non-nil), returns
// ErrWriteDurabilityFailed.
//
// This is the structural backstop for write-loss modes that don't
// manifest as file replacement: WAL desync, partial-write rollback,
// long-handle-with-FS-mismatch. The file-replacement watcher catches
// the load-bearing case faster; this verifier catches subtler residue.
//
// Tolerates transient query failures (e.g. SQLite busy on either
// handle) the same way as the file-replacement watcher: a single
// failure doesn't trip; transientStatRetryThreshold consecutive
// failures do. Open errors on the fresh handle count as a query
// failure (the path may be transiently inaccessible).
//
// liveDB is the broker's long-lived *sql.DB. interval is the time
// between checks; pass 0 for DefaultVerifyInterval.
//
// log may be nil; onReplaced may be nil.
//
// If MAX(id) is fresh > live (fresh handle sees a higher id than the
// live broker), that's the OPPOSITE of phantom mode — another writer
// has touched the file. Log Warn and continue; the live broker can
// still be working correctly even if external writes happened. Only
// live > fresh is the phantom signature.
func WatchWriteDurability(ctx context.Context, path string, liveDB *sql.DB, interval time.Duration, log *slog.Logger, onReplaced func()) error {
	if interval <= 0 {
		interval = DefaultVerifyInterval
	}
	if liveDB == nil {
		return errors.New("storage: WatchWriteDurability requires non-nil liveDB")
	}

	if log != nil {
		log.Info("storage verifier: starting fresh-handle write durability check", "path", path, "interval", interval)
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	queryFailStreak := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			liveMax, freshMax, qerr := compareMaxID(ctx, path, liveDB)
			if qerr != nil {
				queryFailStreak++
				if log != nil {
					log.Warn("storage verifier: comparison query failed", "err", qerr, "consecutive_failures", queryFailStreak)
				}
				if queryFailStreak < transientStatRetryThreshold {
					continue
				}
				// Sustained failure: treat as durability failed —
				// either the fresh handle can't be opened (file
				// inaccessible long enough that the broker is in
				// effect running unguarded) or both handles are in
				// trouble. Either way, restart is the right call.
				if log != nil {
					log.Error("storage verifier: query failed for sustained ticks — treating as durability failure",
						"path", path, "last_err", qerr, "threshold", transientStatRetryThreshold)
				}
				if onReplaced != nil {
					onReplaced()
				}
				return ErrWriteDurabilityFailed
			}
			queryFailStreak = 0

			if liveMax > freshMax {
				if log != nil {
					log.Error("storage verifier: live handle reports higher MAX(id) than fresh handle — phantom-handle mode active",
						"path", path,
						"live_max_id", liveMax,
						"fresh_max_id", freshMax,
						"divergence", liveMax-freshMax,
					)
				}
				if onReplaced != nil {
					onReplaced()
				}
				return ErrWriteDurabilityFailed
			}
			if liveMax < freshMax {
				// External writer scenario — fresh handle sees writes
				// that the live broker never made. Not phantom mode
				// (the live broker isn't lying about its writes); but
				// indicates concurrent access, which has its own risk
				// class. Log Warn and continue.
				if log != nil {
					log.Warn("storage verifier: fresh handle reports higher MAX(id) than live — external writer detected",
						"path", path,
						"live_max_id", liveMax,
						"fresh_max_id", freshMax,
					)
				}
			}
			// liveMax == freshMax: writes are durable, broker healthy.
		}
	}
}

// compareMaxID runs MAX(id) FROM chat_messages on both the live handle
// and a fresh sql.DB opened to path, returning both values and any
// error. Used by WatchWriteDurability; factored out for testability.
//
// The fresh DB connection uses a read-only DSN — we don't want to
// mutate the file from this verifier path.
//
// Caveats:
//
//   - On Windows, SQLite's WAL mode requires the -shm file to exist
//     before a read-only connection can attach to a WAL database. In
//     normal operation the live broker creates -shm at Open, so by the
//     time the first verifier tick fires the file is present. The
//     edge case (verifier tick before live broker writes its first
//     WAL frame) is absorbed by the queryFailStreak threshold.
//
//   - sql.Open with the ncruces driver does not connect immediately —
//     it returns a pool config and defers the actual connection until
//     the first query. So an "open failure" (file missing, permission
//     denied) surfaces as a Ping/Query error rather than from sql.Open
//     itself. We Ping eagerly to attribute that error correctly in
//     logs, since the verifier's error stream is the only signal an
//     operator has into "is the path even reachable?"
func compareMaxID(ctx context.Context, path string, liveDB *sql.DB) (liveMax, freshMax int64, err error) {
	// Live query first. Cheap; no I/O beyond what's already memory-mapped.
	const q = "SELECT COALESCE(MAX(id), 0) FROM chat_messages"
	if err = liveDB.QueryRowContext(ctx, q).Scan(&liveMax); err != nil {
		return 0, 0, fmt.Errorf("live MAX(id) query: %w", err)
	}

	// Fresh handle. mode=ro skips write-locking; the file is being
	// concurrently written by the live broker.
	uriPath := filepath.ToSlash(path)
	dsn := "file:" + uriPath + "?mode=ro&_pragma=busy_timeout(2000)"
	fresh, err := sql.Open("sqlite3", dsn)
	if err != nil {
		// sql.Open errors on this driver are essentially DSN parse
		// errors, which we don't expect to vary per-tick. Still
		// caught for completeness.
		return liveMax, 0, fmt.Errorf("fresh sql.Open: %w", err)
	}
	defer fresh.Close()

	// Ping eagerly to surface file-not-found / permission errors
	// with the right attribution, before the query path conflates
	// them with query-execution failures.
	if err = fresh.PingContext(ctx); err != nil {
		return liveMax, 0, fmt.Errorf("fresh ping: %w", err)
	}

	if err = fresh.QueryRowContext(ctx, q).Scan(&freshMax); err != nil {
		return liveMax, 0, fmt.Errorf("fresh MAX(id) query: %w", err)
	}

	return liveMax, freshMax, nil
}
