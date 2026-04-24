package broker

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// TestSendTurnEndToEnd wires the Broker's SendTurn path all the way
// through: aspect registers over WS, broker dispatches a turn frame,
// aspect sends turn.result back, broker correlates and returns.
func TestSendTurnEndToEnd(t *testing.T) {
	srv, _, b := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	// Register as an aspect.
	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	if ack := recvFrame(t, c); ack.Kind != frames.KindRegisterAck {
		t.Fatalf("register ack kind = %q", ack.Kind)
	}

	// Start a fake-aspect reader that responds to any turn frame
	// with a canned turn.result correlated to the request.
	turnRespCh := make(chan struct{})
	go func() {
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			if env.Kind != frames.KindTurn {
				continue
			}
			resp, _ := frames.NewResponse(frames.KindTurnResult, env.ID, frames.TurnResultPayload{
				Output:     "hello from fake aspect",
				StopReason: "end_turn",
				Tokens:     frames.TokenUsage{Input: 5, Output: 10, Total: 15},
				EntryIDs:   []string{"e-u", "e-a"},
			})
			raw, _ := frames.Encode(resp)
			_ = c.Write(ctx, websocket.MessageText, raw)
			close(turnRespCh)
			return
		}
	}()

	// Drive the dispatch from the broker side.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := b.SendTurn(ctx, "smoketest", frames.TurnPayload{Prompt: "hi"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if result.Output != "hello from fake aspect" {
		t.Errorf("Output = %q", result.Output)
	}
	if result.Tokens.Total != 15 {
		t.Errorf("Tokens.Total = %d", result.Tokens.Total)
	}
	if len(result.EntryIDs) != 2 {
		t.Errorf("EntryIDs len = %d", len(result.EntryIDs))
	}

	select {
	case <-turnRespCh:
	case <-time.After(1 * time.Second):
		t.Error("fake aspect never received the turn frame")
	}
}

// TestSendTurnToUnregisteredAspect returns ErrAspectNotConnected.
func TestSendTurnToUnregisteredAspect(t *testing.T) {
	_, _, b := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := b.SendTurn(ctx, "nobody-home", frames.TurnPayload{Prompt: "hi"})
	if !errors.Is(err, ErrAspectNotConnected) {
		t.Errorf("err = %v, want ErrAspectNotConnected", err)
	}
}

// TestSendTurnPropagatesTurnError surfaces a turn.error from the
// aspect as a Go error from SendTurn.
func TestSendTurnPropagatesTurnError(t *testing.T) {
	srv, _, b := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "brokenaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/brk",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	_ = recvFrame(t, c)

	// Fake aspect replies with turn.error.
	go func() {
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			if env.Kind != frames.KindTurn {
				continue
			}
			resp, _ := frames.NewResponse(frames.Kind("turn.error"), env.ID, map[string]string{
				"error": "provider simulated failure",
			})
			raw, _ := frames.Encode(resp)
			_ = c.Write(ctx, websocket.MessageText, raw)
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := b.SendTurn(ctx, "brokenaspect", frames.TurnPayload{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error from SendTurn when aspect returns turn.error")
	}
	if !contains(err.Error(), "provider simulated failure") {
		t.Errorf("err = %v, should mention simulated failure", err)
	}
}

// TestSendTurnContextTimeout respects the caller's context.
func TestSendTurnContextTimeout(t *testing.T) {
	srv, _, b := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "slowaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/slow",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	_ = recvFrame(t, c)

	// Don't start a reader — the aspect never responds.

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := b.SendTurn(ctx, "slowaspect", frames.TurnPayload{Prompt: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// TestDisconnectUnbindsDispatcher — once the aspect's WS drops, the
// dispatcher must no longer route to it.
func TestDisconnectUnbindsDispatcher(t *testing.T) {
	srv, _, b := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "ghostaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/ghost",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	_ = recvFrame(t, c)

	_ = c.Close(websocket.StatusGoingAway, "bye")

	// Wait for the server-side cleanup to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.dispatcher.mu.Lock()
		_, stillBound := b.dispatcher.connsByAspect["ghostaspect"]
		b.dispatcher.mu.Unlock()
		if !stillBound {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := b.SendTurn(ctx, "ghostaspect", frames.TurnPayload{Prompt: "hi"})
	if !errors.Is(err, ErrAspectNotConnected) {
		t.Errorf("SendTurn after disconnect err = %v, want ErrAspectNotConnected", err)
	}
}

// Silence httptest unused-import if build is slim.
var _ = httptest.NewServer

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
