// File-replacement watcher for the nexus.db path.
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
// This package's defence: a goroutine that periodically os.Stat's the path
// and compares the FileInfo against a baseline captured at startup. On
// divergence, log CRIT and signal the supervisor (cancel the parent
// context). The supervisor restarts cleanly; the new process opens
// whatever file is currently visible at the path — automatic recovery.
//
// Cheap: stat-only, no SQL traffic, no fresh DB connection. Catches the
// failure mode within one tick (default 15s).

package storage

import (
	"context"
	"errors"
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
