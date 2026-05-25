package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
)

// fixedClock returns a deterministic clock that advances by 1ms on
// each call. Pass it to NewGrouperWithClock.
func fixedClock() func() time.Time {
	t := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	tick := time.Millisecond
	return func() time.Time {
		now := t
		t = t.Add(tick)
		return now
	}
}

type capture struct {
	frames []Frame
}

func (c *capture) emit(f Frame) { c.frames = append(c.frames, f) }

func (c *capture) lastOf(kind FrameKind) (Frame, bool) {
	for i := len(c.frames) - 1; i >= 0; i-- {
		if c.frames[i].Kind == kind {
			return c.frames[i], true
		}
	}
	return Frame{}, false
}

func decodeTurn(t *testing.T, f Frame) TurnFrame {
	t.Helper()
	var tf TurnFrame
	if err := json.Unmarshal(f.Payload, &tf); err != nil {
		t.Fatalf("decode turn payload: %v", err)
	}
	return tf
}

func TestGrouperHappyPath(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())

	g.BeginTurn("turn-1", "", "claude-opus-4-7", "claude-api", 189)
	g.OnBridleEvent(bridle.ModelChunk{Text: "thinking "})
	g.OnBridleEvent(bridle.ModelChunk{Text: "about it. "})
	g.OnBridleEvent(bridle.ToolCallStart{
		ID: "t1", Name: "Edit",
		Args: json.RawMessage(`{"file_path":"/a.go","old_string":"x","new_string":"y"}`),
	})
	g.OnBridleEvent(bridle.ToolCallResult{ID: "t1", Result: json.RawMessage(`"ok"`)})
	g.OnBridleEvent(bridle.ModelChunk{Text: "done"})
	g.OnBridleEvent(bridle.TurnDone{Result: bridle.TurnResult{
		Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10, CostUSD: 0.001},
	}})
	g.EndTurn()

	// Verify monotonic seqs.
	for i, f := range c.frames {
		if f.Sequence != int64(i+1) {
			t.Errorf("frame %d seq=%d want %d", i, f.Sequence, i+1)
		}
		if f.Aspect != "plumb" {
			t.Errorf("frame %d aspect=%s", i, f.Aspect)
		}
	}

	// Final frame should be terminal turn snapshot.
	last, ok := c.lastOf(FrameTurn)
	if !ok {
		t.Fatal("no turn frame")
	}
	tf := decodeTurn(t, last)
	if tf.Status != TurnComplete {
		t.Errorf("status=%s want complete", tf.Status)
	}
	if tf.Ended == nil {
		t.Error("Ended is nil")
	}
	if tf.TriggerMsg != 189 {
		t.Errorf("trigger=%d", tf.TriggerMsg)
	}
	if tf.Model != "claude-opus-4-7" || tf.Provider != "claude-api" {
		t.Errorf("model/provider mismatch: %+v", tf)
	}
	if tf.Usage == nil || tf.Usage.InputTokens != 100 || tf.Usage.OutputTokens != 50 {
		t.Errorf("usage: %+v", tf.Usage)
	}

	// Events: text("thinking about it. "), tool_call(Edit, result, artifact), text("done")
	if len(tf.Events) != 3 {
		t.Fatalf("events len=%d want 3: %+v", len(tf.Events), tf.Events)
	}
	if tf.Events[0].Kind != TurnEventText || tf.Events[0].Text != "thinking about it. " {
		t.Errorf("event0: %+v", tf.Events[0])
	}
	if tf.Events[1].Kind != TurnEventToolCall || tf.Events[1].Tool == nil {
		t.Fatalf("event1: %+v", tf.Events[1])
	}
	tc := tf.Events[1].Tool
	if tc.ID != "t1" || tc.Name != "Edit" {
		t.Errorf("tool id/name: %+v", tc)
	}
	if tc.Result == nil || tc.Result.IsError {
		t.Errorf("tool result: %+v", tc.Result)
	}
	if tc.Artifact == nil || tc.Artifact.Kind != ArtifactFileEdit || tc.Artifact.FilePath != "/a.go" {
		t.Errorf("artifact: %+v", tc.Artifact)
	}
	if tf.Events[2].Kind != TurnEventText || tf.Events[2].Text != "done" {
		t.Errorf("event2: %+v", tf.Events[2])
	}
}

