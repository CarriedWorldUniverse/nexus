package broker

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func (r *modelConfigTestRig) getDispatchEnabled(t *testing.T, aspect string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", r.url+"/api/admin/aspects/"+aspect+"/dispatch-enabled", nil)
	req.Header.Set("Authorization", "Bearer "+r.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET dispatch-enabled: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET dispatch-enabled status = %d; want 200", resp.StatusCode)
	}
	var got adminDispatchEnabledBody
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode dispatch-enabled: %v", err)
	}
	return got.Enabled
}

func (r *modelConfigTestRig) putDispatchEnabled(t *testing.T, aspect string, enabled bool) {
	t.Helper()
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	req, _ := http.NewRequest("PUT", r.url+"/api/admin/aspects/"+aspect+"/dispatch-enabled", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT dispatch-enabled: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT dispatch-enabled status = %d; want 200", resp.StatusCode)
	}
}

func TestDispatchEnabledGetPut(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	if got := rig.getDispatchEnabled(t, "anvil"); !got {
		t.Fatalf("default = %v, want true", got)
	}
	rig.putDispatchEnabled(t, "anvil", false)
	if got := rig.getDispatchEnabled(t, "anvil"); got {
		t.Fatalf("after PUT false = %v, want false", got)
	}
}
