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
	"encoding/json"
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

// runWorkersSubcommand parses `nexus workers [--data-dir DIR] [--json] [--role ROLE]` and
// prints the consolidated worker_status fleet. Default renders a fixed-width
// table; --json writes a pretty-printed JSON array of worker status objects
// (same shape as GET /api/admin/workers) for programmatic consumption.
// --role filters both output paths to rows whose Role matches exactly
// (case-sensitive). Empty role = show all.
func runWorkersSubcommand(args []string) int {
	fs := flag.NewFlagSet("workers", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	jsonOut := fs.Bool("json", false, "emit the fleet as a JSON array (one object per worker) instead of a table")
	role := fs.String("role", "", "filter rows to workers whose role matches exactly (case-sensitive); empty = show all")
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
	if *role != "" {
		rows = filterByRole(rows, *role)
	}
	if *jsonOut {
		return printWorkerStatusJSON(os.Stdout, rows)
	}
	printWorkerStatusTable(os.Stdout, rows, time.Now())
	return 0
}

// filterByRole returns a copy of rows containing only those whose Role
// matches target exactly (case-sensitive). Empty target returns the
// input unchanged (no-op filter) — callers can pass through a flag
// default without branching.
func filterByRole(rows []workerstatus.Status, target string) []workerstatus.Status {
	if target == "" {
		return rows
	}
	out := rows[:0:0] // pre-allocate empty slice of same length to avoid allocs when no matches
	for _, r := range rows {
		if r.Role == target {
			out = append(out, r)
		}
	}
	return out
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
// AGENT / ROLE / WORK_ITEM / STATE / TURNS / TOKENS / LAST_HEARTBEAT (age).
// now is passed in (rather than time.Now() inline) so the age column is
// unit-testable without a real clock.
func printWorkerStatusTable(w io.Writer, rows []workerstatus.Status, now time.Time) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no workers reporting (worker_status is empty)")
		return
	}
	fmt.Fprintf(w, "%-24s %-10s %-16s %-14s %6s %10s %s\n",
		"AGENT", "ROLE", "WORK_ITEM", "STATE", "TURNS", "TOKENS", "LAST_HEARTBEAT")
	for _, r := range rows {
		fmt.Fprintf(w, "%-24s %-10s %-16s %-14s %6d %10d %s\n",
			truncateCell(r.Agent, 24),
			truncateCell(r.Role, 10),
			truncateCell(r.WorkItemID, 16),
			truncateCell(r.State, 14),
			r.Turns,
			r.TokensUsed,
			heartbeatAge(r, now))
	}
}

// printWorkerStatusJSON writes rows as a pretty-printed JSON array on w.
// Empty input writes an empty array ("[]\n"), not null — callers can
// downstream-feed the output through `jq length` without a null-vs-array
// distinction. Uses the Status struct's existing JSON tags (see
// workerstatus.Status in nexus/workerstatus/workerstatus.go), so the
// wire shape matches GET /api/admin/workers one-for-one.
func printWorkerStatusJSON(w io.Writer, rows []workerstatus.Status) int {
	if rows == nil {
		rows = []workerstatus.Status{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		fmt.Fprintf(os.Stderr, "workers: encode json: %v\n", err)
		return 1
	}
	return 0
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
func truncateCell(s string, max int) string {
	if max <= 0 {
		return ""
	}
	// Rune-safe: count Unicode runes, not bytes, so multi-byte UTF-8 never
	// gets corrupted at the truncation boundary. (Cherry-picked from PR #402
	// — the pool pipeline's first real ticket, built by anvil-builder.)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return string(runes[0])
	}
	return string(runes[:max-1]) + "…"
}