func TestGrouperToolPairingByID(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.BeginTurn("turn-x", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ToolCallStart{ID: "a", Name: "Read", Args: json.RawMessage(`{}`)})
	g.OnBridleEvent(bridle.ToolCallStart{ID: "b", Name: "Bash", Args: json.RawMessage(`{}`)})
	g.OnBridleEvent(bridle.ToolCallResult{ID: "b", Result: json.RawMessage(`"b-ok"`)})
	g.OnBridleEvent(bridle.ToolCallResult{ID: "a", Result: json.RawMessage(`"a-ok"`)})
	g.EndTurn()

	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if len(tf.Events) != 2 {
		t.Fatalf("events: %+v", tf.Events)
	}
	for _, ev := range tf.Events {
		if ev.Tool == nil || ev.Tool.Result == nil {
			t.Fatalf("missing result on %+v", ev)
		}
	}
	if tf.Events[0].Tool.ID != "a" || tf.Events[0].Tool.Result.Preview != `"a-ok"` {
		t.Errorf("a: %+v", tf.Events[0].Tool)
	}
	if tf.Events[1].Tool.ID != "b" || tf.Events[1].Tool.Result.Preview != `"b-ok"` {
		t.Errorf("b: %+v", tf.Events[1].Tool)
	}
}

func TestGrouperOrphanToolResult(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ToolCallResult{ID: "ghost", Result: json.RawMessage(`"r"`)})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if len(tf.Events) != 1 || tf.Events[0].Kind != TurnEventOrphanResult {
		t.Fatalf("expected one orphan event, got %+v", tf.Events)
	}
	if tf.Events[0].Tool == nil || tf.Events[0].Tool.ID != "ghost" {
		t.Errorf("orphan tool: %+v", tf.Events[0].Tool)
	}
}

func TestGrouperErroredTurn(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "prv", 0)
	g.OnBridleEvent(bridle.ModelChunk{Text: "partial"})
	g.OnBridleEvent(bridle.TurnError{Err: errors.New("boom"), Stage: "provider"})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if tf.Status != TurnErrored {
		t.Errorf("status=%s want errored", tf.Status)
	}
	if tf.Error != "boom" {
		t.Errorf("error=%q", tf.Error)
	}
}

func TestGrouperToolErrorBuildsErrorResult(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "Bash", Args: json.RawMessage(`{}`)})
	g.OnBridleEvent(bridle.ToolCallResult{ID: "1", Err: "permission denied"})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if tf.Events[0].Tool.Result == nil || !tf.Events[0].Tool.Result.IsError {
		t.Errorf("expected error result: %+v", tf.Events[0].Tool.Result)
	}
	if tf.Events[0].Tool.Result.Preview != "permission denied" {
		t.Errorf("preview=%q", tf.Events[0].Tool.Result.Preview)
	}
}

func TestGrouperChatInboundOutbound(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	t0 := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	g.OnChat(chat.Message{ID: 1, From: "operator", Content: "hi", CreatedAt: t0}, DirectionInbound)
	g.OnChat(chat.Message{ID: 2, From: "plumb", Content: "yo", ReplyTo: 1, CreatedAt: t0}, DirectionOutbound)

	if len(c.frames) != 2 {
		t.Fatalf("frames len=%d", len(c.frames))
	}
	for i, want := range []Direction{DirectionInbound, DirectionOutbound} {
		if c.frames[i].Kind != FrameChat {
			t.Errorf("frame %d kind=%s", i, c.frames[i].Kind)
		}
		if c.frames[i].Sequence != int64(i+1) {
			t.Errorf("frame %d seq=%d", i, c.frames[i].Sequence)
		}
		var cf ChatFrame
		if err := json.Unmarshal(c.frames[i].Payload, &cf); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cf.Direction != want {
			t.Errorf("frame %d direction=%s want %s", i, cf.Direction, want)
		}
	}
}

func TestGrouperPresence(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.OnPresence(true, "registered")
	g.OnPresence(false, "ws_closed")
	if len(c.frames) != 2 {
		t.Fatalf("frames len=%d", len(c.frames))
	}
	for i, f := range c.frames {
		if f.Kind != FramePresence {
			t.Errorf("frame %d kind=%s", i, f.Kind)
		}
		var pf PresenceFrame
		_ = json.Unmarshal(f.Payload, &pf)
		if i == 0 && (!pf.Connected || pf.Reason != "registered") {
			t.Errorf("frame 0: %+v", pf)
		}
		if i == 1 && (pf.Connected || pf.Reason != "ws_closed") {
			t.Errorf("frame 1: %+v", pf)
		}
	}
}

