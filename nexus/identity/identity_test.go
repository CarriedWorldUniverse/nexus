package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/nexus-cw/nexus/nexus/storage"
)

// TestInit_FreshDBCreatesIdentity covers the primary path: empty
// nexus_identity table → Init populates it with valid keys + UUID +
// secret of correct sizes.
func TestInit_FreshDBCreatesIdentity(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	id, err := Init(context.Background(), db, false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if id.NexusID == "" {
		t.Error("nexus_id empty")
	}
	if len(id.ServerPublicKey) != ed25519.PublicKeySize {
		t.Errorf("server pubkey wrong size: got %d", len(id.ServerPublicKey))
	}
	if len(id.ServerPrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("server privkey wrong size: got %d", len(id.ServerPrivateKey))
	}
	if len(id.SessionSigningSecret) != sessionSecretSize {
		t.Errorf("session secret wrong size: got %d", len(id.SessionSigningSecret))
	}

	// Verify the keypair is internally consistent (pubkey derived from privkey).
	derivedPub := id.ServerPrivateKey.Public().(ed25519.PublicKey)
	if !id.ServerPublicKey.Equal(derivedPub) {
		t.Error("server pubkey != Public(privkey) — keypair mismatch")
	}
}

// TestInit_RefusesSecondInit ensures the non-force path errors with
// ErrAlreadyInitialized when a row exists. This is the load-bearing
// safety check — accidental re-init regenerates everything and
// silently invalidates all keyfiles.
func TestInit_RefusesSecondInit(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	_, err = Init(context.Background(), db, false)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	_, err = Init(context.Background(), db, false)
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("second Init returned %v; want ErrAlreadyInitialized", err)
	}
}

// TestInit_ForceReplacesExisting verifies that --force regenerates
// everything. Loads pre-force, runs force-init, loads post-force,
// confirms all four fields changed.
func TestInit_ForceReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	first, err := Init(context.Background(), db, false)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	second, err := Init(context.Background(), db, true)
	if err != nil {
		t.Fatalf("force Init: %v", err)
	}

	if first.NexusID == second.NexusID {
		t.Error("nexus_id unchanged after force-init — UUIDs collided or row not replaced")
	}
	if first.ServerPublicKey.Equal(second.ServerPublicKey) {
		t.Error("server pubkey unchanged after force-init")
	}
	// Compare session secrets as byte slices.
	if string(first.SessionSigningSecret) == string(second.SessionSigningSecret) {
		t.Error("session secret unchanged after force-init")
	}

	// Reload from DB to confirm persistence.
	reloaded, err := Load(context.Background(), db)
	if err != nil {
		t.Fatalf("Load after force: %v", err)
	}
	if reloaded.NexusID != second.NexusID {
		t.Errorf("reloaded nexus_id mismatch: got %q want %q", reloaded.NexusID, second.NexusID)
	}
}

// TestLoad_EmptyDBReturnsErrNotInitialized covers the boot path's
// most important error case: operator hasn't run `nexus identity
// init` yet. Boot must fail loud with this sentinel so the wrapper
// can print the init hint.
func TestLoad_EmptyDBReturnsErrNotInitialized(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	_, err = Load(context.Background(), db)
	if !errors.Is(err, ErrNotInitialized) {
		t.Errorf("Load on empty db returned %v; want ErrNotInitialized", err)
	}
}

// TestInit_LoadRoundTrip confirms what's written matches what's read.
// Catches encoding/decoding bugs in the BLOB columns.
func TestInit_LoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	written, err := Init(context.Background(), db, false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	read, err := Load(context.Background(), db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if read.NexusID != written.NexusID {
		t.Errorf("nexus_id mismatch: got %q want %q", read.NexusID, written.NexusID)
	}
	if !read.ServerPublicKey.Equal(written.ServerPublicKey) {
		t.Error("server pubkey mismatch on round-trip")
	}
	if !read.ServerPrivateKey.Equal(written.ServerPrivateKey) {
		t.Error("server privkey mismatch on round-trip")
	}
	if string(read.SessionSigningSecret) != string(written.SessionSigningSecret) {
		t.Error("session secret mismatch on round-trip")
	}
}
