package funnel

import (
	"testing"
)

// #226.4 — SessionResolver behaviour across the three ContextModes.

func TestSessionResolver_GlobalReturnsSameHandleAcrossThreads(t *testing.T) {
	r := NewSessionResolver("plumb", ContextGlobal)
	a := r.Resolve(101)
	b := r.Resolve(202)
	if a.ID != b.ID {
		t.Errorf("global mode should ignore thread root: got %q vs %q", a.ID, b.ID)
	}
	if a.ID != r.GlobalHandle().ID {
		t.Errorf("resolved id should match GlobalHandle: %q vs %q", a.ID, r.GlobalHandle().ID)
	}
}

func TestSessionResolver_GlobalMarksResumedFlipsNew(t *testing.T) {
	r := NewSessionResolver("plumb", ContextGlobal)
	first := r.Resolve(0)
	if !first.New {
		t.Fatal("first resolve should be New=true")
	}
	r.MarkResumed(first.ID)
	second := r.Resolve(0)
	if second.New {
		t.Errorf("after MarkResumed, second resolve should be New=false")
	}
	if second.ID != first.ID {
		t.Errorf("global handle id should persist: %q vs %q", first.ID, second.ID)
	}
}

func TestSessionResolver_GlobalRotateMintsFresh(t *testing.T) {
	r := NewSessionResolver("plumb", ContextGlobal)
	first := r.Resolve(0)
	r.MarkResumed(first.ID)
	rotated := r.RotateGlobal()
	if rotated.ID == first.ID {
		t.Errorf("RotateGlobal should mint a fresh id")
	}
	if !rotated.New {
		t.Errorf("rotated handle should be New=true")
	}
	next := r.Resolve(0)
	if next.ID != rotated.ID {
		t.Errorf("post-rotate resolve should return the new handle")
	}
}

func TestSessionResolver_ThreadIsolatedDifferentRootsDifferentIDs(t *testing.T) {
	r := NewSessionResolver("plumb", ContextThreadIsolated)
	a1 := r.Resolve(101)
	b1 := r.Resolve(202)
	if a1.ID == b1.ID {
		t.Errorf("different thread roots must produce different session ids")
	}
	if !a1.New || !b1.New {
		t.Errorf("first resolve per thread should be New=true (got %v, %v)", a1.New, b1.New)
	}

	// Second resolve on the same thread returns the same id with New=false.
	a2 := r.Resolve(101)
	if a2.ID != a1.ID {
		t.Errorf("second resolve on same thread should return same id: %q vs %q", a1.ID, a2.ID)
	}
	if a2.New {
		t.Errorf("second resolve on same thread should be New=false")
	}
}

func TestSessionResolver_ThreadIsolatedDeterministicAcrossInstances(t *testing.T) {
	// Two resolvers for the same aspect with the same thread root
	// should derive the same session id — that's the whole point of
	// uuid_v5. Cold-boot replay across process restarts depends on it.
	r1 := NewSessionResolver("plumb", ContextThreadIsolated)
	r2 := NewSessionResolver("plumb", ContextThreadIsolated)
	id1 := r1.Resolve(42).ID
	id2 := r2.Resolve(42).ID
	if id1 != id2 {
		t.Errorf("uuid_v5 derivation should be stable across resolver instances: %q vs %q", id1, id2)
	}
}

func TestSessionResolver_ThreadIsolatedSeparatesByAspect(t *testing.T) {
	// Same thread root, different aspects → different sessions on
	// disk. Otherwise two aspects in a shared thread would clobber
	// each other's jsonl.
	r1 := NewSessionResolver("plumb", ContextThreadIsolated)
	r2 := NewSessionResolver("forge", ContextThreadIsolated)
	if r1.Resolve(42).ID == r2.Resolve(42).ID {
		t.Errorf("different aspects must produce different session ids for the same thread")
	}
}

func TestSessionResolver_ThreadIsolatedFallsBackToGlobalForNoThread(t *testing.T) {
	r := NewSessionResolver("plumb", ContextThreadIsolated)
	a := r.Resolve(0)
	if a.ID != r.GlobalHandle().ID {
		t.Errorf("threadRoot==0 should return the global handle: %q vs %q", a.ID, r.GlobalHandle().ID)
	}
}

func TestSessionResolver_StatelessMintsFreshEveryTime(t *testing.T) {
	r := NewSessionResolver("plumb", ContextStateless)
	a := r.Resolve(101)
	b := r.Resolve(101) // same thread root — should still be a new id
	if a.ID == b.ID {
		t.Errorf("stateless mode should mint a fresh id every call")
	}
	if !a.New || !b.New {
		t.Errorf("stateless ids are always New=true")
	}
}

func TestSessionResolver_DefaultModeIsGlobal(t *testing.T) {
	r := NewSessionResolver("plumb", "")
	if r.Mode() != ContextGlobal {
		t.Errorf("empty mode should default to ContextGlobal, got %q", r.Mode())
	}
}
