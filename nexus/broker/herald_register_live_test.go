package broker

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
	"github.com/CarriedWorldUniverse/nexus/nexus/cwb/custodian"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// TestLiveHeraldRegister proves the broker redeems a real casket assertion
// presented in a register frame against the live dMon herald.
// Gated on CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG.
func TestLiveHeraldRegister(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG to run the live herald register test")
	}
	srv, _, b := newTestServer(t)
	b.custodian = custodian.New(edge)

	ctx := context.Background()
	tu, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	assertion, err := identity.AgentAssertion([]byte(seed), slug, agentID, tu)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}

	c := dialWS(t, srv, "testtoken")
	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         "live-ha",
			ContextMode:  schemas.ContextGlobal,
			Provider:     "claude-api",
			Port:         0,
			Capabilities: []string{"smoke"},
			SessionID:    "sess-live",
			Home:         "/tmp/x",
			StartedAt:    time.Now().UTC(),
		},
		Assertion: assertion,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	ack := recvFrame(t, c)
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("kind=%s", ack.Kind)
	}
	if got := ackSubject(t, ack); got != agentID {
		t.Fatalf("herald_subject=%q want %q", got, agentID)
	}
}

// TestLiveCWBRequestRelay proves the broker relays an aspect's cwb.request to
// the live herald edge through the connection's custodied client and returns
// the response: GET /api/me must come back as the registered agent's record.
// Gated on the same env vars as TestLiveHeraldRegister.
func TestLiveCWBRequestRelay(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG for the live cwb relay test")
	}
	srv, _, b := newTestServer(t)
	b.custodian = custodian.New(edge)

	ctx := context.Background()
	tu, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	assertion, err := identity.AgentAssertion([]byte(seed), slug, agentID, tu)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}

	c := dialWS(t, srv, "testtoken")
	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         "live-relay",
			ContextMode:  schemas.ContextGlobal,
			Provider:     "claude-api",
			Port:         0,
			Capabilities: []string{"smoke"},
			SessionID:    "sess-live-relay",
			Home:         "/tmp/x",
			StartedAt:    time.Now().UTC(),
		},
		Assertion: assertion,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	ack := recvFrame(t, c)
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("kind=%s", ack.Kind)
	}
	if got := ackSubject(t, ack); got != agentID {
		t.Fatalf("herald_subject=%q want %q", got, agentID)
	}

	// Drive a cwb.request over the SAME connection (heraldClient now bound by
	// the real custodian) and assert the relayed response.
	req, err := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: "herald",
		Method: "GET",
		Path:   "/api/me",
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, req)

	resp, ok := recvFrameWithTimeout(t, c, 15*time.Second)
	if !ok {
		t.Fatal("no cwb.response within timeout")
	}
	if resp.Kind != frames.KindCWBResponse {
		t.Fatalf("kind=%s want %s", resp.Kind, frames.KindCWBResponse)
	}
	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Status != 200 {
		t.Fatalf("relayed /api/me status = %d, body=%s", p.Status, p.Body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(p.Body, &me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.ID != agentID {
		t.Fatalf("relayed identity id = %q, want %q", me.ID, agentID)
	}
	t.Logf("live: cwb.request relayed to herald as the agent; /api/me id=%s", me.ID)
}
