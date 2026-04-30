package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/nexus/roster"
)

// adminTestRig wires a Broker with an admin surface for tests. Token
// store gets two tokens: one admin (frame), one non-admin (peer aspect).
// Both point at the same broker so we can hit /api/admin/* with each
// and assert behavior.
type adminTestRig struct {
	srv         *httptest.Server
	adminToken  string
	peerToken   string
	shutdownCh  chan struct{}
	compactSeen *atomic.Int32
	rewindSeen  *atomic.Int32
	dispatchErr error
}

func newAdminTestRig(t *testing.T, opts ...func(*AdminCallbacks)) *adminTestRig {
	t.Helper()

	rig := &adminTestRig{
		shutdownCh:  make(chan struct{}, 1),
		compactSeen: &atomic.Int32{},
		rewindSeen:  &atomic.Int32{},
	}

	cb := &AdminCallbacks{
		Shutdown: func(ctx context.Context) error {
			rig.shutdownCh <- struct{}{}
			return nil
		},
		Compact: func(ctx context.Context) error {
			rig.compactSeen.Add(1)
			return nil
		},
		Rewind: func(ctx context.Context, threadID string, turns int) error {
			rig.rewindSeen.Add(1)
			return nil
		},
		DispatchStatus: func(ctx context.Context) (DispatchStatusReport, error) {
			if rig.dispatchErr != nil {
				return DispatchStatusReport{}, rig.dispatchErr
			}
			return DispatchStatusReport{
				ActiveWorkers: 1,
				SoftCap:       3,
				HardCeiling:   7,
				QueueDepth:    0,
				BusyAspects:   []string{"frame"},
			}, nil
		},
	}
	for _, opt := range opts {
		opt(cb)
	}

	// Mint admin + peer tokens in-memory (no DB).
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}
	rig.adminToken = tokens.TokenForAgent("frame")
	rig.peerToken = tokens.TokenForAgent("peer")

	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		Admin:  cb,
	}, r)

	mux := http.NewServeMux()
	mux.Handle("GET /api/aspects", b.auth(http.HandlerFunc(b.handleList)))
	b.registerAdmin(mux)

	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

func (r *adminTestRig) do(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// Auth gating — non-admin tokens get admin_required.
func TestAdmin_RejectsNonAdminToken(t *testing.T) {
	rig := newAdminTestRig(t)

	endpoints := []struct {
		method, path string
	}{
		{"POST", "/api/admin/shutdown"},
		{"POST", "/api/admin/compact"},
		{"POST", "/api/admin/rewind"},
		{"GET", "/api/admin/dispatch-status"},
		{"GET", "/api/admin/roster"},
	}
	for _, ep := range endpoints {
		var body any
		if ep.path == "/api/admin/rewind" {
			body = map[string]any{"thread_id": "t1", "turns": 1}
		}
		resp := rig.do(t, ep.method, ep.path, rig.peerToken, body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with peer token: status=%d want 403", ep.method, ep.path, resp.StatusCode)
		}
		b := bodyJSON(t, resp)
		if b["error"] != "admin_required" {
			t.Errorf("%s %s: error=%v want admin_required", ep.method, ep.path, b["error"])
		}
	}
}

// Auth gating — missing token gets 401.
func TestAdmin_RejectsMissingToken(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "POST", "/api/admin/compact", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestAdmin_Shutdown_KicksOffAndReturns202(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "POST", "/api/admin/shutdown", rig.adminToken, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d want 202", resp.StatusCode)
	}
	body := bodyJSON(t, resp)
	if body["op_id"] == "" {
		t.Error("op_id missing")
	}
	if body["action"] != "shutdown" {
		t.Errorf("action=%v want shutdown", body["action"])
	}

	// Shutdown callback should fire async.
	select {
	case <-rig.shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown callback did not fire")
	}

	// Op should be queryable and report ok status.
	opID := body["op_id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := rig.do(t, "GET", "/api/admin/op/"+opID, rig.adminToken, nil)
		statusBody := bodyJSON(t, statusResp)
		if statusBody["status"] == "ok" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("op never reached status=ok")
}

func TestAdmin_Compact_RoundTrip(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "POST", "/api/admin/compact", rig.adminToken, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d want 202 body=%v", resp.StatusCode, bodyJSON(t, resp))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rig.compactSeen.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if rig.compactSeen.Load() == 0 {
		t.Fatal("compact callback never fired")
	}
}

func TestAdmin_Rewind_BodyValidation(t *testing.T) {
	rig := newAdminTestRig(t)

	cases := []struct {
		name string
		body any
		want int
	}{
		{"happy", map[string]any{"thread_id": "t1", "turns": 2}, http.StatusAccepted},
		{"missing thread_id", map[string]any{"turns": 1}, http.StatusBadRequest},
		{"zero turns", map[string]any{"thread_id": "t1", "turns": 0}, http.StatusBadRequest},
		{"negative turns", map[string]any{"thread_id": "t1", "turns": -1}, http.StatusBadRequest},
		{"unknown field", map[string]any{"thread_id": "t1", "turns": 1, "weird": true}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := rig.do(t, "POST", "/api/admin/rewind", rig.adminToken, tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("status=%d want %d body=%v", resp.StatusCode, tc.want, bodyJSON(t, resp))
			}
		})
	}
}

