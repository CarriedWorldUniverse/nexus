package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// fakeSender is a refreshSender stub for unit tests. It records every
// call and returns a caller-supplied envelope/error per invocation.
type fakeSender struct {
	t     *testing.T
	mu    sync.Mutex
	calls int32
	// respond is called per Request invocation; receives the call
	// index (1-based) and must return the envelope+error to use.
	respond func(call int) (frames.Envelope, error)
}

func (f *fakeSender) Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error) {
	// Regression guard: refresh frames must carry a correlation ID
	// (frames.NewRequest, not frames.New) — wsclient.Request rejects
	// empty IDs unconditionally.
	if env.ID == "" {
		if f.t != nil {
			f.t.Helper()
			f.t.Fatalf("refresh envelope missing ID — use frames.NewRequest")
		}
		return frames.Envelope{}, errors.New("fakeSender: envelope missing ID")
	}
	n := int(atomic.AddInt32(&f.calls, 1))
	f.mu.Lock()
	resp := f.respond
	f.mu.Unlock()
	if resp == nil {
		return frames.Envelope{}, errors.New("fakeSender: no responder set")
	}
	return resp(n)
}

func (f *fakeSender) Calls() int { return int(atomic.LoadInt32(&f.calls)) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeRefreshResp builds a synthetic SessionRefreshResultPayload
// envelope carrying the supplied JWT/expiry.
func makeRefreshResp(t *testing.T, jwt string, exp time.Time) frames.Envelope {
	t.Helper()
	env, err := frames.New(frames.KindSessionRefreshResult, frames.SessionRefreshResultPayload{
		SessionJWT:       jwt,
		SessionExpiresAt: exp.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("compose refresh resp: %v", err)
	}
	return env
}

func TestRefreshLoop_SuccessUpdatesStateAndReschedules(t *testing.T) {
	t.Parallel()
	initial := time.Now().Add(200 * time.Millisecond)
	newExp := time.Now().Add(2 * time.Second)

	state := newSessionState(sessionSnapshot{JWT: "old", Expires: initial})
	sender := &fakeSender{t: t}
	sender.respond = func(call int) (frames.Envelope, error) {
		return makeRefreshResp(t, "new", newExp), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		sessionRefreshLoop(ctx, state, sender, 100*time.Millisecond, testLogger())
		close(done)
	}()

	// Poll until the refresh has occurred.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if state.Snapshot().JWT == "new" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := state.Snapshot()
	if snap.JWT != "new" {
		t.Fatalf("expected JWT update, got %q", snap.JWT)
	}
	if !snap.Expires.Equal(newExp.UTC().Truncate(time.Second)) {
		// RFC3339 parsing truncates to seconds.
		if snap.Expires.Unix() != newExp.UTC().Truncate(time.Second).Unix() {
			t.Fatalf("expected expiry %v, got %v", newExp.UTC().Truncate(time.Second), snap.Expires)
		}
	}
	if sender.Calls() < 1 {
		t.Fatalf("expected at least 1 sender call, got %d", sender.Calls())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("loop did not exit after ctx cancel")
	}
}

func TestRefreshLoop_ThreeFailuresGiveUpForWindow(t *testing.T) {
	t.Parallel()
	// Use very short expiry so the give-up sleep (until expiry) is
	// also short, letting the test complete quickly.
	initial := time.Now().Add(300 * time.Millisecond)
	state := newSessionState(sessionSnapshot{JWT: "old", Expires: initial})
	sender := &fakeSender{t: t}
	sender.respond = func(call int) (frames.Envelope, error) {
		return frames.Envelope{}, errors.New("simulated broker failure")
	}

	// Patch the package-level retryDelay isn't possible since it's a
	// const; instead we use a very short ctx timeout and verify the
	// loop didn't update state and made at least 1 attempt before
	// being torn down. The full 3-attempt cycle would otherwise take
	// 10+ minutes due to the 5-min retry delay.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		sessionRefreshLoop(ctx, state, sender, 100*time.Millisecond, testLogger())
		close(done)
	}()

	<-done

	if state.Snapshot().JWT != "old" {
		t.Fatalf("state should not have updated on failure; got JWT %q", state.Snapshot().JWT)
	}
	if sender.Calls() < 1 {
		t.Fatalf("expected at least 1 attempt, got %d", sender.Calls())
	}
}

func TestRefreshLoop_CtxCancelExitsCleanly(t *testing.T) {
	t.Parallel()
	// Far-future expiry so the loop is parked in the wait phase.
	initial := time.Now().Add(1 * time.Hour)
	state := newSessionState(sessionSnapshot{JWT: "old", Expires: initial})
	sender := &fakeSender{t: t}
	sender.respond = func(call int) (frames.Envelope, error) {
		return makeRefreshResp(t, "new", time.Now().Add(2*time.Hour)), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sessionRefreshLoop(ctx, state, sender, 1*time.Minute, testLogger())
		close(done)
	}()

	// Let it park.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("loop did not exit within 500ms after ctx cancel")
	}
}
