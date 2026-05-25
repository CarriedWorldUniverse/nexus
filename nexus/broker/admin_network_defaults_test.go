package broker

import (
	"net/http"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// NEX-294 Slice 2: GET on a fresh DB returns an all-empty
// NetworkDefaults — singleton row exists (INSERT OR IGNORE on
// bootstrap) but no columns set.
func TestAdminNetworkDefaults_GetEmpty(t *testing.T) {
	rig := newModelConfigTestRig(t)

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/network-defaults", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got credentials.NetworkDefaults
	mustDecode(t, resp, &got)
	if got != (credentials.NetworkDefaults{}) {
		t.Errorf("expected zero NetworkDefaults on fresh store; got %+v", got)
	}
}

// NEX-294 Slice 2: PUT sets a subset, GET reflects the change.
// Mirrors the per-aspect model-config test (TestAdminModelConfig_PutThenGet).
func TestAdminNetworkDefaults_PutThenGet(t *testing.T) {
	rig := newModelConfigTestRig(t)

	// Seed a credential the judge_credential default can reference
	// (SetNetworkDefaultField validates existence for *_credential fields).
	seedProviderCredential(t, rig, "deepseek-judge")

	body := `{"judge_model": "deepseek-chat", "judge_credential": "deepseek-judge"}`
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d; want 200", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", rig.url+"/api/admin/network-defaults", nil)
	req2.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	var got credentials.NetworkDefaults
	mustDecode(t, resp2, &got)
	if got.JudgeModel != "deepseek-chat" {
		t.Errorf("JudgeModel = %q; want deepseek-chat", got.JudgeModel)
	}
	if got.JudgeCredential != "deepseek-judge" {
		t.Errorf("JudgeCredential = %q; want deepseek-judge", got.JudgeCredential)
	}
	if got.CompactModel != "" {
		t.Errorf("CompactModel should be unset; got %q", got.CompactModel)
	}
}

// NEX-294 Slice 2: PUT with empty string clears a previously-set field.
// Mirrors TestAdminModelConfig_PutClearsField.
func TestAdminNetworkDefaults_PutClearsField(t *testing.T) {
	rig := newModelConfigTestRig(t)

	// Set judge_model.
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults",
		strings.NewReader(`{"judge_model": "haiku"}`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT set: %v", err)
	}
	resp.Body.Close()

	// Clear via empty string.
	req2, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults",
		strings.NewReader(`{"judge_model": ""}`))
	req2.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("PUT clear: %v", err)
	}
	resp2.Body.Close()

	req3, _ := http.NewRequest("GET", rig.url+"/api/admin/network-defaults", nil)
	req3.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp3.Body.Close()
	var got credentials.NetworkDefaults
	mustDecode(t, resp3, &got)
	if got.JudgeModel != "" {
		t.Errorf("JudgeModel should be cleared; got %q", got.JudgeModel)
	}
}

// NEX-294 Slice 2: non-admin token rejected with 403.
func TestAdminNetworkDefaults_RejectsNonAdmin(t *testing.T) {
	rig := newModelConfigTestRig(t)

	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults",
		strings.NewReader(`{"judge_model": "x"}`))
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PUT status = %d; want 403", resp.StatusCode)
	}
}

// NEX-294 Slice 2: PUT rejects setting judge_credential to a name
// that doesn't exist in the credentials store (existence validation
// in SetNetworkDefaultField — same pattern as NEX-263).
func TestAdminNetworkDefaults_RejectsUnknownCredential(t *testing.T) {
	rig := newModelConfigTestRig(t)
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults",
		strings.NewReader(`{"judge_credential": "does-not-exist"}`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown-credential PUT status = %d; want 400", resp.StatusCode)
	}
}

// NEX-294 Slice 2: PUT with unknown column name rejected. The
// handler's typed adminNetworkDefaultsReq struct + DisallowUnknownFields
// on the JSON decoder catches anything not in the four allowed columns.
func TestAdminNetworkDefaults_RejectsUnknownField(t *testing.T) {
	rig := newModelConfigTestRig(t)
	// primary_model is intentionally absent from NetworkDefaults
	// (primary is per-aspect by design). PUT-ing it must 400.
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/network-defaults",
		strings.NewReader(`{"primary_model": "x"}`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown-field PUT status = %d; want 400", resp.StatusCode)
	}
}

// seedProviderCredential creates a kind=provider credential in the
// test rig's store. Reused across tests that need to reference a
// real credential name in PUT bodies.
func seedProviderCredential(t *testing.T, rig *modelConfigTestRig, name string) {
	t.Helper()
	if err := rig.creds.Set(t.Context(), credentials.UpsertParams{
		Name: name,
		Kind: credentials.KindProvider,
		Bundle: map[string]any{
			"api_shape": "anthropic",
			"base_url":  "https://api.deepseek.com/v1",
			"key":       "sk-test",
		},
		AllowedAspects: []string{"*"},
		Mode:           credentials.ModeProxy,
	}); err != nil {
		t.Fatalf("seed credential %q: %v", name, err)
	}
}
