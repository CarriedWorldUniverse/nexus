package custodian

import (
	"context"
	"os"
	"testing"

	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
)

// TestLiveCustodian redeems a provisioned agent's assertion at the live herald,
// then uses the custodian's client to call a pillar AS that agent (identity-
// derived authz proven) and forces a refresh.
//
// Gated on CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG.
func TestLiveCustodian(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG to run the live custodian test")
	}
	ctx := context.Background()
	tu, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	assertion, err := identity.AgentAssertion([]byte(seed), slug, agentID, tu)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}

	c := New(edge)
	sub, err := c.Redeem(ctx, assertion)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if sub != agentID {
		t.Fatalf("subject %q != agentID %q", sub, agentID)
	}
	cl, err := c.Client(sub)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	// Call /api/me AS the agent — proves the custodied token carries the agent's identity.
	ui, err := herald.Me(ctx, cl)
	if err != nil {
		t.Fatalf("herald.Me via custodian: %v", err)
	}
	if ui.ID != agentID || ui.Kind != "agent" {
		t.Fatalf("Me as agent: %+v", ui)
	}
	// Force a refresh, then call again.
	if _, err := (&source{cust: c, subject: sub}).Refresh(ctx); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if _, err := herald.Me(ctx, cl); err != nil {
		t.Fatalf("Me after refresh: %v", err)
	}
}
