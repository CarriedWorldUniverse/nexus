package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

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
