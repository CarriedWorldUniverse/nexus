package operator

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func openTestStore(t *testing.T) *PasskeyStore {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPasskeyStore(db)
}

// fakeCred returns a credentialID + publicKey pair distinct enough
// for round-trip and uniqueness checks. Real credentials are 32-256
// bytes; we use short sentinel strings here for test legibility.
func fakeCred(seed string) (cred, pub []byte) {
	return []byte("cred-" + seed), []byte("pub-" + seed)
}

func TestRegisterAndGetByCredentialID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("alpha")

	id, err := s.Register(ctx, cred, pub, "little-blue", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if id == 0 {
		t.Error("Register returned id=0")
	}

	got, err := s.GetByCredentialID(ctx, cred)
	if err != nil {
		t.Fatalf("GetByCredentialID: %v", err)
	}
	if got.ID != id {
		t.Errorf("id mismatch: got %d want %d", got.ID, id)
	}
	if !bytes.Equal(got.CredentialID, cred) {
		t.Errorf("credential_id mismatch: got %q want %q", got.CredentialID, cred)
	}
	if !bytes.Equal(got.PublicKey, pub) {
		t.Errorf("public_key mismatch: got %q want %q", got.PublicKey, pub)
	}
	if got.Label != "little-blue" {
		t.Errorf("label mismatch: %q", got.Label)
	}
	if got.SignCount != 0 {
		t.Errorf("fresh registration must have sign_count=0, got %d", got.SignCount)
	}
	if got.LastUsedAt != "" {
		t.Errorf("fresh registration must have empty last_used_at, got %q", got.LastUsedAt)
	}
}

func TestRegisterRejectsEmptyArgs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("x")

	cases := []struct {
		name       string
		cred, pub  []byte
		label      string
		wantSubstr string
	}{
		{"empty cred", nil, pub, "x", "credential_id"},
		{"empty pub", cred, nil, "x", "public_key"},
		{"empty label", cred, pub, "", "label"},
		{"whitespace label", cred, pub, "   ", "label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Register(ctx, tc.cred, tc.pub, tc.label, "")
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestRegisterRejectsDuplicateCredentialID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("dup")

	if _, err := s.Register(ctx, cred, pub, "device-a", ""); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := s.Register(ctx, cred, pub, "device-b", "")
	if !errors.Is(err, ErrCredentialIDInUse) {
		t.Fatalf("expected ErrCredentialIDInUse, got %v", err)
	}
}

func TestGetByCredentialIDNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.GetByCredentialID(ctx, []byte("never-registered"))
	if !errors.Is(err, ErrPasskeyNotFound) {
		t.Fatalf("expected ErrPasskeyNotFound, got %v", err)
	}
}

func TestGetByCredentialIDRejectsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.GetByCredentialID(ctx, nil)
	if err == nil {
		t.Fatal("expected error for empty credential_id")
	}
}

func TestSaveSignCountStrictGreater(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("counter")
	id, err := s.Register(ctx, cred, pub, "device", "")
	if err != nil {
		t.Fatal(err)
	}

	// 0 → 1 succeeds.
	if err := s.SaveSignCount(ctx, id, 1); err != nil {
		t.Fatalf("0→1: %v", err)
	}
	got, _ := s.GetByCredentialID(ctx, cred)
	if got.SignCount != 1 {
		t.Errorf("sign_count: got %d want 1", got.SignCount)
	}
	if got.LastUsedAt == "" {
		t.Error("last_used_at must be set after SaveSignCount")
	}

	// 1 → 5 succeeds.
	if err := s.SaveSignCount(ctx, id, 5); err != nil {
		t.Fatalf("1→5: %v", err)
	}

	// 5 → 5 must fail (equal is replay).
	if err := s.SaveSignCount(ctx, id, 5); !errors.Is(err, ErrSignCountReplay) {
		t.Errorf("5→5: expected ErrSignCountReplay, got %v", err)
	}

	// 5 → 3 must fail (lower is replay).
	if err := s.SaveSignCount(ctx, id, 3); !errors.Is(err, ErrSignCountReplay) {
		t.Errorf("5→3: expected ErrSignCountReplay, got %v", err)
	}

	// Replays must not have mutated state.
	got, _ = s.GetByCredentialID(ctx, cred)
	if got.SignCount != 5 {
		t.Errorf("after rejected replays sign_count must stay 5, got %d", got.SignCount)
	}
}