func TestGrouperOnFilterDecision(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.OnFilterDecision("turn-123", "claude-opus", "claude-api", false, "scratch", "scratch")
	if len(c.frames) != 1 {
		t.Fatalf("frames len=%d want 1", len(c.frames))
	}
	f := c.frames[0]
	if f.Kind != FrameFilterDecision {
		t.Errorf("kind=%s want %s", f.Kind, FrameFilterDecision)
	}
	if f.Aspect != "plumb" {
		t.Errorf("aspect=%q want plumb", f.Aspect)
	}
	var fd FilterDecisionFrame
	if err := json.Unmarshal(f.Payload, &fd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fd.MainTurnID != "turn-123" {
		t.Errorf("MainTurnID=%q want turn-123", fd.MainTurnID)
	}
	if fd.Model != "claude-opus" || fd.Provider != "claude-api" {
		t.Errorf("model/provider mismatch: %+v", fd)
	}
	if fd.ShouldPost {
		t.Errorf("ShouldPost=true want false")
	}
	if fd.Reason != "scratch" || fd.Class != "scratch" {
		t.Errorf("reason/class: %+v", fd)
	}
}

func TestGrouperEventWithoutActiveTurnIsNoOp(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.OnBridleEvent(bridle.ModelChunk{Text: "stray"})
	g.OnBridleEvent(bridle.ToolCallStart{ID: "x", Name: "Edit", Args: json.RawMessage(`{}`)})
	g.EndTurn() // also a no-op
	if len(c.frames) != 0 {
		t.Errorf("expected 0 frames, got %d: %+v", len(c.frames), c.frames)
	}
}

func TestGrouperMonotonicSequence(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ModelChunk{Text: "a"})
	g.OnChat(chat.Message{ID: 1, From: "op", Content: "hi", CreatedAt: time.Now()}, DirectionInbound)
	g.OnBridleEvent(bridle.ModelChunk{Text: "b"})
	g.OnPresence(true, "")
	g.EndTurn()
	for i, f := range c.frames {
		if f.Sequence != int64(i+1) {
			t.Errorf("frame %d seq=%d", i, f.Sequence)
		}
	}
}

func TestGrouperInFlightSnapshotsBeforeEnd(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ModelChunk{Text: "x"})
	// Without EndTurn, status of every emitted snapshot must be in_flight.
	if len(c.frames) < 2 {
		t.Fatalf("expected >=2 in-flight frames, got %d", len(c.frames))
	}
	for i, f := range c.frames {
		tf := decodeTurn(t, f)
		if tf.Status != TurnInFlight {
			t.Errorf("frame %d status=%s want in_flight", i, tf.Status)
		}
		if tf.Ended != nil {
			t.Errorf("frame %d Ended=%v want nil pre-EndTurn", i, tf.Ended)
		}
	}
}

func TestGrouperTextPreviewTruncation(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	big := make([]byte, 500)
	for i := range big {
		big[i] = 'a'
	}
	g.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "Bash", Args: json.RawMessage(`{}`)})
	g.OnBridleEvent(bridle.ToolCallResult{ID: "1", Result: json.RawMessage(big)})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	preview := tf.Events[0].Tool.Result.Preview
	// previewMax + 1-byte ellipsis marker; ensure shorter than input.
	if len(preview) >= 500 {
		t.Errorf("preview not truncated: %d bytes", len(preview))
	}
}

// TestGrouperArtifactParseErrorSurfaced feeds malformed JSON to a
// known artifact-bearing tool (Edit) and asserts the parse error is
// surfaced on the ToolCall rather than silently dropped.
func TestGrouperArtifactParseErrorSurfaced(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("t", "", "m", "p", 0)
	// Well-formed JSON envelope (so the snapshot can be re-marshaled)
	// but wrong shape for Edit — file_path is a number, which makes
	// json.Unmarshal into the Edit struct fail.
	g.OnBridleEvent(bridle.ToolCallStart{
		ID: "1", Name: "Edit",
		Args: json.RawMessage(`{"file_path":123,"old_string":"x","new_string":"y"}`),
	})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if len(tf.Events) != 1 || tf.Events[0].Tool == nil {
		t.Fatalf("expected one tool event, got %+v", tf.Events)
	}
	tc := tf.Events[0].Tool
	if tc.Artifact != nil {
		t.Errorf("artifact should be nil on parse error: %+v", tc.Artifact)
	}
	if tc.ArtifactParseErr == "" {
		t.Errorf("ArtifactParseErr should be non-empty")
	}
}

