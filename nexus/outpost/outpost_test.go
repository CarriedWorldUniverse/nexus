package outpost

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/internal/testcerts"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// fakeNexus is a tiny WS server that records incoming frames and
// acks outpost.register. Used in place of a real Nexus for tests.
type fakeNexus struct {
	srv          *httptest.Server
	token        string
	mu           sync.Mutex
	conns        []*websocket.Conn
	outpostRegistered atomic.Int32
	aspectRegistered  atomic.Int32
	ch                chan frames.Envelope
}

func newFakeNexus(t *testing.T, token string) *fakeNexus {
	t.Helper()
	f := &fakeNexus{
		token: token,
		ch:    make(chan frames.Envelope, 32),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		wsc.SetReadLimit(1 << 20)
		f.mu.Lock()
		f.conns = append(f.conns, wsc)
		f.mu.Unlock()
		f.serveLoop(wsc)
	}))
	t.Cleanup(func() {
		f.srv.Close()
	})
	return f
}

func (f *fakeNexus) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/connect" }

func (f *fakeNexus) serveLoop(wsc *websocket.Conn) {
	ctx := context.Background()
	for {
		_, data, err := wsc.Read(ctx)
		if err != nil {
			return
		}
		env, err := frames.Decode(data)
		if err != nil {
			continue
		}
		switch env.Kind {
		case frames.KindOutpostRegister:
			f.outpostRegistered.Add(1)
			ack, _ := frames.NewResponse(frames.KindOutpostRegisterAck, env.ID, frames.OutpostRegisterAckPayload{
				HeartbeatIntervalS: 15,
			})
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
		case frames.KindRegister:
			f.aspectRegistered.Add(1)
			ack, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
				HeartbeatIntervalS: 15,
				StaleAfterS:        30,
			})
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
			select {
			case f.ch <- env:
			default:
			}
		default:
			select {
			case f.ch <- env:
			default:
			}
		}
	}
}

// freePort picks an available TCP port for the Outpost listener.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestOutpostRegistersUpstream(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	o, err := New(Config{
		ListenAddr:  freePort(t),
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.outpostRegistered.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.outpostRegistered.Load() == 0 {
		t.Fatal("outpost never registered upstream")
	}

	cancel()
	<-done
}

func TestAspectConnectIsForwardedWithViaStamp(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	listenAddr := freePort(t)
	o, err := New(Config{
		ListenAddr:  listenAddr,
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost-42",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Wait for the upstream register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.outpostRegistered.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.outpostRegistered.Load() == 0 {
		t.Fatal("outpost never registered upstream; abort")
	}

	// Connect as an aspect to the Outpost. Outpost serves TLS now, so
	// dial with wss:// + a TLS-skip-verify HTTPClient since the test
	// cert is self-signed and not in any trust store.
	if !strings.Contains(listenAddr, ":") {
		t.Fatalf("unexpected listen addr format: %q", listenAddr)
	}
	outpostURL := "wss://" + listenAddr + "/connect"

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	c, _, err := websocket.Dial(dialCtx, outpostURL, &websocket.DialOptions{
		HTTPClient: insecureClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer tok"}},
	})
	if err != nil {
		t.Fatalf("aspect dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	// Send a register frame.
	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "viaaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-x",
			Home:        "/tmp/viaaspect",
			StartedAt:   time.Now().UTC(),
		},
	})
	raw, _ := frames.Encode(regEnv)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	if err := c.Write(writeCtx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	// Wait for the fakeNexus to see it, with via_outpost stamped.
	select {
	case env := <-nx.ch:
		if env.Kind != frames.KindRegister {
			t.Fatalf("fakeNexus got kind %q, want register", env.Kind)
		}
		var forwarded frames.ForwardedRegisterPayload
		if err := frames.PayloadAs(env, &forwarded); err != nil {
			t.Fatal(err)
		}
		if forwarded.Name != "viaaspect" {
			t.Errorf("forwarded name = %q", forwarded.Name)
		}
		if forwarded.ViaOutpost != "test-outpost-42" {
			t.Errorf("via_outpost = %q, want test-outpost-42", forwarded.ViaOutpost)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fakeNexus never received the forwarded register")
	}
}

// Silence unused imports if slim build.
var _ = json.Marshal
