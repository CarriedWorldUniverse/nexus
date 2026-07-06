package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// TestFilterByRole covers the client-side role filter applied in
// runWorkersSubcommand before the table renderer. Key properties: empty
// role is passthrough (never an actual filter), match is case-sensitive,
// no-match returns an empty slice (not nil) so range-on-result is safe.
func TestFilterByRole(t *testing.T) {
	now := time.Now().UTC().Round(time.Second)
	rows := []workerstatus.Status{
		{Agent: "a1", Role: "builder", WorkItemID: "wi-1", LastHeartbeat: now},
		{Agent: "a2", Role: "forge", WorkItemID: "wi-2", LastHeartbeat: now},
		{Agent: "a3", Role: "builder", WorkItemID: "wi-3", LastHeartbeat: now},
		{Agent: "a4", Role: "wren", WorkItemID: "wi-4", LastHeartbeat: now},
	}

	// Empty role: passthrough — all rows returned, same slice.
	got := filterByRole(rows, "")
	if len(got) != 4 {
		t.Fatalf("empty role: got %d rows, want 4", len(got))
	}

	// Exact match.
	got = filterByRole(rows, "builder")
	if len(got) != 2 {
		t.Fatalf("role=builder: got %d rows, want 2", len(got))
	}
	if got[0].Agent != "a1" || got[1].Agent != "a3" {
		t.Errorf("role=builder: agents = %v, want [a1 a3]",
			[]string{got[0].Agent, got[1].Agent})
	}

	// Case-sensitive: no match for lowercase.
	got = filterByRole(rows, "Builder")
	if len(got) != 0 {
		t.Fatalf("role=Builder (case-mismatch): got %d rows, want 0", len(got))
	}

	// No-match role returns empty slice, not nil — caller should not panic
	// on range.
	got = filterByRole(rows, "nonexistent")
	if len(got) != 0 {
		t.Fatalf("role=nonexistent: got %d rows, want 0", len(got))
	}

	// Nil input with no role: nil passthrough.
	got = filterByRole(nil, "")
	if got != nil {
		t.Errorf("nil input, empty role: got %v, want nil", got)
	}

	// Nil input with a role: still empty (not nil), since we built a slice.
	got = filterByRole(nil, "builder")
	if len(got) != 0 {
		t.Fatalf("nil input, role=builder: got %d rows, want 0", len(got))
	}
}

// TestRunWorkersSubcommand_RoleFilter drives the subcommand end-to-end
// against a real temp-dir DB and confirms the --role flag trims the
// printed output to only the matching agents.
func TestRunWorkersSubcommand_RoleFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	store := workerstatus.NewSQLStore(db)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := time.Now().UTC().Round(time.Second)
	for _, s := range []workerstatus.Status{
		{Agent: "builder-1", Role: "builder", WorkItemID: "wi-1", State: "running", LastHeartbeat: now, Turns: 1, TokensUsed: 100},
		{Agent: "forge-1", Role: "forge", WorkItemID: "wi-2", State: "idle", LastHeartbeat: now, Turns: 0, TokensUsed: 0},
		{Agent: "builder-2", Role: "builder", WorkItemID: "wi-3", State: "running", LastHeartbeat: now, Turns: 2, TokensUsed: 200},
	} {
		if err := store.Upsert(context.Background(), s); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	if code := runWorkersSubcommand([]string{"--data-dir", dir, "--role", "builder"}); code != 0 {
		t.Fatalf("runWorkersSubcommand returned %d, want 0", code)
	}
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	for _, want := range []string{"builder-1", "builder-2", "wi-1", "wi-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q with --role=builder; got:\n%s", want, out)
		}
	}
	// forge row absent.
	if strings.Contains(out, "forge-1") {
		t.Errorf("output should not contain forge-1 with --role=builder; got:\n%s", out)
	}
	if strings.Contains(out, "wi-2") {
		t.Errorf("output should not contain wi-2 with --role=builder; got:\n%s", out)
	}
}

