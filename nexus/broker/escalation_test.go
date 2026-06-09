package broker

import (
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// TestEscalation_RoundTrip drives the full broker relay with a real
// aspect WS connection and a real operator WS connection:
//
//	aspect sends escalation.request (correlated by env.ID)
//	  → broker fans it out to the operator
//	  → operator sends escalation.decision (request_id in payload)
//	  → broker routes the decision back to the aspect's conn
//	     with InReplyTo == the original request id.
//
// This is exactly the chain the aspect's wsclient.Request blocks on:
// the returned frame's InReplyTo resolves the pending correlation.
func TestEscalation_RoundTrip(t *testing.T) {
	srv, b, _, _, opTok := newOperatorTestServerFull(t)

	// Operator connection.
	opC := dialWS(t, srv, opTok)

	// Aspect connection, registered as "plumb".
	b.cfg.Tokens.SetTokenForTest("plumb", "plumb-token", false)
	aspectC := dialWS(t, srv, "plumb-token")
	registerAspect(t, aspectC, "plumb")

	// The aspect's funnel issues a correlated escalation.request.
	req, err := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "plumb",
		Tool:   "bash",
		Args:   []byte(`{"command":"id"}`),
		Reason: "policy requires operator approval for tool \"bash\"",
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, aspectC, req)

	// The operator receives the request (skip any roster pushes etc).
	opEnv := expectKindWithin(t, opC, frames.KindEscalationRequest, brokerAsyncWait)
	if opEnv.ID != req.ID {
		t.Fatalf("operator-side request ID = %q, want %q (correlation must survive relay)", opEnv.ID, req.ID)
	}
	gotReq := payloadAs[frames.EscalationRequestPayload](t, opEnv)
	if gotReq.Aspect != "plumb" || gotReq.Tool != "bash" {
		t.Fatalf("operator saw wrong request: %+v", gotReq)
	}

	// The operator approves. The decision carries the correlation id in
	// the PAYLOAD (request_id), NOT the envelope InReplyTo — so the
	// broker's read loop doesn't short-circuit it through routeResponse.
	dec, _ := frames.New(frames.KindEscalationDecision, frames.EscalationDecisionPayload{
		Aspect:    "plumb",
		Decision:  frames.EscalationApprove,
		Note:      "ok this once",
		RequestID: req.ID,
	})
	sendFrame(t, opC, dec)

	// The aspect receives the decision correlated to its original
	// request — this is what unblocks wsclient.Request.
	decEnv := expectKindWithin(t, aspectC, frames.KindEscalationDecision, brokerAsyncWait)
	if decEnv.InReplyTo != req.ID {
		t.Fatalf("decision InReplyTo = %q, want %q (must resolve the aspect's pending Request)", decEnv.InReplyTo, req.ID)
	}
	gotDec := payloadAs[frames.EscalationDecisionPayload](t, decEnv)
	if gotDec.Decision != frames.EscalationApprove {
		t.Fatalf("decision = %q, want approve", gotDec.Decision)
	}
	// Operator identity must be stamped from the authenticated conn.
	if gotDec.Operator != "operator" {
		t.Errorf("decision Operator = %q, want %q (broker stamps from auth)", gotDec.Operator, "operator")
	}
}

