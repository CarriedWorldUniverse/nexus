package broker

import (
	"context"
	"strings"
	"testing"
	"time"
)

// wsConnectURL derives the ws:// /connect URL from an httptest server.
func wsConnectURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/connect"
}

// TestHarness_DeliversAcrossAspects proves the L4 harness end to end: a
// seeded message addressed to A makes A run a turn and post a reply
// addressed to B; the broker delivers it to B and B runs a turn. That is
// aspect -> broker -> aspect over the real WS path, with no live network,
// no real LLM, and no secrets. GREEN today — it proves the harness works.
func TestHarness_DeliversAcrossAspects(t *testing.T) {
	srv, b := newInteractionBroker(t)
	wsURL := wsConnectURL(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// A relays to B exactly once; B acks once with no routing mention, so
	// the exchange stops after a single hop (no echo in this test).
	a := newHarnessAspect(t, ctx, b, wsURL, "aspecta", "@aspectb relay", 1)
	bb := newHarnessAspect(t, ctx, b, wsURL, "aspectb", "ack", 1)
	waitConnected(t, b, "aspecta", "aspectb")

	seed(t, b, "@aspecta kickoff")

	waitFor(t, 15*time.Second, func() bool {
		return a.turns() >= 1 && bb.delivered() >= 1 && bb.turns() >= 1
	}, "A -> broker -> B hop")

	t.Logf("hop ok: aspecta turns=%d, aspectb delivered=%d turns=%d",
		a.turns(), bb.delivered(), bb.turns())
}

// TestHarness_EchoLoopReproduced reproduces the shadow<->plumb echo: two
// aspects that each reply addressing the other. With the NEX-365 Tier-1
// loop damper in the funnel, a single seed makes each aspect take a few
// turns and then DAMP — the runaway is capped well below the maxCalls
// ceiling. (Before the gate this climbed to ~50; the cap proves the gate,
// not the maxCalls safety net, stopped it.)
func TestHarness_EchoLoopDamped(t *testing.T) {
	srv, b := newInteractionBroker(t)
	wsURL := wsConnectURL(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a := newHarnessAspect(t, ctx, b, wsURL, "echoa", "@echob ping", 50)
	bb := newHarnessAspect(t, ctx, b, wsURL, "echob", "@echoa pong", 50)
	waitConnected(t, b, "echoa", "echob")

	seed(t, b, "@echoa start")

	// The loop starts (a few turns each) ...
	waitFor(t, 20*time.Second, func() bool {
		return a.turns() >= 2 && bb.turns() >= 2
	}, "echo loop to start")

	// ... then the damper caps it. Give it ample time to (not) run away.
	time.Sleep(2 * time.Second)
	const cap = 10 // damp threshold is 3 (+ a couple repeats to detect) — well under 50
	if a.turns() > cap || bb.turns() > cap {
		t.Fatalf("loop not damped: echoa=%d echob=%d (want <=%d each; ungated would approach maxCalls=50)",
			a.turns(), bb.turns(), cap)
	}
	t.Logf("echo damped: echoa turns=%d, echob turns=%d (capped, not runaway)", a.turns(), bb.turns())
}

// TestHarness_OperatorNeverDamped is the safety invariant: even after an
// aspect is damped by a degenerate peer echo, an OPERATOR message still gets
// a turn — the damper must never silence the human channel.
func TestHarness_OperatorNeverDamped(t *testing.T) {
	srv, b := newInteractionBroker(t)
	wsURL := wsConnectURL(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a := newHarnessAspect(t, ctx, b, wsURL, "dampa", "@dampb ping", 200)
	bb := newHarnessAspect(t, ctx, b, wsURL, "dampb", "@dampa pong", 200)
	_ = bb // bb only exists to drive the echo that damps dampa
	waitConnected(t, b, "dampa", "dampb")

	// Drive dampa into a damped state via the peer echo, then let it settle.
	seed(t, b, "@dampa start")
	waitFor(t, 20*time.Second, func() bool { return a.turns() >= 4 }, "dampa to start looping")
	time.Sleep(1500 * time.Millisecond) // let damping engage + the peer storm quiesce
	damped := a.turns()

	// Now the operator addresses dampa directly. Even damped, this MUST run.
	seed(t, b, "@dampa operator here — please ack")
	waitFor(t, 20*time.Second, func() bool { return a.turns() > damped }, "operator message to get a turn despite damping")
	t.Logf("operator broke through: dampa turns %d -> %d", damped, a.turns())
}
