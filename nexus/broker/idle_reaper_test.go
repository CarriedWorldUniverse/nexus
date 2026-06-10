package broker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// reaperHarness is a broker + idle reaper with a fake scaler and a fake
// clock (shared by the reaper and the observability groupers). plumb is
// wake-on-mention, keel is always-on; both registered live in the
// roster.
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
	return newReaperHarnessWithPolicies(t, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"keel":  WakePolicyAlwaysOn,
	})
}

func newReaperHarnessWithPolicies(t *testing.T, policies map[string]string) *reaperHarness {
	t.Helper()
	h := &reaperHarness{
		scaler: &fakeScaler{},
		runs:   &memRuns{},
		r:      roster.New(),
		now:    time.Now(),
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
		Observability:      observability.NewHubWithClock(500, nil, func() time.Time { return h.now }),
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

// An open turn whose observe.end never arrived (crashed pod, lost conn)
// must not hold the mid-turn guard forever: past max(2h, 4×IdleTimeout)
// the reaper treats it as not in flight, reaps, and warns naming the
// aspect and turn age.
func TestIdleReaperReapsPastStaleTurnCap(t *testing.T) {
	h := newReaperHarness(t)
	var logBuf bytes.Buffer
	h.b.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	h.b.touchChatActivity("plumb", h.now)
	h.b.observability.GrouperFor("plumb").BeginTurn("t1", "main", "m", "p", 0)

	h.now = h.now.Add(3 * time.Hour) // past the 2h cap (4×15m = 1h < 2h floor)
	h.sweep()

	calls := h.scaler.scaleCalls()
	if len(calls) != 1 || calls[0] != (scaleCall{name: "plumb", replicas: 0}) {
		t.Fatalf("scale calls = %v, want exactly plumb→0 past the stale-turn cap", calls)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "plumb") || !strings.Contains(logs, "stale") {
		t.Fatalf("want a warn naming the aspect and the stale turn, got: %q", logs)
	}
	if !strings.Contains(logs, "turn_age") {
		t.Fatalf("want the warn to carry the turn age, got: %q", logs)
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

// A wake-on-mention aspect unknown to the roster (never registered this
// broker lifetime) can't be flipped to napping, so the already-napping
// guard never catches it. The reaper must re-stamp its activity after
// the scale-0 so it issues exactly one scale-0 per idle window, not one
// per sweep.
func TestIdleReaperParksUnregisteredAspectOncePerWindow(t *testing.T) {
	h := newReaperHarnessWithPolicies(t, map[string]string{
		"ghost": WakePolicyWakeOnMention, // not registered in the roster
	})
	h.b.touchChatActivity("ghost", h.now)

	h.now = h.now.Add(16 * time.Minute)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want one scale-0 after the idle window", calls)
	}

	// One ticker interval later — inside the fresh idle window started
	// by the post-reap re-stamp: no re-issued scale-0.
	h.now = h.now.Add(5 * time.Minute)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want no re-reap inside the window", calls)
	}

	// A full idle window later it's still unregistered and quiet — one
	// more scale-0 is correct (idempotent on k8s, and bounded per window).
	h.now = h.now.Add(16 * time.Minute)
	h.sweep()
	if calls := h.scaler.scaleCalls(); len(calls) != 2 {
		t.Fatalf("scale calls = %v, want one scale-0 per idle window", calls)
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
