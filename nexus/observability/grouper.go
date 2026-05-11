package observability

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
)

// previewMax is the maximum length (in bytes) of a tool result before
// truncation. Renderers can request the full body via the optional
// ToolResult.Full field; Phase A never populates it.
//
// TODO(observability): rune-aware truncation when renderers display
// tool previews — current byte-length cut can split a multibyte rune.
const previewMax = 200

// Grouper consumes bridle events plus chat/presence/turn-boundary
// calls from the funnel and emits Frame snapshots. One Grouper per
// aspect; the Sequence field of every emitted Frame is monotonic
// within that Grouper instance.
//
// All exported methods are safe to call from multiple goroutines.
// Emit callbacks run while the per-Grouper mutex is held; callbacks
// MUST NOT re-enter the same Grouper (would deadlock). The Hub's
// emit path (buffer.Append + onFrame broadcast → broadcastObserveFrame
// → wsConn.send) does not re-enter, so the canonical wiring is safe.
type Grouper struct {
	// mu guards every mutable field below. Held across emit callbacks
	// — see type-level doc on the no-reentry contract.
	mu     sync.Mutex
	aspect string
	emit   func(Frame)
	clock  func() time.Time
	seq    int64

	// in-flight turn state — nil between turns.
	turn       *TurnFrame
	turnStart  time.Time
	turnErrSet bool // sticky: any TurnError in this turn → status=errored
}

// NewGrouper constructs a Grouper bound to an aspect identifier with
// the wall-clock as its time source. emit is called synchronously for
// every produced Frame; the caller is responsible for any fanout
// (broker write, buffer append, etc).
func NewGrouper(aspect string, emit func(Frame)) *Grouper {
	return NewGrouperWithClock(aspect, emit, time.Now)
}

// NewGrouperWithClock constructs a Grouper with a caller-supplied
// clock. Tests pass a deterministic time source; production code
// should use NewGrouper.
func NewGrouperWithClock(aspect string, emit func(Frame), clock func() time.Time) *Grouper {
	return &Grouper{aspect: aspect, emit: emit, clock: clock}
}

// BeginTurn opens a new in-flight turn and emits the initial
// TurnFrame snapshot. trigger may be 0 if the turn was not driven
// by a specific chat message (e.g. proactive deliberation).
func (g *Grouper) BeginTurn(turnID, model, provider string, trigger int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.clock()
	// Defensive: if a previous turn is still in flight, treat as a
	// protocol violation (missing EndTurn) but recover by force-closing
	// it as errored so the renderer sees a terminal snapshot before
	// the new turn begins.
	if g.turn != nil {
		g.turn.Status = TurnErrored
		if g.turn.Error == "" {
			g.turn.Error = "interrupted by new turn"
		} else {
			g.turn.Error = g.turn.Error + "; interrupted by new turn"
		}
		end := t
		g.turn.Ended = &end
		if g.turn.Usage != nil {
			g.turn.Usage.Duration = end.Sub(g.turnStart)
		}
		g.emitTurnSnapshot()
		g.turn = nil
	}
	g.turn = &TurnFrame{
		TurnID:     turnID,
		Status:     TurnInFlight,
		Started:    t,
		TriggerMsg: trigger,
		Model:      model,
		Provider:   provider,
		Events:     []TurnEvent{},
	}
	g.turnStart = t
	g.turnErrSet = false
	g.emitTurnSnapshot()
}

// OnBridleEvent folds a single bridle event into the in-flight
// turn. If no turn is in flight, the event is silently dropped —
// production wiring always BeginTurns first, but the defensive
// path matters because tests exercise it and provider failures
// can race the boundary calls.
func (g *Grouper) OnBridleEvent(ev bridle.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn == nil {
		return
	}
	switch e := ev.(type) {
	case bridle.ModelChunk:
		g.appendText(e.Text)
	case bridle.ToolCallStart:
		g.startToolCall(e)
	case bridle.ToolCallResult:
		g.completeToolCall(e)
	case bridle.StepBoundary:
		g.turn.Events = append(g.turn.Events, TurnEvent{Kind: TurnEventStep, Step: e.Step})
	case bridle.TurnDone:
		g.turn.Usage = usageFromBridle(e.Result.Usage, g.clock().Sub(g.turnStart))
	case bridle.TurnError:
		g.turnErrSet = true
		if e.Err != nil {
			if g.turn.Error == "" {
				g.turn.Error = e.Err.Error()
			} else {
				g.turn.Error = g.turn.Error + "; " + e.Err.Error()
			}
		}
	default:
		// Unknown bridle event type — ignore; new event types
		// shouldn't crash an older Grouper.
		return
	}
	g.emitTurnSnapshot()
}

// EndTurn finalises the in-flight turn and emits the terminal
// TurnFrame. No-op if no turn is in flight.
func (g *Grouper) EndTurn() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn == nil {
		return
	}
	end := g.clock()
	g.turn.Ended = &end
	if g.turnErrSet {
		g.turn.Status = TurnErrored
	} else {
		g.turn.Status = TurnComplete
	}
	if g.turn.Usage != nil {
		// Refresh Duration with the boundary-call end time so it
		// reflects the funnel's view, not bridle's internal one.
		g.turn.Usage.Duration = end.Sub(g.turnStart)
	}
	g.emitTurnSnapshot()
	g.turn = nil
}