// TestEscalation_DenyRoundTrip mirrors the approve case but with a deny
// decision + note, proving the deny path relays intact.
func TestEscalation_DenyRoundTrip(t *testing.T) {
	srv, b, _, _, opTok := newOperatorTestServerFull(t)
	opC := dialWS(t, srv, opTok)
	b.cfg.Tokens.SetTokenForTest("plumb", "plumb-token", false)
	aspectC := dialWS(t, srv, "plumb-token")
	registerAspect(t, aspectC, "plumb")

	req, _ := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "plumb", Tool: "bash", Args: []byte(`{"command":"rm -rf /"}`),
	})
	sendFrame(t, aspectC, req)
	expectKindWithin(t, opC, frames.KindEscalationRequest, brokerAsyncWait)

	dec, _ := frames.New(frames.KindEscalationDecision, frames.EscalationDecisionPayload{
		Aspect: "plumb", Decision: frames.EscalationDeny, Note: "too dangerous", RequestID: req.ID,
	})
	sendFrame(t, opC, dec)

	decEnv := expectKindWithin(t, aspectC, frames.KindEscalationDecision, brokerAsyncWait)
	if decEnv.InReplyTo != req.ID {
		t.Fatalf("deny InReplyTo = %q, want %q", decEnv.InReplyTo, req.ID)
	}
	gotDec := payloadAs[frames.EscalationDecisionPayload](t, decEnv)
	if gotDec.Decision != frames.EscalationDeny || gotDec.Note != "too dangerous" {
		t.Fatalf("deny payload = %+v", gotDec)
	}
}

// TestEscalation_RequestIdentityMismatch: an aspect cannot escalate
// under another aspect's name. The broker rejects with an error frame
// and does NOT fan out to operators.
func TestEscalation_RequestIdentityMismatch(t *testing.T) {
	srv, b, _, _, opTok := newOperatorTestServerFull(t)
	opC := dialWS(t, srv, opTok)
	b.cfg.Tokens.SetTokenForTest("plumb", "plumb-token", false)
	aspectC := dialWS(t, srv, "plumb-token")
	registerAspect(t, aspectC, "plumb")

	// plumb tries to escalate as "anvil".
	req, _ := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "anvil", Tool: "bash",
	})
	sendFrame(t, aspectC, req)

	// Aspect gets an identity-mismatch error correlated to its request.
	errEnv := expectKindWithin(t, aspectC, frames.Kind(string(frames.KindEscalationRequest)+".error"), brokerAsyncWait)
	if errEnv.InReplyTo != req.ID {
		t.Errorf("error InReplyTo = %q, want %q", errEnv.InReplyTo, req.ID)
	}
	// Operator must NOT have received the forged request.
	expectNoFrame(t, opC, 300*time.Millisecond)
}

// TestEscalation_DecisionAspectNotConnected: the operator answers but
// the target aspect has disconnected. The broker errors back to the
// operator rather than panicking on a nil conn.
func TestEscalation_DecisionAspectNotConnected(t *testing.T) {
	srv, _, _, _, opTok := newOperatorTestServerFull(t)
	opC := dialWS(t, srv, opTok)

	dec, _ := frames.New(frames.KindEscalationDecision, frames.EscalationDecisionPayload{
		Aspect: "ghost", Decision: frames.EscalationApprove, RequestID: "01J-fake-id",
	})
	sendFrame(t, opC, dec)

	errEnv := expectKindWithin(t, opC, frames.Kind(string(frames.KindEscalationDecision)+".error"), brokerAsyncWait)
	if errEnv.Kind == "" {
		t.Fatal("expected an error frame for decision to a disconnected aspect")
	}
}

// TestEscalation_RequestFromOperatorRejected: escalation.request is an
// aspect→broker frame. An operator sending it (it falls through
// dispatchOperatorFrame to the aspect switch, where the operator is
// admin) must be rejected, NOT relayed — operators answer, they don't
// ask. We assert the operator gets an error, not its own request back.
func TestEscalation_RequestFromOperatorRejected(t *testing.T) {
	srv, _, _, _, opTok := newOperatorTestServerFull(t)
	opC := dialWS(t, srv, opTok)

	req, _ := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "operator", Tool: "bash",
	})
	sendFrame(t, opC, req)
	errEnv := expectKindWithin(t, opC, frames.Kind(string(frames.KindEscalationRequest)+".error"), brokerAsyncWait)
	if errEnv.InReplyTo != req.ID {
		t.Errorf("error InReplyTo = %q, want %q", errEnv.InReplyTo, req.ID)
	}
}