// TestRunWorkersSubcommand_NoRoleFilter confirms that without --role, all
// agents appear in the output (regression guard: a bug in the filter
// plumbing shouldn't silently drop rows when no filter is requested).
func TestRunWorkersSubcommand_NoRoleFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	store := workerstatus.NewSQLStore(db)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := time.Now().UTC().Round(time.Second)
	for _, s := range []workerstatus.Status{
		{Agent: "builder-1", Role: "builder", WorkItemID: "wi-1", State: "running", LastHeartbeat: now},
		{Agent: "forge-1", Role: "forge", WorkItemID: "wi-2", State: "idle", LastHeartbeat: now},
		{Agent: "wren-1", Role: "wren", WorkItemID: "wi-3", State: "idle", LastHeartbeat: now},
	} {
		if err := store.Upsert(context.Background(), s); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	if code := runWorkersSubcommand([]string{"--data-dir", dir}); code != 0 {
		t.Fatalf("runWorkersSubcommand returned %d, want 0", code)
	}
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	for _, want := range []string{"builder-1", "forge-1", "wren-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q without --role; got:\n%s", want, out)
		}
	}
}

// TestListWorkerStatusRows_RealSQLStore proves `nexus workers`' underlying
// store read against a REAL SQLStore (schema-bootstrapped via storage.Open +
// t.TempDir, like ensure_pool_personalities_test.go) rather than a fake —
// the table this reads (worker_status) is exactly the one the M1 Unit 5
// fix (SendWorkerStatus's Ready() gate + the wsClient.Run reorder in
// agentfunnel main.go) is now able to actually populate.
func TestListWorkerStatusRows_RealSQLStore(t *testing.T) {
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	// Nothing upserted yet — listWorkerStatusRows must still succeed (it
	// runs Migrate itself) and return an empty slice, not an error.
	rows, err := listWorkerStatusRows(context.Background(), db)
	if err != nil {
		t.Fatalf("listWorkerStatusRows (empty): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows before any upsert, got %d", len(rows))
	}

	store := workerstatus.NewSQLStore(db)
	now := time.Now().UTC().Round(time.Second)
	if err := store.Upsert(context.Background(), workerstatus.Status{
		Agent:         "anvil-builder",
		Role:          "builder",
		Personality:   "anvil",
		WorkItemID:    "wi-1",
		State:         "running",
		LastHeartbeat: now,
		Turns:         3,
		TokensUsed:    1200,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	rows, err = listWorkerStatusRows(context.Background(), db)
	if err != nil {
		t.Fatalf("listWorkerStatusRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Agent != "anvil-builder" || rows[0].Role != "builder" || rows[0].WorkItemID != "wi-1" {
		t.Errorf("row = %+v, want agent=anvil-builder role=builder work_item=wi-1", rows[0])
	}
}

func TestPrintWorkerStatusTable(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	rows := []workerstatus.Status{
		{
			Agent:         "anvil-builder",
			Role:          "builder",
			WorkItemID:    "wi-1",
			State:         "running",
			LastHeartbeat: now.Add(-90 * time.Second),
			Turns:         3,
			TokensUsed:    1200,
		},
		{
			Agent: "stale-agent",
			State: "spawning",
			// LastHeartbeat left zero — never reported.
		},
	}
	var buf bytes.Buffer
	printWorkerStatusTable(&buf, rows, now)
	out := buf.String()

	for _, want := range []string{
		"AGENT", "ROLE", "WORK_ITEM", "STATE", "TURNS", "TOKENS", "LAST_HEARTBEAT",
		"anvil-builder", "builder", "wi-1", "running", "3", "1200", "1m30s ago",
		"stale-agent", "spawning", "never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q; got:\n%s", want, out)
		}
	}
}

func TestPrintWorkerStatusTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	printWorkerStatusTable(&buf, nil, time.Now())
	if !strings.Contains(buf.String(), "no workers reporting") {
		t.Errorf("expected empty-fleet message, got: %s", buf.String())
	}
}

func TestTruncateCell(t *testing.T) {
	if got := truncateCell("short", 10); got != "short" {
		t.Errorf("truncateCell(short) = %q, want unchanged", got)
	}
	if got := truncateCell("a-very-long-work-item-id", 10); got != "a-very-lo…" {
		t.Errorf("truncateCell(long) = %q, want truncated with ellipsis", got)
	}
}
