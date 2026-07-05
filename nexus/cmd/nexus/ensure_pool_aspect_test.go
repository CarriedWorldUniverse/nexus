package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// TestEnsurePoolAspect proves pool self-provisioning against a REAL aspects
// SQLStore (schema-bootstrapped via storage.Open, like the aspects package's
// own tests): the "pool" parent row — which MintDerivedCredential requires and
// which never self-registers, having no keyfile — is inserted when missing and
// left alone when already present.
func TestEnsurePoolAspect(t *testing.T) {
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()
	store := aspects.NewSQLStore(db)
	ctx := context.Background()
	logger := slog.Default()

	// Absent → inserted.
	if a, _ := store.Get(ctx, "pool"); a != nil {
		t.Fatalf("pool should not exist yet")
	}
	ensurePoolAspect(ctx, store, logger)
	a, err := store.Get(ctx, "pool")
	if err != nil || a == nil {
		t.Fatalf("pool not provisioned: %v", err)
	}
	if a.Status != aspects.StatusActive {
		t.Errorf("pool status = %v, want active", a.Status)
	}
	t.Logf("provisioned pool aspect: status=%s provider=%s model=%s", a.Status, a.Provider, a.Model)

	// Present → idempotent (no error, no duplicate).
	ensurePoolAspect(ctx, store, logger)
	if a2, err := store.Get(ctx, "pool"); err != nil || a2 == nil {
		t.Fatalf("pool disappeared after second ensure: %v", err)
	}
}