// OnChat emits a ChatFrame independent of turn state.
func (g *Grouper) OnChat(msg chat.Message, direction Direction) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cf := ChatFrame{
		MsgID:     msg.ID,
		From:      msg.From,
		Content:   msg.Content,
		ReplyTo:   msg.ReplyTo,
		Topic:     msg.Topic,
		Direction: direction,
		CreatedAt: msg.CreatedAt,
	}
	payload, _ := json.Marshal(cf)
	g.emitFrame(Frame{
		Kind:    FrameChat,
		Aspect:  g.aspect,
		TS:      msg.CreatedAt,
		Payload: payload,
	})
}

// OnPresence emits a PresenceFrame for the WS connection-state flip.
func (g *Grouper) OnPresence(connected bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pf := PresenceFrame{Connected: connected, Reason: reason}
	payload, _ := json.Marshal(pf)
	g.emitFrame(Frame{
		Kind:    FramePresence,
		Aspect:  g.aspect,
		TS:      g.clock(),
		Payload: payload,
	})
}

// --- internals ---

func (g *Grouper) appendText(text string) {
	if text == "" {
		return
	}
	// If the most recent event is a text event, extend it; otherwise
	// start a new text segment. A tool_call (or any non-text) breaks
	// the streak so the next ModelChunk starts a fresh segment.
	if n := len(g.turn.Events); n > 0 && g.turn.Events[n-1].Kind == TurnEventText {
		g.turn.Events[n-1].Text += text
		return
	}
	g.turn.Events = append(g.turn.Events, TurnEvent{Kind: TurnEventText, Text: text})
}

func (g *Grouper) startToolCall(e bridle.ToolCallStart) {
	tc := &ToolCall{
		ID:    e.ID,
		Name:  e.Name,
		Input: e.Args,
	}
	if art, err := ParseArtifact(e.Name, e.Args); err != nil {
		tc.ArtifactParseErr = err.Error()
	} else if art != nil {
		tc.Artifact = art
	}
	g.turn.Events = append(g.turn.Events, TurnEvent{Kind: TurnEventToolCall, Tool: tc})
}

func (g *Grouper) completeToolCall(e bridle.ToolCallResult) {
	// Walk events in reverse for the most recent matching unresolved
	// tool call. This handles interleaved calls and ensures we attach
	// the result to the right invocation when names repeat.
	for i := len(g.turn.Events) - 1; i >= 0; i-- {
		ev := g.turn.Events[i]
		if ev.Kind == TurnEventToolCall && ev.Tool != nil && ev.Tool.ID == e.ID && ev.Tool.Result == nil {
			ev.Tool.Result = buildToolResult(e)
			g.turn.Events[i] = ev
			return
		}
	}
	// No matching pending tool call — orphan. Surface a defensive
	// placeholder so renderers can show the result rather than swallow it.
	orphan := &ToolCall{
		ID:     e.ID,
		Name:   "",
		Input:  nil,
		Result: buildToolResult(e),
	}
	g.turn.Events = append(g.turn.Events, TurnEvent{Kind: TurnEventOrphanResult, Tool: orphan})
}

func buildToolResult(e bridle.ToolCallResult) *ToolResult {
	if e.Err != "" {
		return &ToolResult{Preview: truncate(e.Err), IsError: true}
	}
	return &ToolResult{Preview: truncate(string(e.Result)), IsError: false}
}

// truncate clips s to previewMax. The first newline (if earlier)
// is treated as an end-of-preview marker; renderers can offer
// "expand" to reveal the rest.
func truncate(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < previewMax {
		return s[:i] + "…"
	}
	if len(s) <= previewMax {
		return s
	}
	return s[:previewMax] + "…"
}

func usageFromBridle(u bridle.Usage, d time.Duration) *UsageStats {
	return &UsageStats{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		Duration:                 d,
		CostUSD:                  u.CostUSD,
	}
}

// emitTurnSnapshot deep-copies the current TurnFrame and emits it.
// Events, ToolCall, ToolResult, and Artifact (including its Edits
// slice) are all copied so a Phase B broker can safely queue or
// retain references to Frames without observing later mutations
// (e.g. a ToolCallResult landing after the snapshot was queued).
// ToolCall.Input is json.RawMessage and treated as immutable
// post-creation, so the underlying byte slice is shared.
func (g *Grouper) emitTurnSnapshot() {
	tf := *g.turn
	events := make([]TurnEvent, len(g.turn.Events))
	for i, ev := range g.turn.Events {
		events[i] = ev
		if ev.Tool != nil {
			tc := *ev.Tool
			if ev.Tool.Result != nil {
				r := *ev.Tool.Result
				tc.Result = &r
			}
			if ev.Tool.Artifact != nil {
				a := *ev.Tool.Artifact
				if len(ev.Tool.Artifact.Edits) > 0 {
					a.Edits = append([]EditPair(nil), ev.Tool.Artifact.Edits...)
				}
				tc.Artifact = &a
			}
			events[i].Tool = &tc
		}
	}
	tf.Events = events
	payload, _ := json.Marshal(tf)
	g.emitFrame(Frame{
		Kind:    FrameTurn,
		Aspect:  g.aspect,
		TS:      g.clock(),
		Payload: payload,
	})
}

func (g *Grouper) emitFrame(f Frame) {
	g.seq++
	f.Sequence = g.seq
	g.emit(f)
}
