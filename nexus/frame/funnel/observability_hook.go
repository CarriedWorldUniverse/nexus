package funnel

import (
	"context"
	"strings"
	"sync"

	"github.com/CarriedWorldUniverse/bridle"
)

// ObservabilityHook receives bridle's raw event stream plus turn-boundary
// signals from the funnel. Implemented by observability.Grouper.
//
// Dual-scoping with Config.Events is intentional and documented in
// docs/2026-05-12-funnel-observability-audit.md §4: Config.Events fires
// once per Deliberate (lifecycle telemetry), while ObservabilityHook
// fires 1–3 times per Deliberate (main, optional compact, optional
// filter-judge). They share time boundaries but are different events.
// Do not pair them N-to-N.
//
// Implementations must be safe for concurrent invocation — the funnel
// calls BeginTurn / OnBridleEvent / EndTurn from the same goroutine
// per turn, but multiple aspects share one Hub.
type ObservabilityHook interface {
	// BeginTurn opens a turn snapshot. label distinguishes the call
	// site: "main" for the deliberation turn, "compact" for mid-
	// deliberation summarization, "filter-judge" for the post-hoc
	// cheap-judge filter. triggerMsg is the chat msg_id that drove
	// this deliberation, or 0 for proactive turns.
	BeginTurn(turnID, label, model, provider string, triggerMsg int64)

	// OnBridleEvent folds one bridle event into the current turn.
	// Called for every event emitted by the provider during RunTurn
	// (ModelChunk, ToolCallStart, ToolCallResult, StepBoundary,
	// TurnDone, TurnError).
	OnBridleEvent(ev bridle.Event)

	// EndTurn finalises the in-flight turn snapshot.
	EndTurn()
}

// hookSink adapts an ObservabilityHook to bridle.EventSink so the hook
// can be passed as the sink argument to Harness.RunTurn. Each Emit
// forwards to OnBridleEvent.
type hookSink struct{ hook ObservabilityHook }

func (s hookSink) Emit(ev bridle.Event) { s.hook.OnBridleEvent(ev) }

// multiSink fans Emit calls out to multiple bridle.EventSinks. Nil
// entries are silently skipped. Order of Emit calls matches the
// order sinks were passed at construction.
//
// Lives in funnel rather than bridle to avoid a bridle PR cycle for
// a three-line helper; could be promoted to bridle later if other
// callers want it.
type multiSink []bridle.EventSink

func (m multiSink) Emit(ev bridle.Event) {
	for _, s := range m {
		if s != nil {
			s.Emit(ev)
		}
	}
}

// turnSink builds the per-RunTurn sink for a call site. When the hook
// is nil, returns the existing no-op collectSink to preserve the
// pre-Phase-E behavior exactly. When the hook is non-nil, returns a
// fan-out that delivers events to BOTH collectSink (in case a future
// funnel rev grows side-effects) and the hook adapter.
func turnSink(hook ObservabilityHook) bridle.EventSink {
	if hook == nil {
		return collectSink{}
	}
	return multiSink{collectSink{}, hookSink{hook: hook}}
}

// streamingChatSink intercepts ModelChunk events and posts each text
// block to chat immediately via ChatGateway, giving the operator live
// visibility into a turn's progress. Tool-call events pass through
// without side effects.
//
// The first block replies to replyTo (the trigger msg); subsequent
// blocks chain onto the previous block's msg_id so the thread shows
// a linear progression rather than a fan of siblings.
type streamingChatSink struct {
	gateway  ChatGateway
	replyTo  int64
	aspectID string

	mu        sync.Mutex
	lastMsgID int64
}

func (s *streamingChatSink) Emit(ev bridle.Event) {
	chunk, ok := ev.(bridle.ModelChunk)
	if !ok {
		return
	}
	text := strings.TrimSpace(chunk.Text)
	if text == "" {
		return
	}

	s.mu.Lock()
	replyTo := s.replyTo
	if s.lastMsgID != 0 {
		replyTo = s.lastMsgID
	}
	s.mu.Unlock()

	// Use a detached context: chat posts should complete even if the
	// turn's context is cancelled mid-stream.
	msgID, err := s.gateway.SendChat(context.Background(), text, replyTo, "")
	if err != nil {
		return
	}

	s.mu.Lock()
	s.lastMsgID = msgID
	s.mu.Unlock()
}
