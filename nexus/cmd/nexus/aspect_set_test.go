package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAspectSetViaBrokerParsesVerbAndFlags(t *testing.T) {
	t.Setenv("NEXUS_ADMIN_TOKEN", "test-admin-token")

	var sawGet, sawPut bool
	var gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/aspects/plumb/provider-binding" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			sawGet = true
			_ = json.NewEncoder(w).Encode(map[string]string{
				"aspect":   "plumb",
				"provider": "claude-api",
				"model":    "claude-opus-4-7",
			})
		case http.MethodPut:
			sawPut = true
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"aspect":   "plumb",
				"provider": gotBody["provider"],
				"model":    gotBody["model"],
			})
		default:
			t.Errorf("method = %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	code := runAspectSubcommand([]string{
		"set", "plumb",
		"--provider", "openai",
		"--model", "gpt-5",
		"--via", srv.URL,
	})
	if code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	if !sawGet || !sawPut {
		t.Fatalf("saw GET=%v PUT=%v; want both", sawGet, sawPut)
	}
	if gotAuth != "Bearer test-admin-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["provider"] != "openai" || gotBody["model"] != "gpt-5" {
		t.Errorf("request body = %#v; want provider=openai model=gpt-5", gotBody)
	}
}

func TestAspectSetViaBrokerAllowsProviderOnly(t *testing.T) {
	t.Setenv("NEXUS_ADMIN_TOKEN", "test-admin-token")

	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"aspect":   "plumb",
				"provider": "claude-api",
				"model":    "keep-this-model",
			})
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"aspect":   "plumb",
				"provider": gotBody["provider"],
				"model":    gotBody["model"],
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	code := runAspectSubcommand([]string{
		"set", "plumb",
		"--provider", "openai",
		"--via", srv.URL,
	})
	if code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	if gotBody["provider"] != "openai" || gotBody["model"] != "keep-this-model" {
		t.Errorf("request body = %#v; want provider=openai model=keep-this-model", gotBody)
	}
}