func TestAdmin_DispatchStatus_Synchronous(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "GET", "/api/admin/dispatch-status", rig.adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body := bodyJSON(t, resp)
	if int(body["active_workers"].(float64)) != 1 {
		t.Errorf("active_workers=%v want 1", body["active_workers"])
	}
	if int(body["soft_cap"].(float64)) != 3 {
		t.Errorf("soft_cap=%v want 3", body["soft_cap"])
	}
	if int(body["hard_ceiling"].(float64)) != 7 {
		t.Errorf("hard_ceiling=%v want 7", body["hard_ceiling"])
	}
}

func TestAdmin_DispatchStatus_PropagatesError(t *testing.T) {
	rig := newAdminTestRig(t)
	rig.dispatchErr = errors.New("queue offline")
	resp := rig.do(t, "GET", "/api/admin/dispatch-status", rig.adminToken, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}
	body := bodyJSON(t, resp)
	if !strings.Contains(fmt.Sprint(body["error"]), "queue offline") {
		t.Errorf("error=%v should contain 'queue offline'", body["error"])
	}
}

func TestAdmin_Roster_Returns200(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "GET", "/api/admin/roster", rig.adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
}

func TestAdmin_Op_NotFound(t *testing.T) {
	rig := newAdminTestRig(t)
	resp := rig.do(t, "GET", "/api/admin/op/does-not-exist", rig.adminToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.StatusCode)
	}
}

func TestAdmin_NotImplemented_WhenCallbackNil(t *testing.T) {
	// Build a rig with all callbacks nil — they should each return 501.
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{}, // empty callbacks
	}, r)
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	adminToken := tokens.TokenForAgent("frame")

	cases := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/admin/shutdown", nil},
		{"POST", "/api/admin/compact", nil},
		{"POST", "/api/admin/rewind", map[string]any{"thread_id": "t", "turns": 1}},
		{"GET", "/api/admin/dispatch-status", nil},
	}
	for _, tc := range cases {
		var rdr io.Reader
		if tc.body != nil {
			raw, _ := json.Marshal(tc.body)
			rdr = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, rdr)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s: status=%d want 501", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_NotRegistered_WhenAdminNil(t *testing.T) {
	// Config without Admin callbacks at all → /api/admin/* not registered → 404.
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	r := roster.New()
	b := New(Config{Tokens: tokens}, r) // no Admin

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/admin/shutdown")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404 (admin surface not registered)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_Op_RecordsError(t *testing.T) {
	rig := newAdminTestRig(t,
		func(cb *AdminCallbacks) {
			cb.Compact = func(ctx context.Context) error {
				return errors.New("disk full")
			}
		},
	)
	resp := rig.do(t, "POST", "/api/admin/compact", rig.adminToken, nil)
	body := bodyJSON(t, resp)
	opID := body["op_id"].(string)

	// Poll until status flips off "running".
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := rig.do(t, "GET", "/api/admin/op/"+opID, rig.adminToken, nil)
		stBody := bodyJSON(t, st)
		if stBody["status"] == "error" {
			if !strings.Contains(fmt.Sprint(stBody["error"]), "disk full") {
				t.Errorf("op error=%v want 'disk full'", stBody["error"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("op never reached error status")
}
