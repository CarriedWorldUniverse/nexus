package broker

import (
	"context"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// reaperHarness is a broker + idle reaper with a fake scaler and a fake
// clock. plumb is wake-on-mention, keel is always-on; both registered
// live in the roster.
type reaperHarness struct {
	b      *Broker
	r      *roster.Roster
	scaler *fakeScaler
	runs   *memRuns
	reaper *idleReaper
	now    time.Time
}

func newReaperHarness(t *testing.T) *reaperHarness {
	t.Helper()
	h := &reaperHarness{
		scaler: &fakeScaler{},
		runs:   &memRuns{},
		r:      roster.New(),
		now:    time.Now(),
	}
	policies := map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"keel":  WakePolicyAlwaysOn,
	}
	h.b = New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          &fakeChatStore{},
		RecipientPolicy:    &RecipientPolicy{},
		RunsStore:          h.runs,
		AspectWakePolicy:   policies,
	}, h.r)
	h.b.ctx, h.b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(h.b.ctxCancel)

	for _, name := range []string{"plumb", "keel"} {
		if _, _, err := h.r.Register(&schemas.RegisterRequest{
			Name:        name,
			SessionID:   name + "-sess",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-code",
			StartedAt:   h.now,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	h.reaper = newIdleReaper(h.b, h.scaler, 15*time.Minute)
	h.reaper.now = func() time.Time { return h.now }
	return h
}

func (h *reaperHarness) sweep() { h.reaper.sweep(context.Background()) }

func TestIdleReaperScalesQuietAspectToZero(t *testing.T) {
	h := newReaperHarness(t)
	h.b.touchChatActivity("plumb", h.now)

	h.now = h.now.Add(16 * time.Minute)
	h.sweep()

	calls := h.scaler.scaleCalls()
	if len(calls) != 1 || calls[0] != (scaleCall{name: "plumb", replicas: 0}) {
		t.Fatalf("scale calls = %v, want exactly plumb→0", calls)
	}
	a, _ := h.r.Get("plumb")
	if a.Status != roster.StatusNapping {
		t.Fatalf("plumb status = %q, want %q", a.Status, roster.StatusNapping)
	}

	// Already napping → subsequent sweeps don't re-scale.
	h.now = h.now.Add(16 * time.Minute)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls after second sweep = %v, want still one", calls)
	}
}

func TestIdleReaperSkipsRecentChat(t *testing.T) {
	h := newReaperHarness(t)
	h.b.touchChatActivity("plumb", h.now)

	h.now = h.now.Add(10 * time.Minute) // inside the 15m window
	h.sweep()

	if calls := h.scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none for recently-active aspect", calls)
	}
	a, _ := h.r.Get("plumb")
	if a.Status == roster.StatusNapping {
		t.Fatal("recently-active aspect flipped to napping")
	}
}

func TestIdleReaperSkipsActiveDispatchRun(t *testing.T) {
	h := newReaperHarness(t)
	h.b.touchChatActivity("plumb", h.now)
	_ = h.runs.Insert(context.Background(), runs.Run{
		RunID: "run-1", Ticket: "NEX-1", Agent: "plumb",
		Status: runs.StatusRunning, StartedAt: h.now,
	})

	h.now = h.now.Add(16 * time.Minute)
	h.sweep()

	if calls := h.scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none while a dispatch run is active", calls)
	}

	// Run completes → next sweep reaps.
	_ = h.runs.MarkDone(context.Background(), "run-1", runs.StatusComplete, h.now, "", 1)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want reap once the run is done", calls)
	}
}

func TestIdleReaperSkipsInFlightTurn(t *testing.T) {
	h := newReaperHarness(t)
	h.b.touchChatActivity("plumb", h.now)
	h.b.observability.GrouperFor("plumb").BeginTurn("t1", "main", "m", "p", 0)

	h.now = h.now.Add(16 * time.Minute)
	h.sweep()

	if calls := h.scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none mid-turn", calls)
	}

	// Turn ends → next sweep reaps.
	h.b.observability.GrouperFor("plumb").EndTurn()
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want reap once the turn ended", calls)
	}
}

func TestIdleReaperNeverReapsAlwaysOn(t *testing.T) {
	h := newReaperHarness(t)
	h.b.touchChatActivity("keel", h.now)

	h.now = h.now.Add(24 * time.Hour)
	h.sweep()

	if calls := h.scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none for always-on", calls)
	}
	a, _ := h.r.Get("keel")
	if a.Status == roster.StatusNapping {
		t.Fatal("always-on aspect flipped to napping")
	}
}

// An aspect with no recorded chat activity (e.g. right after a broker
// restart) starts its idle clock at the first sweep rather than being
// reaped immediately with zero evidence.
func TestIdleReaperStampsUnknownActivityBeforeReaping(t *testing.T) {
	h := newReaperHarness(t)

	h.sweep() // first sight: stamp, don't reap
	if calls := h.scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none on first sight", calls)
	}

	h.now = h.now.Add(16 * time.Minute)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want reap one idle-window after first sight", calls)
	}
}

// HandleChatSend is the reaper's last-activity source: it stamps the
// sender and every computed recipient.
func TestHandleChatSendStampsLastChatActivity(t *testing.T) {
	h := newReaperHarness(t)

	before := time.Now()
	if _, err := h.b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	for _, name := range []string{"shadow", "plumb"} {
		at, ok := h.b.lastChatTouch(name)
		if !ok {
			t.Fatalf("no lastChatActivity entry for %s", name)
		}
		if at.Before(before) {
			t.Fatalf("%s stamped %v, want >= %v", name, at, before)
		}
	}
	if _, ok := h.b.lastChatTouch("keel"); ok {
		t.Fatal("keel stamped without being sender or recipient")
	}
}

func TestIdleTimeoutDefaultApplied(t *testing.T) {
	h := newReaperHarness(t)
	ir := newIdleReaper(h.b, h.scaler, 0)
	if ir.timeout != defaultIdleTimeout {
		t.Fatalf("timeout = %v, want default %v", ir.timeout, defaultIdleTimeout)
	}
}
