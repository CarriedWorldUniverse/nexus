package obsforward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// fakeSender records every envelope passed to Send. The mutex keeps
// the tests goroutine-safe even though current tests are single-
// threaded — useful protection for future expansion.
type fakeSender struct {
	mu   sync.Mutex
	sent []frames.Envelope
	err  error // when non-nil, every Send fails with this
}

func (s *fakeSender) Send(_ context.Context, env frames.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, env)
	return nil
}

func (s *fakeSender) snapshot() []frames.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]frames.Envelope, len(s.sent))
	copy(cp, s.sent)
	return cp
}

func newTestForwarder() (*WSForwarder, *fakeSender) {
	s := &fakeSender{}
	return New(s, "plumb", slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func TestWSForwarder_BeginTurnEmitsObserveBegin(t *testing.T) {
	w, s := newTestForwarder()
	w.BeginTurn("turn-123", "main", "claude-opus-4-7", "claudecode", 99)

	sent := s.snapshot()
	if len(sent) != 1 {
		t.Fatalf("len=%d want 1", len(sent))
	}
	if sent[0].Kind != frames.KindObserveBegin {
		t.Fatalf("kind=%s want %s", sent[0].Kind, frames.KindObserveBegin)
	}
	var p frames.ObserveBeginPayload
	if err := json.Unmarshal(sent[0].Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.TurnID != "turn-123" || p.Label != "main" || p.Model != "claude-opus-4-7" ||
		p.Provider != "claudecode" || p.TriggerMsg != 99 || p.Aspect != "plumb" {
		t.Errorf("payload=%+v", p)
	}
}

func TestWSForwarder_EndTurnEmitsObserveEnd(t *testing.T) {
	w, s := newTestForwarder()
	w.EndTurn()
	sent := s.snapshot()
	if len(sent) != 1 || sent[0].Kind != frames.KindObserveEnd {
		t.Fatalf("sent=%+v", sent)
	}
}

func TestWSForwarder_OnBridleEventEncodesAllKinds(t *testing.T) {
	cases := []struct {
		name    string
		ev      bridle.Event
		expKind string
	}{
		{"ModelChunk", bridle.ModelChunk{Text: "hi"}, EventKindModelChunk},
		{"ToolCallStart", bridle.ToolCallStart{ID: "t1", Name: "Read", Args: json.RawMessage(`{"file_path":"a"}`)}, EventKindToolCallStart},
		{"ToolCallResult", bridle.ToolCallResult{ID: "t1", Result: json.RawMessage(`"ok"`)}, EventKindToolCallResult},
		{"StepBoundary", bridle.StepBoundary{Step: 2}, EventKindStepBoundary},
		{"TurnDone", bridle.TurnDone{Result: bridle.TurnResult{FinalText: "done"}}, EventKindTurnDone},
		{"TurnError", bridle.TurnError{Err: errors.New("boom"), Stage: "provider"}, EventKindTurnError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, s := newTestForwarder()
			w.OnBridleEvent(tc.ev)
			sent := s.snapshot()
			if len(sent) != 1 || sent[0].Kind != frames.KindObserveEvent {
				t.Fatalf("sent=%+v", sent)
			}
			var p frames.ObserveEventPayload
			if err := json.Unmarshal(sent[0].Payload, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.EventKind != tc.expKind {
				t.Errorf("EventKind=%s want %s", p.EventKind, tc.expKind)
			}
			if len(p.Event) == 0 {
				t.Errorf("Event body empty")
			}
		})
	}
}

func TestWSForwarder_TurnErrorStringifiesError(t *testing.T) {
	w, s := newTestForwarder()
	w.OnBridleEvent(bridle.TurnError{Err: errors.New("provider boom"), Stage: "provider"})
	sent := s.snapshot()
	var p frames.ObserveEventPayload
	if err := json.Unmarshal(sent[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Err   string `json:"err"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(p.Event, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Err != "provider boom" || raw.Stage != "provider" {
		t.Errorf("decoded=%+v want Err=provider boom Stage=provider", raw)
	}
}

func TestWSForwarder_SendFailureDoesNotPanic(t *testing.T) {
	s := &fakeSender{err: errors.New("wire down")}
	w := New(s, "plumb", slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Should not panic on any of the three boundary methods.
	w.BeginTurn("t", "main", "m", "p", 0)
	w.OnBridleEvent(bridle.ModelChunk{Text: "x"})
	w.EndTurn()
}