// TestGrouperBeginTurnForcesCloseOfInFlight asserts that calling
// BeginTurn twice without EndTurn between force-closes the first
// turn as errored with the expected message, then the second turn
// starts fresh.
func TestGrouperBeginTurnForcesCloseOfInFlight(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("p", c.emit, fixedClock())
	g.BeginTurn("turn-1", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ModelChunk{Text: "partial"})
	// Second BeginTurn — should force-close turn-1.
	g.BeginTurn("turn-2", "", "m", "p", 0)

	// Find the last frame for turn-1 and assert it's errored.
	var lastT1 *TurnFrame
	var firstT2 *TurnFrame
	for _, f := range c.frames {
		if f.Kind != FrameTurn {
			continue
		}
		tf := decodeTurn(t, f)
		switch tf.TurnID {
		case "turn-1":
			x := tf
			lastT1 = &x
		case "turn-2":
			if firstT2 == nil {
				x := tf
				firstT2 = &x
			}
		}
	}
	if lastT1 == nil {
		t.Fatal("no turn-1 frame found")
	}
	if lastT1.Status != TurnErrored {
		t.Errorf("turn-1 final status=%s want errored", lastT1.Status)
	}
	if lastT1.Error != "interrupted by new turn" {
		t.Errorf("turn-1 error=%q want %q", lastT1.Error, "interrupted by new turn")
	}
	if lastT1.Ended == nil {
		t.Error("turn-1 Ended is nil")
	}
	if firstT2 == nil {
		t.Fatal("no turn-2 frame found")
	}
	if firstT2.Status != TurnInFlight {
		t.Errorf("turn-2 initial status=%s want in_flight", firstT2.Status)
	}
	if len(firstT2.Events) != 0 {
		t.Errorf("turn-2 should start with no events, got %+v", firstT2.Events)
	}
	if firstT2.Error != "" {
		t.Errorf("turn-2 should start with no error, got %q", firstT2.Error)
	}
}

// TestGrouper_OnChat_Concurrent fires N OnChat calls from N goroutines
// and asserts every emitted ChatFrame has a unique sequence number and
// that the set of sequences is exactly {1..N}. Run under -race to
// catch any reintroduction of unsynchronised access to g.seq / g.emit.
func TestGrouper_OnChat_Concurrent(t *testing.T) {
	const N = 100

	var mu sync.Mutex
	seqs := make([]int64, 0, N)
	emit := func(f Frame) {
		// Capture under our own mutex — emit is called while the
		// Grouper's mutex is held, so this is contention-free, but
		// the slice is not the Grouper's so we own its sync.
		mu.Lock()
		seqs = append(seqs, f.Sequence)
		mu.Unlock()
	}
	g := NewGrouper("plumb", emit)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			g.OnChat(chat.Message{
				ID:        int64(i + 1),
				From:      "operator",
				Content:   "hi",
				CreatedAt: time.Now(),
			}, DirectionInbound)
		}()
	}
	wg.Wait()

	if len(seqs) != N {
		t.Fatalf("got %d emissions, want %d", len(seqs), N)
	}
	seen := make(map[int64]bool, N)
	var maxSeq int64
	for _, s := range seqs {
		if seen[s] {
			t.Errorf("duplicate sequence %d", s)
		}
		seen[s] = true
		if s > maxSeq {
			maxSeq = s
		}
		if s < 1 || s > N {
			t.Errorf("sequence %d out of range [1,%d]", s, N)
		}
	}
	if maxSeq != N {
		t.Errorf("max sequence = %d, want %d", maxSeq, N)
	}
}

