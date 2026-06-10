package roster

import (
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func registerReq(name, session string) *schemas.RegisterRequest {
	return &schemas.RegisterRequest{
		Name:        name,
		SessionID:   session,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-code",
		StartedAt:   time.Now().UTC(),
	}
}

func TestSetNappingFlipsKnownAspect(t *testing.T) {
	r := New()
	if _, _, err := r.Register(registerReq("plumb", "s1")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if !r.SetNapping("plumb") {
		t.Fatal("SetNapping(plumb) = false, want true for a registered aspect")
	}
	a, ok := r.Get("plumb")
	if !ok {
		t.Fatal("Get(plumb): aspect missing after SetNapping")
	}
	if a.Status != StatusNapping {
		t.Fatalf("status = %q, want %q", a.Status, StatusNapping)
	}

	// Idempotent: flipping an already-napping aspect succeeds and stays napping.
	if !r.SetNapping("plumb") {
		t.Fatal("SetNapping(plumb) second call = false, want true (idempotent)")
	}
	a, _ = r.Get("plumb")
	if a.Status != StatusNapping {
		t.Fatalf("status after second SetNapping = %q, want %q", a.Status, StatusNapping)
	}
}

func TestSetNappingUnknownAspectIsNoOp(t *testing.T) {
	r := New()
	if r.SetNapping("ghost") {
		t.Fatal("SetNapping(ghost) = true, want false for an unknown aspect")
	}
	if _, ok := r.Get("ghost"); ok {
		t.Fatal("SetNapping must not create roster entries")
	}
}

// A napping aspect that registers (the wake path: pod scaled 0→1, aspect
// boots, registers) goes napping→live. The Register path already handles
// any→live; this locks it in so the wake controller can rely on it.
func TestRegisterTransitionsNappingToLive(t *testing.T) {
	r := New()
	if _, _, err := r.Register(registerReq("plumb", "s1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !r.SetNapping("plumb") {
		t.Fatal("SetNapping failed")
	}

	// Re-register with a NEW session id — the woken pod is a fresh boot.
	// Napping is not "live", so the different-session guard must not fire.
	if _, _, err := r.Register(registerReq("plumb", "s2")); err != nil {
		t.Fatalf("Register after napping: %v", err)
	}
	a, _ := r.Get("plumb")
	if a.Status != "live" {
		t.Fatalf("status after wake register = %q, want live", a.Status)
	}
	if a.SessionID != "s2" {
		t.Fatalf("session = %q, want s2", a.SessionID)
	}
}

// The stale sweep must never demote a napping aspect: napping is a
// deliberate state (idle reaper scaled it to zero), not staleness. A
// napping aspect by definition has an ancient heartbeat.
func TestReapStaleLeavesNappingAlone(t *testing.T) {
	r := New()
	if _, _, err := r.Register(registerReq("plumb", "s1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := r.Register(registerReq("anvil", "s2")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.SetNapping("plumb")

	staleAfter := 30 * time.Second
	future := time.Now().Add(10 * staleAfter)

	stale, down := r.ReapStale(future, staleAfter)
	for _, name := range stale {
		if name == "plumb" {
			t.Fatal("napping aspect reported stale")
		}
	}
	for _, name := range down {
		if name == "plumb" {
			t.Fatal("napping aspect reported down")
		}
	}
	a, _ := r.Get("plumb")
	if a.Status != StatusNapping {
		t.Fatalf("napping aspect status after sweep = %q, want %q", a.Status, StatusNapping)
	}
	// The live-but-silent aspect IS swept — the guard is napping-specific.
	b, _ := r.Get("anvil")
	if b.Status != "down" {
		t.Fatalf("silent live aspect status after sweep = %q, want down", b.Status)
	}
}

// Lock the existing live→stale→down ladder so the napping guard doesn't
// regress normal staleness handling.
func TestReapStaleLadderUnchanged(t *testing.T) {
	r := New()
	if _, _, err := r.Register(registerReq("keel", "s1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	staleAfter := 30 * time.Second

	stale, down := r.ReapStale(time.Now().Add(staleAfter+time.Second), staleAfter)
	if len(stale) != 1 || stale[0] != "keel" || len(down) != 0 {
		t.Fatalf("first sweep: stale=%v down=%v, want stale=[keel]", stale, down)
	}
	stale, down = r.ReapStale(time.Now().Add(2*staleAfter+time.Second), staleAfter)
	if len(down) != 1 || down[0] != "keel" || len(stale) != 0 {
		t.Fatalf("second sweep: stale=%v down=%v, want down=[keel]", stale, down)
	}
}
