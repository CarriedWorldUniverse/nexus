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

	waitFor(t, 3*time.Second, func() bool {
		return a.turns() >= 1 && bb.delivered() >= 1 && bb.turns() >= 1
	}, "A -> broker -> B hop")

	t.Logf("hop ok: aspecta turns=%d, aspectb delivered=%d turns=%d",
		a.turns(), bb.delivered(), bb.turns())
}

// TestHarness_EchoLoopReproduced reproduces the shadow<->plumb echo: two
// aspects that each reply addressing the other, with no judge (AlwaysPost)
// and no pre-turn inbox gate. A single seed triggers a runaway — both
// aspects keep taking turns off each other's posts. This documents the bug
// the Day-2 inbox gate must damp; once the gate lands, a sibling test will
// assert the loop is bounded WHILE operator/addressed messages still always
// get a turn. GREEN today because the loop genuinely exists.
func TestHarness_EchoLoopReproduced(t *testing.T) {
	srv, b := newInteractionBroker(t)
	wsURL := wsConnectURL(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// maxCalls caps the storm so an ungated loop can't run forever.
	a := newHarnessAspect(t, ctx, b, wsURL, "echoa", "@echob ping", 50)
	bb := newHarnessAspect(t, ctx, b, wsURL, "echob", "@echoa pong", 50)
	waitConnected(t, b, "echoa", "echob")

	seed(t, b, "@echoa start")

	// One seed -> many turns each = the echo. Without a gate it keeps going.
	waitFor(t, 5*time.Second, func() bool {
		return a.turns() >= 4 && bb.turns() >= 4
	}, "echo loop runaway from a single seed")

	t.Logf("echo reproduced from a single seed: echoa turns=%d, echob turns=%d",
		a.turns(), bb.turns())
}
