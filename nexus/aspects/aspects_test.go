package aspects

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// freshStore opens a fresh DB with the bootstrapped schema and
// returns a SQLStore wrapping it.
func freshStore(t *testing.T) *SQLStore {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSQLStore(db)
}

// fakePubkey returns a 32-byte pubkey for tests. Doesn't need to be
// real Ed25519 — the schema stores BLOB and the package doesn't
// verify cryptographic shape, that's the validation endpoint's job.
func fakePubkey(seed byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = seed
	}
	return out
}

func TestInsert_AndGet(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	a := Aspect{
		Name:         "plumb",
		AspectPubkey: fakePubkey(1),
		Provider:     "claude-api",
		Model:        "claude-opus-4-7",
		Capabilities: `["code","review"]`,
	}
	if err := s.Insert(ctx, a); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := s.Get(ctx, "plumb")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "plumb" {
		t.Errorf("Name = %q; want plumb", got.Name)
	}
	if got.Status != StatusActive {
		t.Errorf("default Status = %q; want active", got.Status)
	}
	if got.CurrentKeyfileVersion != 1 {
		t.Errorf("default version = %d; want 1", got.CurrentKeyfileVersion)
	}
	if string(got.AspectPubkey) != string(fakePubkey(1)) {
		t.Errorf("AspectPubkey mismatch")
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus-4-7" {
		t.Errorf("provider/model not stored: got %q / %q", got.Provider, got.Model)
	}
	if got.Capabilities != `["code","review"]` {
		t.Errorf("Capabilities = %q; want JSON array", got.Capabilities)
	}
	if got.CreatedAt == "" {
		t.Error("CreatedAt empty — default datetime('now') not applied")
	}
}

func TestGet_NotFound(t *testing.T) {
	s := freshStore(t)
	_, err := s.Get(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(ghost) = %v; want ErrNotFound", err)
	}
}

func TestList_OrderByName(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	for _, name := range []string{"forge", "anvil", "wren"} {
		if err := s.Insert(ctx, Aspect{Name: name, AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
			t.Fatalf("Insert(%q): %v", name, err)
		}
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d aspects; want 3", len(got))
	}
	want := []string{"anvil", "forge", "wren"}
	for i, a := range got {
		if a.Name != want[i] {
			t.Errorf("List[%d] = %q; want %q", i, a.Name, want[i])
		}
	}
}

func TestInsert_DuplicateName(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	a := Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}
	if err := s.Insert(ctx, a); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := s.Insert(ctx, a); err == nil {
		t.Error("second Insert with same name should error; got nil")
	}
}

func TestBumpKeyfileVersion(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	if err := s.Insert(ctx, Aspect{
		Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	v, err := s.BumpKeyfileVersion(ctx, "plumb", fakePubkey(2))
	if err != nil {
		t.Fatalf("Bump: %v", err)
	}
	if v != 2 {
		t.Errorf("new version = %d; want 2", v)
	}

	got, _ := s.Get(ctx, "plumb")
	if got.CurrentKeyfileVersion != 2 {
		t.Errorf("Get after Bump: version = %d; want 2", got.CurrentKeyfileVersion)
	}
	if string(got.AspectPubkey) != string(fakePubkey(2)) {
		t.Error("Bump did not replace pubkey")
	}

	// Bump again — version 3
	v, err = s.BumpKeyfileVersion(ctx, "plumb", fakePubkey(3))
	if err != nil {
		t.Fatalf("Bump 2: %v", err)
	}
	if v != 3 {
		t.Errorf("second bump version = %d; want 3", v)
	}
}

func TestBumpKeyfileVersion_NotFound(t *testing.T) {
	s := freshStore(t)
	_, err := s.BumpKeyfileVersion(context.Background(), "ghost", fakePubkey(1))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Bump(ghost) = %v; want ErrNotFound", err)
	}
}

func TestSetStatus(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := s.SetStatus(ctx, "plumb", StatusRetired); err != nil {
		t.Fatalf("SetStatus retired: %v", err)
	}
	got, _ := s.Get(ctx, "plumb")
	if got.Status != StatusRetired {
		t.Errorf("Status = %q after retire; want retired", got.Status)
	}

	if err := s.SetStatus(ctx, "plumb", StatusActive); err != nil {
		t.Fatalf("SetStatus active: %v", err)
	}
	got, _ = s.Get(ctx, "plumb")
	if got.Status != StatusActive {
		t.Errorf("Status = %q after resurrect; want active", got.Status)
	}
}

func TestSetStatus_NotFound(t *testing.T) {
	s := freshStore(t)
	err := s.SetStatus(context.Background(), "ghost", StatusRetired)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("SetStatus(ghost) = %v; want ErrNotFound", err)
	}
}

func TestSetStatus_ChecksConstraintEnforced(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// "frozen" is not in the CHECK list. SQLite should reject.
	err := s.SetStatus(ctx, "plumb", "frozen")
	if err == nil {
		t.Error("SetStatus with invalid status should fail; got nil (CHECK constraint not enforced?)")
	}
}

