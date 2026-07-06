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

func TestPrintWorkerStatusJSON(t *testing.T) {
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
	rc := printWorkerStatusJSON(&buf, rows)
	if rc != 0 {
		t.Fatalf("printWorkerStatusJSON returned %d, want 0", rc)
	}
	out := buf.String()

	// Top-level must be a JSON array (starts with '[').
	if !strings.HasPrefix(out, "[") {
		t.Errorf("expected JSON array, got: %s", out)
	}
	// Sanity-check a few field names that come from Status's json tags.
	for _, want := range []string{`"agent": "anvil-builder"`, `"state": "running"`,
		`"agent": "stale-agent"`, `"turns": 3`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output missing %q; got:\n%s", want, out)
		}
	}
	// Zero heartbeat renders as epoch-zero integer (RFC 3339 "1970-01-01T00:00:00Z")
	// or, withomitempty, the field is omitted. Either way the agent+state must
	// still be present.
	if !strings.Contains(out, `"stale-agent"`) {
		t.Errorf("json missing stale-agent: %s", out)
	}
}

func TestPrintWorkerStatusJSON_Empty(t *testing.T) {
	var buf bytes.Buffer
	rc := printWorkerStatusJSON(&buf, nil)
	if rc != 0 {
		t.Fatalf("printWorkerStatusJSON returned %d, want 0", rc)
	}
	if buf.String() != "[]\n" {
		t.Errorf("expected [] for empty input, got: %q", buf.String())
	}
}

func TestPrintWorkerStatusJSON_NilSlice(t *testing.T) {
	// Nil slice (not empty) must also emit "[]\n", matching
	// printWorkerStatusTable's empty-fleet behaviour.
	var buf bytes.Buffer
	var rows []workerstatus.Status
	rc := printWorkerStatusJSON(&buf, rows)
	if rc != 0 {
		t.Fatalf("printWorkerStatusJSON returned %d, want 0", rc)
	}
	if buf.String() != "[]\n" {
		t.Errorf("expected [] for nil slice, got: %q", buf.String())
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
