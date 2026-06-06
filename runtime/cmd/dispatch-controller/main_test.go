package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDispatchControllerRegisterProvider(t *testing.T) {
	if got := dispatchControllerRegisterProvider("claude-api"); got != "claude-api" {
		t.Fatalf("non-empty provider = %q, want claude-api", got)
	}
	if got := dispatchControllerRegisterProvider("   "); got != "dispatch-controller" {
		t.Fatalf("empty provider fallback = %q, want dispatch-controller", got)
	}
}

func TestDispatchHealthHandler(t *testing.T) {
	cases := []struct {
		name      string
		connected bool
		ready     bool
		wantCode  int
		wantBody  string
	}{
		{
			name:     "disconnected",
			wantCode: http.StatusServiceUnavailable,
			wantBody: "ws disconnected\n",
		},
		{
			name:      "connected but register barrier parked",
			connected: true,
			wantCode:  http.StatusServiceUnavailable,
			wantBody:  "ws registering\n",
		},
		{
			name:      "ready",
			connected: true,
			ready:     true,
			wantCode:  http.StatusOK,
			wantBody:  "ok\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			dispatchHealthHandler(fakeReadiness{
				connected: tc.connected,
				ready:     tc.ready,
			}).ServeHTTP(rec, req)

			resp := rec.Result()
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d; body %q", resp.StatusCode, tc.wantCode, string(body))
			}
			if string(body) != tc.wantBody {
				t.Fatalf("body = %q, want %q", string(body), tc.wantBody)
			}
		})
	}
}

func TestDispatchHealthHandlerNotFound(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/other", nil)
	rec := httptest.NewRecorder()
	dispatchHealthHandler(fakeReadiness{connected: true, ready: true}).ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Result().StatusCode)
	}
}

type fakeReadiness struct {
	connected bool
	ready     bool
}

func (f fakeReadiness) Connected() bool { return f.connected }
func (f fakeReadiness) Ready() bool     { return f.ready }
