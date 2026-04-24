package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/runtime/providers"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// mockProvider — reused pattern from compactor tests, tailored to agent.
type mockProvider struct {
	reply       string
	invokeCalls int32
}

func (m *mockProvider) Invoke(_ context.Context, _ providers.InvokeRequest) (providers.InvokeResult, error) {
	atomic.AddInt32(&m.invokeCalls, 1)
	return providers.InvokeResult{
		Output:     m.reply,
		StopReason: providers.StopEndTurn,
		Tokens:     providers.TokenCounts{Input: 10, Output: 20, Total: 30},
	}, nil
}
func (m *mockProvider) Stream(context.Context, providers.InvokeRequest) (providers.StreamIterator, error) {
	return nil, providers.ErrUnsupported
}
func (m *mockProvider) TokenCount(context.Context, string, string) (int, error)             { return 0, nil }
func (m *mockProvider) Compact(context.Context, []providers.Entry, string) (providers.CompactionResult, error) {
	return providers.CompactionResult{}, providers.ErrUnsupported
}
func (m *mockProvider) Embed(context.Context, providers.EmbedRequest) (providers.EmbedResult, error) {
	return providers.EmbedResult{}, providers.ErrUnsupported
}
func (m *mockProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Chat: true, MaxContextTokens: 200_000}
}
func (m *mockProvider) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (m *mockProvider) TriageModel() string                               { return "mock-triage" }

// fakeNexus spins up an httptest.Server simulating the Nexus broker.
// Records which endpoints were hit for assertions.
type fakeNexus struct {
	registers   int32
	heartbeats  int32
	deregisters int32
	token       string
	srv         *httptest.Server
}

func newFakeNexus(token string) *fakeNexus {
	f := &fakeNexus{token: token}
	mux := http.NewServeMux()

	mux.HandleFunc("/aspects/register", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		atomic.AddInt32(&f.registers, 1)
		_ = json.NewEncoder(w).Encode(schemas.RegisterResponse{
			Status: "registered", HeartbeatIntervalS: 1, StaleAfterS: 30,
		})
	})
	mux.HandleFunc("/aspects/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		atomic.AddInt32(&f.heartbeats, 1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/aspects/deregister", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(w, r) {
			return
		}
		atomic.AddInt32(&f.deregisters, 1)
		w.WriteHeader(http.StatusOK)
	})

	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeNexus) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get("Authorization")
	if got != "Bearer "+f.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeNexus) Registers() int32   { return atomic.LoadInt32(&f.registers) }
func (f *fakeNexus) Heartbeats() int32  { return atomic.LoadInt32(&f.heartbeats) }
func (f *fakeNexus) Deregisters() int32 { return atomic.LoadInt32(&f.deregisters) }
func (f *fakeNexus) URL() string        { return f.srv.URL }
func (f *fakeNexus) Close()             { f.srv.Close() }