// TestSaveSignCountZeroAuthenticator pins the WebAuthn §6.1.1
// zero-counter rule: platform authenticators (Touch ID, Face ID,
// Windows Hello) commonly always emit sign_count=0. The store must
// accept 0→0 as "no counter support" rather than rejecting it as a
// replay. Without this, operators on platform authenticators can
// register but never log in.
func TestSaveSignCountZeroAuthenticator(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("zero")
	id, err := s.Register(ctx, cred, pub, "platform-auth", "")
	if err != nil {
		t.Fatal(err)
	}

	// 0 → 0 must succeed and stamp last_used_at.
	if err := s.SaveSignCount(ctx, id, 0); err != nil {
		t.Fatalf("0→0 must be accepted (no-counter authenticator): %v", err)
	}
	got, _ := s.GetByCredentialID(ctx, cred)
	if got.SignCount != 0 {
		t.Errorf("0→0 must keep sign_count at 0, got %d", got.SignCount)
	}
	if got.LastUsedAt == "" {
		t.Error("0→0 must still stamp last_used_at")
	}

	// 0 → 0 again is still fine — repeated logins from the same
	// no-counter authenticator should keep working.
	if err := s.SaveSignCount(ctx, id, 0); err != nil {
		t.Errorf("repeated 0→0 must keep working: %v", err)
	}
}

// TestSaveSignCountDowngradeIsReplay pins the security shape: an
// authenticator that previously implemented the counter and now
// emits 0 is a clone/downgrade signal, not a no-counter device.
func TestSaveSignCountDowngradeIsReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("downgrade")
	id, err := s.Register(ctx, cred, pub, "device", "")
	if err != nil {
		t.Fatal(err)
	}

	// Establish a non-zero counter.
	if err := s.SaveSignCount(ctx, id, 7); err != nil {
		t.Fatalf("0→7: %v", err)
	}

	// 7 → 0 must reject.
	if err := s.SaveSignCount(ctx, id, 0); !errors.Is(err, ErrSignCountReplay) {
		t.Errorf("7→0 must be ErrSignCountReplay (downgrade attack), got %v", err)
	}
	got, _ := s.GetByCredentialID(ctx, cred)
	if got.SignCount != 7 {
		t.Errorf("rejected downgrade must not mutate state: got %d, want 7", got.SignCount)
	}
}

// TestSaveSignCountRejectsNegative pins guard against caller bugs.
func TestSaveSignCountRejectsNegative(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("neg")
	id, _ := s.Register(ctx, cred, pub, "device", "")
	err := s.SaveSignCount(ctx, id, -1)
	if err == nil {
		t.Fatal("expected error for negative next sign_count")
	}
	if errors.Is(err, ErrSignCountReplay) {
		t.Error("negative next must surface as a hard error, not a replay")
	}
}

func TestListNewestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cred1, pub1 := fakeCred("first")
	cred2, pub2 := fakeCred("second")
	cred3, pub3 := fakeCred("third")
	_, _ = s.Register(ctx, cred1, pub1, "first", "")
	_, _ = s.Register(ctx, cred2, pub2, "second", "")
	_, _ = s.Register(ctx, cred3, pub3, "third", "")

	all, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(all))
	}
	if all[0].Label != "third" || all[1].Label != "second" || all[2].Label != "first" {
		t.Errorf("expected newest-first ordering, got %q %q %q", all[0].Label, all[1].Label, all[2].Label)
	}
}

func TestListEmpty(t *testing.T) {
	s := openTestStore(t)
	all, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("expected empty list, got %d", len(all))
	}
}

func TestDeleteByID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cred, pub := fakeCred("del")
	id, _ := s.Register(ctx, cred, pub, "device", "")

	n, err := s.Delete(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete: got %d rows, want 1", n)
	}
	if _, err := s.GetByCredentialID(ctx, cred); !errors.Is(err, ErrPasskeyNotFound) {
		t.Error("row should be gone after Delete")
	}

	// Delete of non-existent id: 0 rows, no error.
	n, err = s.Delete(ctx, 99999)
	if err != nil {
		t.Errorf("Delete non-existent: %v", err)
	}
	if n != 0 {
		t.Errorf("Delete non-existent: got %d, want 0", n)
	}
}

func TestDeleteAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cred1, pub1 := fakeCred("a")
	cred2, pub2 := fakeCred("b")
	_, _ = s.Register(ctx, cred1, pub1, "a", "")
	_, _ = s.Register(ctx, cred2, pub2, "b", "")

	n, err := s.DeleteAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("DeleteAll: got %d, want 2", n)
	}
	all, _ := s.List(ctx)
	if len(all) != 0 {
		t.Errorf("after DeleteAll list must be empty, got %d", len(all))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
