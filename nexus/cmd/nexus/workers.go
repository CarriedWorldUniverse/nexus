// Command `nexus workers` — a direct read of the M1 Unit 5 worker_status
// table (nexus/workerstatus), consolidated fleet state written by every
// worker's heartbeat. Deliberately a plain store read, not an HTTP round
// trip against the admin API: the same posture as test-provider's
// --data-dir store access (openCredentialsStore) — an operator debugging
// a live broker wants the DB truth directly, without an admin-token fight
// (see GET /api/admin/workers for the HTTP-authenticated equivalent this
// mirrors, in nexus/broker/admin.go).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// runWorkersSubcommand parses `nexus workers [--data-dir DIR]` and prints
// the consolidated worker_status fleet as a table.
func runWorkersSubcommand(args []string) int {
	fs := flag.NewFlagSet("workers", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	db, err := storage.Open(context.Background(), *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workers: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	rows, err := listWorkerStatusRows(context.Background(), db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workers: %v\n", err)
		return 1
	}
	printWorkerStatusTable(os.Stdout, rows, time.Now())
	return 0
}

// listWorkerStatusRows opens the workerstatus store against an already-open
// DB and returns every row, most-recently-heartbeated first (List's own
// ORDER BY). Migrate runs first — same idempotent CREATE TABLE IF NOT
// EXISTS the broker runs at boot (nexus/broker/server.go) — so `nexus
// workers` works against a data dir that predates this table (fresh
// install, or a broker binary older than M1 Unit 5) without erroring.
func listWorkerStatusRows(ctx context.Context, db *sql.DB) ([]workerstatus.Status, error) {
	store := workerstatus.NewSQLStore(db)
	if err := store.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate worker_status: %w", err)
	}
	rows, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list worker_status: %w", err)
	}
	return rows, nil
}

// printWorkerStatusTable renders the fleet as a fixed-width table:
// AGENT / ROLE / PERSONALITY / WORK_ITEM / STATE / TURNS / TOKENS / LAST_HEARTBEAT (age).
// now is passed in (rather than time.Now() inline) so the age column is
// unit-testable without a real clock.
func printWorkerStatusTable(w io.Writer, rows []workerstatus.Status, now time.Time) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no workers reporting (worker_status is empty)")
		return
	}
	fmt.Fprintf(w, "%-24s %-10s %-12s %-16s %-14s %6s %10s %s\n",
		"AGENT", "ROLE", "PERSONALITY", "WORK_ITEM", "STATE", "TURNS", "TOKENS", "LAST_HEARTBEAT")
	for _, r := range rows {
		fmt.Fprintf(w, "%-24s %-10s %-12s %-16s %-14s %6d %10d %s\n",
			truncateCell(r.Agent, 24),
			truncateCell(r.Role, 10),
			truncateCell(r.Personality, 12),
			truncateCell(r.WorkItemID, 16),
			truncateCell(r.State, 14),
			r.Turns,
			r.TokensUsed,
			heartbeatAge(r, now))
	}
}

// heartbeatAge renders LastHeartbeat as "<duration> ago", or "never" for a
// zero-valued heartbeat (a row upserted with a malformed/absent timestamp —
// workerstatus.Status.Stale treats zero the same way, as maximally stale).
func heartbeatAge(r workerstatus.Status, now time.Time) string {
	if r.LastHeartbeat.IsZero() {
		return "never"
	}
	age := now.Sub(r.LastHeartbeat).Round(time.Second)
	if age < 0 {
		age = 0
	}
	return age.String() + " ago"
}

// truncateCell keeps a table cell from blowing out fixed-width alignment on
// an unexpectedly long field (e.g. a work_item_id ticket slug). Truncated
// values get a trailing "…" marker so it's visibly not the whole string.
//
// Rune-safe: counts Unicode runes (not bytes) so multi-byte UTF-8 characters
// don't get corrupted at the truncation boundary.
func truncateCell(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return string(runes[0])
	}
	// Truncate to (max-1) runes, then append the ellipsis character.
	// This ensures the output is exactly `max` runes wide.
	return string(runes[:max-1]) + "…"
}