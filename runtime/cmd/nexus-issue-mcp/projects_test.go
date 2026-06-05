package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/issuesrest"
)

func TestListProjectsClientRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(issuesrest.ProjectsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method = "+r.Method+", want GET", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("include_archived") != "true" {
			http.Error(w, "include_archived = "+r.URL.Query().Get("include_archived")+", want true", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"NEX","name":"Nexus","archived":false},{"key":"OLD","name":"Archived","archived":true}]`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	client := newClient(srv.URL, "test-token", false, slog.Default())
	var out []map[string]any
	if err := client.get(context.Background(), issuesrest.ProjectsPath+"?include_archived=true", &out); err != nil {
		t.Fatalf("client.get: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(projects) = %d, want 2: %#v", len(out), out)
	}
	if out[0]["key"] != "NEX" || out[1]["key"] != "OLD" {
		t.Fatalf("projects = %#v, want NEX and OLD", out)
	}
}
