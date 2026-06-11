package broker

import (
	"context"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/convene"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// nullConn satisfies connLike for the shared close path; it discards sends.
type nullConn struct{}

func (nullConn) send(frames.Envelope) {}

func newCloseBroker(t *testing.T, cv *fakeConveneStore) *Broker {
	t.Helper()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ConveneStore:       cv,
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	return b
}

func seedConvene(cv *fakeConveneStore, facilitator string) {
	_ = cv.Insert(context.Background(), convene.Convene{
		ConveneID:   "cv-x",
		Facilitator: facilitator,
		Status:      convene.StatusOpen,
		CreatedAt:   time.Now(),
	})
}

func TestConveneCloseByFacilitator(t *testing.T) {
	cv := &fakeConveneStore{}
	seedConvene(cv, "shadow")
	b := newCloseBroker(t, cv)

	res := b.closeConvene(nullConn{}, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "converged", SummaryMsgID: 7,
	}, "shadow", false)

	if !res.OK {
		t.Fatalf("close not OK: %q", res.Message)
	}
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if len(cv.closed) != 1 || cv.closed[0].status != convene.StatusConverged || cv.closed[0].summary != 7 {
		t.Errorf("close calls = %+v, want one converged/summary=7", cv.closed)
	}
}

func TestConveneCloseByNonFacilitatorRejected(t *testing.T) {
	cv := &fakeConveneStore{}
	seedConvene(cv, "shadow")
	b := newCloseBroker(t, cv)

	// plumb is a participant, not the facilitator.
	res := b.closeConvene(nullConn{}, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "converged",
	}, "plumb", false)

	if res.OK {
		t.Fatal("expected rejection: non-facilitator cannot close")
	}
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if len(cv.closed) != 0 {
		t.Errorf("store.Close called %d times, want 0 (authz rejected)", len(cv.closed))
	}
}

func TestConveneCloseByOperatorAllowed(t *testing.T) {
	cv := &fakeConveneStore{}
	seedConvene(cv, "shadow")
	b := newCloseBroker(t, cv)

	res := b.closeConvene(nullConn{}, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "abandoned",
	}, "operator", true) // isOperator relaxes facilitator authz

	if !res.OK {
		t.Fatalf("operator close not OK: %q", res.Message)
	}
}

func TestConveneCloseRejectsBadStatus(t *testing.T) {
	cv := &fakeConveneStore{}
	seedConvene(cv, "shadow")
	b := newCloseBroker(t, cv)

	res := b.closeConvene(nullConn{}, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "open",
	}, "shadow", false)

	if res.OK {
		t.Fatal("expected rejection: status 'open' is not a valid close target")
	}
}

func TestConveneCloseUnknownConvene(t *testing.T) {
	cv := &fakeConveneStore{}
	b := newCloseBroker(t, cv)
	res := b.closeConvene(nullConn{}, frames.ConveneClosePayload{
		ConveneID: "nope", Status: "converged",
	}, "shadow", false)
	if res.OK {
		t.Fatal("expected rejection: unknown convene")
	}
}