func newAgent(t *testing.T, nexusURL, token string, provider providers.Provider) *Agent {
	t.Helper()
	a, err := New(Config{
		Home: t.TempDir(),
		Aspect: schemas.AspectConfig{
			Name:        "testaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			Port:        0,
		},
		Provider:  provider,
		NexusURL:  nexusURL,
		AuthToken: token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestNewRequiresFields(t *testing.T) {
	cases := map[string]Config{
		"empty home":    {Aspect: schemas.AspectConfig{Name: "x"}, Provider: &mockProvider{}, NexusURL: "http://x"},
		"empty name":    {Home: "/tmp", Provider: &mockProvider{}, NexusURL: "http://x"},
		"nil provider":  {Home: "/tmp", Aspect: schemas.AspectConfig{Name: "x"}, NexusURL: "http://x"},
		"empty nexus":   {Home: "/tmp", Aspect: schemas.AspectConfig{Name: "x"}, Provider: &mockProvider{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestStartRegistersAndDeregisters(t *testing.T) {
	nx := newFakeNexus("tok")
	defer nx.Close()
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()

	// Wait for registration.
	waitFor(t, 2*time.Second, func() bool { return nx.Registers() >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned err: %v", err)
	}
	if nx.Deregisters() != 1 {
		t.Errorf("Deregisters = %d, want 1", nx.Deregisters())
	}
}

func TestStartHeartbeatsWhileRunning(t *testing.T) {
	nx := newFakeNexus("tok")
	defer nx.Close()
	a := newAgent(t, nx.URL(), "tok", &mockProvider{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()

	// Fake Nexus returned HeartbeatIntervalS=1; wait for ≥2 beats.
	waitFor(t, 5*time.Second, func() bool { return nx.Heartbeats() >= 2 })

	cancel()
	<-done
}

func TestTurnDispatchHitsProviderAndTree(t *testing.T) {
	nx := newFakeNexus("tok")
	defer nx.Close()
	mp := &mockProvider{reply: "hello from the model"}
	a := newAgent(t, nx.URL(), "tok", mp)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, 2*time.Second, func() bool { return a.ListenURL() != "" && nx.Registers() >= 1 })

	// Post a turn to the agent's /turn.
	body, _ := json.Marshal(TurnRequest{Prompt: "ping?"})
	resp, err := http.Post(a.ListenURL()+"/turn", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("turn status = %d: %s", resp.StatusCode, b)
	}
	var out TurnResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Output != "hello from the model" {
		t.Errorf("Output = %q", out.Output)
	}
	if len(out.EntryIDs) != 2 {
		t.Errorf("EntryIDs len = %d, want 2 (user + assistant)", len(out.EntryIDs))
	}
	if atomic.LoadInt32(&mp.invokeCalls) != 1 {
		t.Errorf("Invoke calls = %d, want 1", atomic.LoadInt32(&mp.invokeCalls))
	}

	// Second turn — provider should see the prior turn in context.
	body, _ = json.Marshal(TurnRequest{Prompt: "follow-up"})
	resp2, err := http.Post(a.ListenURL()+"/turn", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("second turn status = %d", resp2.StatusCode)
	}

	// Session tree should contain 4 entries (user1, asst1, user2, asst2).
	branch, err := a.Tree().Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(branch) != 4 {
		t.Errorf("branch len after 2 turns = %d, want 4", len(branch))
	}
}

func TestTurnRejectsBadRequests(t *testing.T) {
	nx := newFakeNexus("tok")
	defer nx.Close()
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, 2*time.Second, func() bool { return a.ListenURL() != "" && nx.Registers() >= 1 })

	// Empty prompt.
	body, _ := json.Marshal(TurnRequest{Prompt: ""})
	resp, _ := http.Post(a.ListenURL()+"/turn", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", resp.StatusCode)
	}

	// Wrong method.
	req, _ := http.NewRequest(http.MethodGet, a.ListenURL()+"/turn", nil)
	resp2, _ := http.DefaultClient.Do(req)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /turn status = %d, want 405", resp2.StatusCode)
	}

	// Bad JSON.
	resp3, _ := http.Post(a.ListenURL()+"/turn", "application/json", strings.NewReader("not json"))
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON status = %d, want 400", resp3.StatusCode)
	}
}

func TestAuthFailureOnRegister(t *testing.T) {
	nx := newFakeNexus("correct-token")
	defer nx.Close()
	a := newAgent(t, nx.URL(), "wrong-token", &mockProvider{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := a.Start(ctx)
	if err == nil {
		t.Error("expected error for bad token")
	}
	if nx.Registers() != 0 {
		t.Error("registration should not have succeeded")
	}
}

func TestHealthz(t *testing.T) {
	nx := newFakeNexus("tok")
	defer nx.Close()
	a := newAgent(t, nx.URL(), "tok", &mockProvider{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, 2*time.Second, func() bool { return a.ListenURL() != "" })

	resp, err := http.Get(a.ListenURL() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}
}

// waitFor polls `cond` every 10ms until it returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// One last check to give a clearer failure than "deadline elapsed"
	if !cond() {
		t.Fatalf("waitFor: condition never met within %s", timeout)
	}
}

// Compile-time assertion that mockProvider actually implements Provider —
// catches interface drift early.
var _ providers.Provider = (*mockProvider)(nil)

// silence unused-import warning when errors isn't referenced in a slim build
var _ = errors.New
