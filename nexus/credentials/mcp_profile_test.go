package credentials

import (
	"context"
	"testing"
)

// TestMCPProfile_SetGetRoundTrip verifies the simplest path: Set a
// profile blob, Get it back verbatim. Stored as opaque JSON text — the
// store doesn't parse or validate the shape (the agent funnel does).
func TestMCPProfile_SetGetRoundTrip(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO aspects (name) VALUES ('forge')`); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}

	const blob = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"${credential:gh-pat.key}"}}}}`
	if err := s.SetMCPProfile(ctx, "forge", blob); err != nil {
		t.Fatalf("SetMCPProfile: %v", err)
	}
	got, err := s.GetMCPProfile(ctx, "forge")
	if err != nil {
		t.Fatalf("GetMCPProfile: %v", err)
	}
	if got != blob {
		t.Errorf("round-trip: got %q want %q", got, blob)
	}
}

// TestMCPProfile_Upsert verifies Set replaces the prior profile rather
// than INSERT-conflicting. Operator edits a profile repeatedly during
// setup; each edit must land cleanly.
func TestMCPProfile_Upsert(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO aspects (name) VALUES ('forge')`); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}
	if err := s.SetMCPProfile(ctx, "forge", `{"v":1}`); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := s.SetMCPProfile(ctx, "forge", `{"v":2}`); err != nil {
		t.Fatalf("second set: %v", err)
	}
	got, err := s.GetMCPProfile(ctx, "forge")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != `{"v":2}` {
		t.Errorf("got %q want second value", got)
	}
}

// TestMCPProfile_GetMissingReturnsEmpty verifies Get returns ("", nil)
// for an aspect that has no profile row. Callers treat absent and
// empty-profile identically — no need to differentiate at the API.
func TestMCPProfile_GetMissingReturnsEmpty(t *testing.T) {
	s, _ := newTestStore(t)
	got, err := s.GetMCPProfile(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("GetMCPProfile on missing aspect: %v", err)
	}
	if got != "" {
		t.Errorf("missing profile: got %q want empty string", got)
	}
}
