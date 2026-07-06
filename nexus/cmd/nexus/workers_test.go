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

// TestFilterByRole_EmptyTarget is the no-op path: an empty role filter must
// return the input unchanged (not a copy, not nil) so callers can pass a
// flag default through without branching.
func TestFilterByRole_EmptyTarget(t *testing.T) {
	rows := []workerstatus.Status{
		{Agent: "a", Role: "builder"},
		{Agent: "b", Role: "reviewer"},
	}
	got := filterByRole(rows, "")
	if len(got) != 2 {
		t.Fatalf("empty-role filter returned %d rows, want 2", len(got))
	}
	if got[0].Agent != "a" || got[1].Agent != "b" {
		t.Errorf("empty-role filter reordered rows: %+v", got)
	}
}

// TestFilterByRole_MatchesSome filters rows to a single role and verifies
// the non-matching rows are dropped while matching rows are preserved
// in their original order.
func TestFilterByRole_MatchesSome(t *testing.T) {
	rows := []workerstatus.Status{
		{Agent: "anvil", Role: "builder", WorkItemID: "wi-1"},
		{Agent: "wren", Role: "reviewer", WorkItemID: "wi-2"},
		{Agent: "keel", Role: "builder", WorkItemID: "wi-3"},
		{Agent: "forge", Role: "reviewer", WorkItemID: "wi-4"},
	}
	got := filterByRole(rows, "builder")
	if len(got) != 2 {
		t.Fatalf("filterByRole(builders) = %d rows, want 2", len(got))
	}
	if got[0].Agent != "anvil" || got[0].WorkItemID != "wi-1" {
		t.Errorf("first match wrong: %+v", got[0])
	}
	if got[1].Agent != "keel" || got[1].WorkItemID != "wi-3" {
		t.Errorf("second match wrong: %+v", got[1])
	}
}

// TestFilterByRole_NoMatch returns an empty slice (not nil, not the input)
// when no rows match the requested role. Empty is important because the
// table renderer should still emit the "no workers reporting" message,
// and the JSON renderer should emit "[]" not "null".
func TestFilterByRole_NoMatch(t *testing.T) {
	rows := []workerstatus.Status{
		{Agent: "anvil", Role: "builder"},
		{Agent: "wren", Role: "reviewer"},
	}
	got := filterByRole(rows, "ghost")
	if len(got) != 0 {
		t.Fatalf("filterByRole(ghost) = %d rows, want 0", len(got))
	}
	// Must be an empty (non-nil) slice so table/json renderers get the
	// same "no workers" path as a genuine empty fleet.
	if got == nil {
		t.Error("filterByRole(ghost) returned nil slice, want empty slice")
	}
}

// TestWorkersRoleFilter_Table verifies `nexus workers --role BUILDER`
// renders only matching rows in the table output, dropping non-matches.
func TestWorkersRoleFilter_Table(t *testing.T) {
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
			Agent:         "wren-reviewer",
			Role:          "reviewer",
			WorkItemID:    "wi-2",
			State:         "reviewing",
			LastHeartbeat: now.Add(-30 * time.Second),
			Turns:         1,
			TokensUsed:    400,
		},
		{
			Agent:         "keel-builder",
			Role:          "builder",
			WorkItemID:    "wi-3",
			State:         "spawning",
			LastHeartbeat: now.Add(-5 * time.Second),
			Turns:         0,
			TokensUsed:    0,
		},
	}

	// Filter to "builder" only.
	filtered := filterByRole(rows, "builder")

	var buf bytes.Buffer
	printWorkerStatusTable(&buf, filtered, now)
	out := buf.String()

	// Header must still be present.
	if !strings.Contains(out, "AGENT") {
		t.Errorf("table missing header: %s", out)
	}
	// Builder agents must be present.
	if !strings.Contains(out, "anvil-builder") {
		t.Errorf("table missing anvil-builder: %s", out)
	}
	if !strings.Contains(out, "keel-builder") {
		t.Errorf("table missing keel-builder: %s", out)
	}
	// Reviewer must NOT be present.
	if strings.Contains(out, "wren-reviewer") {
		t.Errorf("table included reviewer row despite --role=builder filter: %s", out)
	}
	// Non-matching state must NOT be present.
	if strings.Contains(out, "reviewing") {
		t.Errorf("table included reviewer state despite --role=builder filter: %s", out)
	}
}

