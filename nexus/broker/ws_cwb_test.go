package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func TestCWBRequestRelays(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	cwb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"id":"agent-x"}`))
	}))
	defer cwb.Close()

	srv, _, b := newTestServer(t)
	// Custodian redeems to agent-x and binds a heraldClient pointing at the
	// stub CWB edge. WithStaticToken builds a Client whose Do hits
	// edge/<pillar><path> with Authorization: Bearer <tok>.
	b.custodian = &fakeCustodian{
		redeem: func(context.Context, string) (string, error) { return "agent-x", nil },
		clientFn: func(string) (*client.Client, error) {
			return client.WithStaticToken(cwb.URL, "tok-agent-x"), nil
		},
	}

	conn := dialWS(t, srv, "testtoken")
	defer conn.Close(websocket.StatusNormalClosure, "")
	if ack := registerWith(t, conn, "aspect-a", "assertion-blob"); ack.Kind != frames.KindRegisterAck {
		t.Fatalf("register failed: %s", ack.Kind)
	}

	req, err := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: "herald", Method: "GET", Path: "/api/me",
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, conn, req)
	resp := recvFrame(t, conn)
	if resp.Kind != frames.KindCWBResponse {
		t.Fatalf("kind = %s, want %s", resp.Kind, frames.KindCWBResponse)
	}

	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 200 || string(p.Body) != `{"id":"agent-x"}` {
		t.Fatalf("resp=%+v", p)
	}
	if gotMethod != "GET" || gotPath != "/herald/api/me" || gotAuth != "Bearer tok-agent-x" {
		t.Fatalf("stub saw method=%q path=%q auth=%q", gotMethod, gotPath, gotAuth)
	}
}

func TestCWBRequestUnboundErrors(t *testing.T) {
	srv, _, _ := newTestServer(t) // no custodian => register won't bind heraldClient
	conn := dialWS(t, srv, "testtoken")
	defer conn.Close(websocket.StatusNormalClosure, "")
	if ack := registerWith(t, conn, "aspect-a", ""); ack.Kind != frames.KindRegisterAck {
		t.Fatalf("register failed: %s", ack.Kind)
	}

	req, err := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: "herald", Method: "GET", Path: "/api/me",
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, conn, req)
	resp := recvFrame(t, conn)
	if resp.Kind != frames.Kind("cwb.request.error") {
		t.Fatalf("want cwb.request.error, got %s", resp.Kind)
	}
}
