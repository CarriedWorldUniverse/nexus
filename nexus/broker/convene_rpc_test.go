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

// NEX-609 follow-through: an authenticated-but-UNREGISTERED connection
// (the comms sidecar, -register=false) closes a convene as its
// JWT-verified identity — same fallback rule as spawn. A registered
// agentfunnel owns the one-session-per-name slot, so the facilitator's
// nexus-comms-mcp could otherwise never close.
func TestConveneCloseFromUnregisteredAuthenticatedFacilitator(t *testing.T) {
	cv := &fakeConveneStore{}
	seedConvene(cv, "shadow")
	srv := newSpawnTestServerCfg(t, nil, func(cfg *Config) {
		cfg.ConveneStore = cv
	})
	c := dialWS(t, srv, signSpawnAspectJWT(t, "shadow"))
	// No register frame — the JWT identity vouches for the caller.
	env, err := frames.NewRequest(frames.KindConveneClose, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "converged", SummaryMsgID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	resp := recvFrame(t, c)
	if resp.Kind != frames.KindConveneCloseResult {
		t.Fatalf("kind = %q", resp.Kind)
	}
	var out frames.ConveneCloseResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("close result = %+v, want ok", out)
	}
	cv.mu.Lock()
	closedCalls := len(cv.closed)
	closedStatus := convene.Status("")
	if closedCalls > 0 {
		closedStatus = cv.closed[0].status
	}
	cv.mu.Unlock()
	if closedCalls != 1 || closedStatus != convene.StatusConverged {
		t.Fatalf("store close calls = %d status = %q, want one converged close", closedCalls, closedStatus)
	}

	// And the legacy master (admin, unregistered) still cannot close.
	c2 := dialWS(t, srv, "testtoken")
	seedConvene(cv, "shadow") // no-op; cv-x already closed — reuse rejection shape
	env2, _ := frames.NewRequest(frames.KindConveneClose, frames.ConveneClosePayload{
		ConveneID: "cv-x", Status: "abandoned",
	})
	sendFrame(t, c2, env2)
	resp2 := recvFrame(t, c2)
	var out2 frames.ConveneCloseResultPayload
	_ = frames.PayloadAs(resp2, &out2)
	if out2.OK {
		t.Fatal("an admin/legacy unregistered connection must not pass the facilitator check")
	}
}