// TestGrouperLabelRouting asserts BeginTurn's label argument lands on
// the emitted TurnFrame.Label field, with empty defaulting to "main".
// Sub-turn labels ("compact" / "filter-judge") let renderers
// distinguish bridle-driven turns that nest inside one Deliberate
// call. Per chat #213 with keel-cli — different bridle turns within
// one deliberation cycle are different events, not aggregation levels
// of the same event; renderers must be able to tell them apart.
func TestGrouperLabelRouting(t *testing.T) {
	cases := []struct {
		give string
		want string
	}{
		{"", "main"},
		{"main", "main"},
		{"compact", "compact"},
		{"filter-judge", "filter-judge"},
		{"some-future-label", "some-future-label"},
	}
	for _, tc := range cases {
		t.Run(tc.give+"_to_"+tc.want, func(t *testing.T) {
			c := &capture{}
			g := NewGrouperWithClock("p", c.emit, fixedClock())
			g.BeginTurn("t", tc.give, "m", "p", 0)
			g.EndTurn()
			// Find the last turn frame emitted (post-EndTurn).
			var last *TurnFrame
			for _, f := range c.frames {
				if f.Kind != FrameTurn {
					continue
				}
				x := decodeTurn(t, f)
				last = &x
			}
			if last == nil {
				t.Fatal("no turn frame found")
			}
			if last.Label != tc.want {
				t.Errorf("Label = %q, want %q", last.Label, tc.want)
			}
		})
	}
}

// NEX-100: regression coverage for tool_call serialization. The
// reported "stripped tool_call data" symptom was likely from a fixed
// pre-rewrite era — current code preserves Input round-trip. Locking
// it in so a future refactor catches if Input vanishes from the
// marshalled payload.

func TestGrouperToolCallInputRoundTrip(t *testing.T) {
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.BeginTurn("turn-1", "", "m", "p", 0)
	args := `{"file_path":"/etc/hosts","limit":50,"offset":0}`
	g.OnBridleEvent(bridle.ToolCallStart{ID: "t1", Name: "Read", Args: json.RawMessage(args)})
	g.OnBridleEvent(bridle.TurnDone{})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if len(tf.Events) != 1 || tf.Events[0].Tool == nil {
		t.Fatalf("events: %+v", tf.Events)
	}
	tc := tf.Events[0].Tool
	if tc.Name != "Read" {
		t.Errorf("Name lost: got %q", tc.Name)
	}
	if string(tc.Input) != args {
		t.Errorf("Input lost or mangled\n got:  %s\n want: %s", string(tc.Input), args)
	}
}

func TestGrouperToolCallNilArgsNormalizedToEmptyObject(t *testing.T) {
	// NEX-100: some providers (bedrock) emit ToolCallStart without Args.
	// Pre-fix the activity log carried "input":null which read as "data
	// stripped". Post-fix it carries "input":{} (truthful empty-args).
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.BeginTurn("turn-1", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ToolCallStart{ID: "t1", Name: "Bash"}) // no Args
	g.OnBridleEvent(bridle.TurnDone{})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	if !bytes.Contains(last.Payload, []byte(`"input":{}`)) {
		t.Errorf("expected \"input\":{} in payload, got %s", string(last.Payload))
	}
	if bytes.Contains(last.Payload, []byte(`"input":null`)) {
		t.Errorf("unexpected \"input\":null in payload — normalization failed: %s", string(last.Payload))
	}
}

func TestGrouperOrphanResultPlaceholderLockedBehavior(t *testing.T) {
	// NEX-100: orphan results (ToolCallResult without matching Start)
	// surface as a placeholder with empty Name + Input=null. This is
	// CORRECT — there's no prior data to populate from. Locking the
	// shape so future refactors don't accidentally hide orphans.
	c := &capture{}
	g := NewGrouperWithClock("plumb", c.emit, fixedClock())
	g.BeginTurn("turn-1", "", "m", "p", 0)
	g.OnBridleEvent(bridle.ToolCallResult{ID: "ghost", Result: json.RawMessage(`"orphaned"`)})
	g.OnBridleEvent(bridle.TurnDone{})
	g.EndTurn()
	last, _ := c.lastOf(FrameTurn)
	tf := decodeTurn(t, last)
	if len(tf.Events) != 1 || tf.Events[0].Kind != TurnEventOrphanResult {
		t.Fatalf("orphan event missing: %+v", tf.Events)
	}
	tc := tf.Events[0].Tool
	// After JSON round-trip through emitTurnSnapshot, nil json.RawMessage
	// stored on the orphan becomes the literal bytes "null" when
	// unmarshalled back. Check the raw bytes rather than nil-ness.
	if tc.ID != "ghost" || tc.Name != "" || string(tc.Input) != "null" {
		t.Errorf("orphan shape changed: %+v (Input=%s)", tc, string(tc.Input))
	}
	if tc.Result == nil || tc.Result.Preview != `"orphaned"` {
		t.Errorf("orphan result lost: %+v", tc.Result)
	}
}
