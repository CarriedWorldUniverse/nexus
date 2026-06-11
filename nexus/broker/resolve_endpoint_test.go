package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

// mintSessionJWT signs a bare aspect session JWT (sub=name) against the
// fixture's signing secret — the same shape MintDerivedCredential and
// validate produce. Used to drive the JWT-boot resolve endpoint without
// a keyfile.
func mintSessionJWT(t *testing.T, secret []byte, name string) string {
	t.Helper()
	tok, err := jwt.Sign(secret, jwt.Claims{
		Iss: "nexus://fixture-nexus",
		Sub: name,
		Iat: 1700000000,
		Exp: 4070000000, // far future
	})
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return tok
}

func postResolve(t *testing.T, url, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/aspect/resolve", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST resolve: %v", err)
	}
	return resp
}

// A base aspect's session JWT resolves its own persona + provider/model
// — the keyfile-less counterpart of validate.
func TestEndpoint_Resolve_BaseAspect(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	if err := f.store.PersonalitySet(context.Background(), aspects.Personality{
		AspectName: "plumb", SoulMD: "plumb soul",
	}); err != nil {
		t.Fatal(err)
	}
	tok := mintSessionJWT(t, f.signingSec, "plumb")

	resp := postResolve(t, f.srv.URL, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.SessionJWT != tok {
		t.Errorf("resolve must echo the presented JWT: ok=%v jwt match=%v", got.OK, got.SessionJWT == tok)
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus-4-7" {
		t.Errorf("provider/model = %q/%q", got.Provider, got.Model)
	}
	if got.Personality.SoulMD != "plumb soul" {
		t.Errorf("persona = %+v", got.Personality)
	}
}

// A hand's derived-name JWT resolves the PARENT's persona AND provider
// (inheritance), with the derived name echoed (truthful lineage).
func TestEndpoint_Resolve_DerivedInheritsParent(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	if err := f.store.PersonalitySet(context.Background(), aspects.Personality{
		AspectName: "plumb", SoulMD: "plumb soul",
	}); err != nil {
		t.Fatal(err)
	}
	// A real derived credential, as the Runner would inject into the Job.
	tok := mintSessionJWT(t, f.signingSec, "plumb.fathom")

	resp := postResolve(t, f.srv.URL, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus-4-7" {
		t.Errorf("hand must inherit parent provider/model, got %q/%q", got.Provider, got.Model)
	}
	if got.Personality.SoulMD != "plumb soul" {
		t.Errorf("hand must serve the parent persona, got %+v", got.Personality)
	}
}

// NEX-609: the MCP profile is served to BASE aspects but NOT to hands.
// A hand Job mounts no keyfile, so the parent's profile entries (which
// authenticate via /etc/nexus/keyfile.json) could never boot in the
// hand's pod — and the spawn tool (no sub-of-sub) lives there too.
func TestEndpoint_Resolve_MCPProfileBaseOnlyNotDerived(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	profile := `{"mcpServers":{"nexus-comms-mcp":{"command":"/usr/local/bin/nexus-comms-mcp"}}}`
	if err := f.creds.SetMCPProfile(context.Background(), "plumb", profile); err != nil {
		t.Fatalf("SetMCPProfile: %v", err)
	}

	// Base aspect resolve carries the profile.
	resp := postResolve(t, f.srv.URL, mintSessionJWT(t, f.signingSec, "plumb"))
	defer resp.Body.Close()
	var base validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&base); err != nil {
		t.Fatal(err)
	}
	if base.MCPProfile != profile {
		t.Errorf("base resolve MCPProfile = %q, want the stored profile", base.MCPProfile)
	}

	// Hand resolve must not.
	resp2 := postResolve(t, f.srv.URL, mintSessionJWT(t, f.signingSec, "plumb.fathom"))
	defer resp2.Body.Close()
	var hand validateResponse
	if err := json.NewDecoder(resp2.Body).Decode(&hand); err != nil {
		t.Fatal(err)
	}
	if hand.MCPProfile != "" {
		t.Errorf("derived resolve MCPProfile = %q, want empty (hands have no keyfile to auth MCP servers)", hand.MCPProfile)
	}
}

func TestEndpoint_Resolve_MissingBearer(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	resp := postResolve(t, f.srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestEndpoint_Resolve_BadToken(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	resp := postResolve(t, f.srv.URL, "not-a-jwt")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestEndpoint_Resolve_UnknownAspect(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	tok := mintSessionJWT(t, f.signingSec, "ghost.umbra")
	resp := postResolve(t, f.srv.URL, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}
