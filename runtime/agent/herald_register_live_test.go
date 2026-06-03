package agent

// Gated live test: requires a reachable CWB edge (dMon herald on the tailnet)
// and a PRE-PROVISIONED agent under CW_IT_OWNER_SEED / CW_IT_AGENT_SLUG.
//
//   CW_IT_EDGE        - CWB edge base (e.g. http://dmonextreme...:8080)
//   CW_IT_OWNER_SEED  - owner seed the agent was derived from
//   CW_IT_AGENT_ID    - the agent's herald UUID
//   CW_IT_AGENT_SLUG  - the agent's slug
//
// It builds a bootstrap keyfile, has the aspect runtime discover + sign the
// assertion (buildAssertion), then redeems it via the jwt-bearer grant — the
// same redemption the broker custodian performs — proving herald accepts the
// keyfile-signed assertion end to end.

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
	"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"
)

func TestLiveAspectHeraldRegister(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG for the live aspect register test")
	}

	priv, pub, err := casket.DeriveAgentKey([]byte(seed), slug)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	kf := &heraldkeyfile.Keyfile{
		Key:         base64.StdEncoding.EncodeToString(priv),
		KeyID:       agentID,
		URL:         edge, // edge is http(s); httpEdge passes it through to the discovery origin
		Slug:        slug,
		Fingerprint: identity.Fingerprint(pub),
	}

	// The aspect runtime discovers the token endpoint through the keyfile url
	// and signs the assertion.
	a := &Agent{cfg: Config{HeraldKeyfile: kf}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	assertion, err := a.buildAssertion(ctx)
	if err != nil {
		t.Fatalf("buildAssertion: %v", err)
	}
	if assertion == "" {
		t.Fatal("empty assertion")
	}

	// Redeem it exactly as the broker custodian does — proves herald accepts it.
	tok, err := oidc.New(edge).JWTBearerGrant(ctx, assertion)
	if err != nil {
		t.Fatalf("jwt-bearer grant (herald rejected the keyfile-signed assertion): %v", err)
	}
	claims, err := identity.DecodeAccessClaims(tok.AccessToken)
	if err != nil {
		t.Fatalf("decode access token: %v", err)
	}
	if claims["sub"] != agentID {
		t.Fatalf("access token sub = %v, want %v", claims["sub"], agentID)
	}
	t.Logf("live: keyfile-signed assertion accepted by herald; access-token sub=%v", claims["sub"])
}