// TestResurrect_AtomicTransition verifies the spec §9.3 invariant that
// status flip + version bump + pubkey replacement are a single
// atomic operation. A two-step implementation would have a window
// where the old keyfile re-validates after status='active' but before
// the version bump.
func TestResurrect_AtomicTransition(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{
		Name: "plumb", AspectPubkey: fakePubkey(1),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.SetStatus(ctx, "plumb", StatusRetired); err != nil {
		t.Fatalf("SetStatus retired: %v", err)
	}

	newVer, err := s.Resurrect(ctx, "plumb", fakePubkey(2))
	if err != nil {
		t.Fatalf("Resurrect: %v", err)
	}
	if newVer != 2 {
		t.Errorf("new version = %d; want 2", newVer)
	}

	got, _ := s.Get(ctx, "plumb")
	if got.Status != StatusActive {
		t.Errorf("status = %q; want active", got.Status)
	}
	if got.CurrentKeyfileVersion != 2 {
		t.Errorf("version = %d; want 2", got.CurrentKeyfileVersion)
	}
	if string(got.AspectPubkey) != string(fakePubkey(2)) {
		t.Error("pubkey not replaced with placeholder")
	}
}

// TestResurrect_RefusesNonRetired — the WHERE clause guards against
// resurrecting an active row. Returns ErrNotFound (zero rows
// affected); caller's "is retired?" pre-check usually catches this,
// but the store-level guard is defense-in-depth.
func TestResurrect_RefusesNonRetired(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{
		Name: "plumb", AspectPubkey: fakePubkey(1),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Aspect is active, not retired. Resurrect must refuse.
	_, err := s.Resurrect(ctx, "plumb", fakePubkey(2))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Resurrect(active) = %v; want ErrNotFound", err)
	}
	// Confirm no state change.
	got, _ := s.Get(ctx, "plumb")
	if got.CurrentKeyfileVersion != 1 {
		t.Errorf("version changed despite refused resurrect: %d", got.CurrentKeyfileVersion)
	}
}

// TestResurrect_NotFound — missing aspect.
func TestResurrect_NotFound(t *testing.T) {
	s := freshStore(t)
	_, err := s.Resurrect(context.Background(), "ghost", fakePubkey(1))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Resurrect(ghost) = %v; want ErrNotFound", err)
	}
}

func TestPersonalityGet_NotFound(t *testing.T) {
	s := freshStore(t)
	_, err := s.PersonalityGet(context.Background(), "plumb")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("PersonalityGet(plumb) = %v; want ErrNotFound", err)
	}
}

func TestPersonalitySet_InsertAndGet(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	// Personality requires a parent aspect row (FK).
	if err := s.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("Insert aspect: %v", err)
	}

	p := Personality{
		AspectName: "plumb",
		NexusMD:    "# plumb operational",
		SoulMD:     "# plumb soul",
		PrimerMD:   "# plumb primer",
	}
	if err := s.PersonalitySet(ctx, p); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	got, err := s.PersonalityGet(ctx, "plumb")
	if err != nil {
		t.Fatalf("PersonalityGet: %v", err)
	}
	if got.NexusMD != p.NexusMD || got.SoulMD != p.SoulMD || got.PrimerMD != p.PrimerMD {
		t.Errorf("personality columns mismatch: got %+v want %+v", got, p)
	}
	if got.Composed != "" {
		t.Errorf("Composed = %q; want empty (cache invalidated on write)", got.Composed)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d on first set; want 1", got.Version)
	}
}

func TestPersonalitySet_BumpsVersionOnUpdate(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("Insert aspect: %v", err)
	}

	if err := s.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "v1"}); err != nil {
		t.Fatalf("PersonalitySet 1: %v", err)
	}
	got, _ := s.PersonalityGet(ctx, "plumb")
	v1 := got.Version

	if err := s.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "v2"}); err != nil {
		t.Fatalf("PersonalitySet 2: %v", err)
	}
	got, _ = s.PersonalityGet(ctx, "plumb")
	if got.Version <= v1 {
		t.Errorf("Version did not bump on update: was %d, now %d", v1, got.Version)
	}
	if got.NexusMD != "v2" {
		t.Errorf("NexusMD did not update: got %q want v2", got.NexusMD)
	}
}

func TestPersonalitySet_FKCascadeOnAspectDelete(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("Insert aspect: %v", err)
	}
	if err := s.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "x"}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	// Delete the parent aspect row directly. Cascade should remove
	// the personality row.
	_, err := s.db.ExecContext(ctx, `DELETE FROM aspects WHERE name = ?`, "plumb")
	if err != nil {
		t.Fatalf("delete aspect: %v", err)
	}

	_, err = s.PersonalityGet(ctx, "plumb")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("PersonalityGet after aspect delete = %v; want ErrNotFound (FK cascade not working)", err)
	}
}
