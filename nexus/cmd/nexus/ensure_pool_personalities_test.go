package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// TestEnsurePoolPersonalities proves the pool personalities self-provision
// against a REAL aspects SQLStore (schema-bootstrapped via storage.Open, like
// the aspects package's own tests): every personality a worker `<personality>-
// <role>` resolves to — which must be a real, non-retired aspects row — is
// inserted when missing and left alone when already present. Credential
// provisioning (nil credStore here) is exercised in the live path; this covers
// the row-provisioning contract.
func TestEnsurePoolPersonalities(t *testing.T) {
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()
	store := aspects.NewSQLStore(db)
	ctx := context.Background()
	logger := slog.Default()

	// Absent → all personalities inserted, active.
	for _, p := range aspects.WorkerPersonalities {
		if a, _ := store.Get(ctx, p); a != nil {
			t.Fatalf("personality %q should not exist yet", p)
		}
	}
	ensurePoolPersonalities(ctx, store, nil, logger)
	for _, p := range aspects.WorkerPersonalities {
		a, err := store.Get(ctx, p)
		if err != nil || a == nil {
			t.Fatalf("personality %q not provisioned: %v", p, err)
		}
		if a.Status != aspects.StatusActive {
			t.Errorf("personality %q status = %v, want active", p, a.Status)
		}
	}
	t.Logf("provisioned personalities: %v", aspects.WorkerPersonalities)

	// shadow is the orchestrator, never a worker personality → not provisioned.
	if a, _ := store.Get(ctx, "shadow"); a != nil {
		t.Errorf("shadow should NOT be provisioned as a pool personality")
	}

	// Present → idempotent (no error, no duplicate).
	ensurePoolPersonalities(ctx, store, nil, logger)
	for _, p := range aspects.WorkerPersonalities {
		if a, err := store.Get(ctx, p); err != nil || a == nil {
			t.Fatalf("personality %q disappeared after second ensure: %v", p, err)
		}
	}
}