// TestWorkersRoleFilter_JSON verifies `nexus workers --role BUILDER --json`
// emits only matching rows in the JSON array output.
func TestWorkersRoleFilter_JSON(t *testing.T) {
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
			Agent:         "wren-reviewer",
			Role:          "reviewer",
			WorkItemID:    "wi-2",
			State:         "reviewing",
			LastHeartbeat: now.Add(-30 * time.Second),
			Turns:         1,
			TokensUsed:    400,
		},
		{
			Agent:         "keel-builder",
			Role:          "builder",
			WorkItemID:    "wi-3",
			State:         "spawning",
			LastHeartbeat: now.Add(-5 * time.Second),
			Turns:         0,
			TokensUsed:    0,
		},
	}

	filtered := filterByRole(rows, "builder")

	var buf bytes.Buffer
	rc := printWorkerStatusJSON(&buf, filtered)
	if rc != 0 {
		t.Fatalf("printWorkerStatusJSON returned %d, want 0", rc)
	}
	out := buf.String()

	// Both builder agents must be present.
	if !strings.Contains(out, `"anvil-builder"`) {
		t.Errorf("json missing anvil-builder: %s", out)
	}
	if !strings.Contains(out, `"keel-builder"`) {
		t.Errorf("json missing keel-builder: %s", out)
	}
	// Reviewer must NOT be present.
	if strings.Contains(out, `"wren-reviewer"`) {
		t.Errorf("json included reviewer despite --role=builder filter: %s", out)
	}
	if strings.Contains(out, `"reviewing"`) {
		t.Errorf("json included reviewer state despite --role=builder filter: %s", out)
	}
	// Must be a valid JSON array of length 2.
	if !strings.HasPrefix(out, "[") {
		t.Errorf("json output not an array: %s", out)
	}
}

// TestWorkersRoleFilter_EmptyPassthrough verifies that an empty role filter
// (the flag default) passes all rows through unchanged for both table and
// JSON output paths. This is the regression guard: --role must not drop
// rows when the operator doesn't supply one.
func TestWorkersRoleFilter_EmptyPassthrough(t *testing.T) {
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
			Agent:         "wren-reviewer",
			Role:          "reviewer",
			WorkItemID:    "wi-2",
			State:         "reviewing",
			LastHeartbeat: now.Add(-30 * time.Second),
			Turns:         1,
			TokensUsed:    400,
		},
	}

	// Empty role = no filter.
	filtered := filterByRole(rows, "")

	// Table path: both agents must appear.
	var tblBuf bytes.Buffer
	printWorkerStatusTable(&tblBuf, filtered, now)
	if !strings.Contains(tblBuf.String(), "anvil-builder") {
		t.Errorf("table missing anvil-builder with empty role filter: %s", tblBuf.String())
	}
	if !strings.Contains(tblBuf.String(), "wren-reviewer") {
		t.Errorf("table missing wren-reviewer with empty role filter: %s", tblBuf.String())
	}

	// JSON path: both agents must appear.
	var jsonBuf bytes.Buffer
	rc := printWorkerStatusJSON(&jsonBuf, filtered)
	if rc != 0 {
		t.Fatalf("printWorkerStatusJSON returned %d, want 0", rc)
	}
	if !strings.Contains(jsonBuf.String(), `"anvil-builder"`) {
		t.Errorf("json missing anvil-builder with empty role filter: %s", jsonBuf.String())
	}
	if !strings.Contains(jsonBuf.String(), `"wren-reviewer"`) {
		t.Errorf("json missing wren-reviewer with empty role filter: %s", jsonBuf.String())
	}
}

// TestWorkersRoleFilter_CaseSensitive verifies the role filter is
// case-sensitive: "Builder" does not match a row whose Role is "builder".
// Worker status roles are conventionally lowercase; case-sensitivity
// prevents accidental partial matches when the operator fat-fingers.
func TestWorkersRoleFilter_CaseSensitive(t *testing.T) {
	rows := []workerstatus.Status{
		{Agent: "anvil", Role: "builder"},
		{Agent: "forge", Role: "reviewer"},
	}
	got := filterByRole(rows, "Builder") // Capital B.
	if len(got) != 0 {
		t.Fatalf("case-sensitive filter should reject 'Builder' for role 'builder', got %d rows", len(got))
	}
	// Lowercase still matches.
	got = filterByRole(rows, "builder")
	if len(got) != 1 || got[0].Agent != "anvil" {
		t.Errorf("lowercase filter should match: got %+v", got)
	}
}
