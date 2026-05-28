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

// FilterDecisionRenderer is an optional sub-interface of
// ObservabilityHook for sinks that want to receive post-hoc filter
// verdicts as structured data rather than fabricated bridle-turn
// events. Funnel type-asserts at runtime; hooks that implement this
// get the clean call, hooks that don't fall back to the synthetic
// BeginTurn → ModelChunk → TurnDone → EndTurn dance preserved for
// backward compatibility.
//
// New hooks should implement this directly. Existing hooks can adopt
// it without API breakage — adding the method is purely additive,
// because the funnel uses a type assertion, not a required method.
//
// Signature uses primitives (not funnel.FilterDecision) so observability
// renderers can implement it without importing funnel — keeps the
// layering one-way (renderers don't depend on funnel).
type FilterDecisionRenderer interface {
	// OnFilterDecision is called once per Deliberate after the post-
	// hoc filter judge returns. mainTurnID identifies the parent
	// deliberation turn so renderers can pair the verdict against it;
	// model + provider mirror what was passed to BeginTurn for the
	// main turn. shouldPost / reason / class mirror the FilterDecision
	// fields the funnel chose.
	OnFilterDecision(mainTurnID, model, provider string, shouldPost bool, reason, class string)
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

// streamingChatSink intercepts ModelChunk events and posts text
// blocks to chat via ChatGateway, giving the operator live visibility
// into a turn's progress.
//
// Chunk-coalescing policy: providers emit ModelChunks at wildly
// different granularities. claudecode/geminicli emit one chunk per
// semantic text block (sentence-to-paragraph sized). openai-shape
// providers (OpenAI, DeepSeek, Together, vLLM) emit one chunk per
// token — emitting each as its own chat row fans a multi-sentence
// reply into ~40-80 individual messages. The fix: buffer ModelChunk
// text between natural transition events (ToolCallStart, TurnDone,
// TurnError) and flush as one post per logical span. claudecode's
// pattern of "text block → tool call → text block" still produces
// one row per block (each tool call flushes the preceding buffer).
// openai's pure-text turn produces one row total (TurnDone flushes).
//
// The first emitted post replies to replyTo (the trigger msg);
// subsequent posts chain onto the previous post's msg_id so the
// thread shows a linear progression rather than a fan of siblings.
type streamingChatSink struct {
	gateway  ChatGateway
	replyTo  int64
	aspectID string

	mu        sync.Mutex
	buf       strings.Builder
	lastMsgID int64
}

func (s *streamingChatSink) Emit(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ModelChunk:
		if e.Text == "" {
			return
		}
		s.mu.Lock()
		s.buf.WriteString(e.Text)
		s.mu.Unlock()
	case bridle.ToolCallStart, bridle.TurnDone, bridle.TurnError:
		_ = e
		s.mu.Lock()
		s.flushLocked()
		s.mu.Unlock()
	}
}

// flushLocked posts the accumulated buffer as one chat row and
// clears the buffer. Caller holds s.mu. No-op on empty buffer.
//
// Uses a detached context: chat posts should complete even if the
// turn's context is cancelled mid-stream.
func (s *streamingChatSink) flushLocked() {
	text := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	if text == "" {
		return
	}
	replyTo := s.replyTo
	if s.lastMsgID != 0 {
		replyTo = s.lastMsgID
	}
	msgID, err := s.gateway.SendChat(context.Background(), text, replyTo, "")
	if err != nil {
		return
	}
	s.lastMsgID = msgID
}
