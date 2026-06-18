package shadowrunner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestGate points a JiraGate at an httptest server (overriding the
// atlassian.net base URL).
func newTestGate(t *testing.T, h http.HandlerFunc) *JiraGate {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	g := NewJiraGate("acme", "bot@acme.test", "tok", DefaultQueueJQL("NEX", "shadow-queue"))
	g.baseURL = srv.URL
	return g
}

func TestJiraGate_HasWork_NonEmpty(t *testing.T) {
	g := newTestGate(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify the gate sends the queue JQL + Basic auth.
		if jql := r.URL.Query().Get("jql"); !strings.Contains(jql, "shadow-queue") {
			t.Errorf("jql = %q, want it to scope to the shadow-queue label", jql)
		}
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("missing Basic auth: %q", auth)
		}
		_, _ = w.Write([]byte(`{"issues":[{"key":"NEX-676"}]}`))
	})
	ok, err := g.HasWork(context.Background())
	if err != nil {
		t.Fatalf("HasWork: %v", err)
	}
	if !ok {
		t.Fatal("HasWork = false, want true (one issue in queue)")
	}
}

func TestJiraGate_HasWork_Empty(t *testing.T) {
	g := newTestGate(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	ok, err := g.HasWork(context.Background())
	if err != nil {
		t.Fatalf("HasWork: %v", err)
	}
	if ok {
		t.Fatal("HasWork = true, want false (empty queue → gate closed, skip drain)")
	}
}

func TestJiraGate_HasWork_HTTPError(t *testing.T) {
	g := newTestGate(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessages":["bad creds"]}`))
	})
	if _, err := g.HasWork(context.Background()); err == nil {
		t.Fatal("HasWork must error on a non-2xx (don't silently treat auth failure as no-work)")
	}
}

func TestDefaultQueueJQL_ExcludesDoneAndBlocked(t *testing.T) {
	jql := DefaultQueueJQL("NEX", "shadow-queue")
	for _, want := range []string{"project = NEX", `labels = "shadow-queue"`, "statusCategory != Done", `status != "Blocked"`} {
		if !strings.Contains(jql, want) {
			t.Errorf("JQL %q missing %q", jql, want)
		}
	}
}
