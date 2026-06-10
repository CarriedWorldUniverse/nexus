package broker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/runtime/obsforward"
)

// TestDecodeInboundBridleEvent verifies the inbound decoder is the
// exact inverse of obsforward.encodeBridleEvent. A drift between the
// two splits the wire protocol and would silently drop events.
func TestDecodeInboundBridleEvent(t *testing.T) {
	cases := []struct {
		name string
		kind string
		body string
		want bridle.Event
	}{
		{
			name: "ModelChunk",
			kind: obsforward.EventKindModelChunk,
			body: `{"Text":"hello"}`,
			want: bridle.ModelChunk{Text: "hello"},
		},
		{
			name: "ToolCallStart",
			kind: obsforward.EventKindToolCallStart,
			body: `{"ID":"t1","Name":"Read","Args":{"file_path":"a.go"}}`,
			want: bridle.ToolCallStart{
				ID:   "t1",
				Name: "Read",
				Args: json.RawMessage(`{"file_path":"a.go"}`),
			},
		},
		{
			name: "ToolCallResult",
			kind: obsforward.EventKindToolCallResult,
			body: `{"ID":"t1","Result":"ok"}`,
			want: bridle.ToolCallResult{ID: "t1", Result: json.RawMessage(`"ok"`)},
		},
		{
			name: "StepBoundary",
			kind: obsforward.EventKindStepBoundary,
			body: `{"Step":3}`,
			want: bridle.StepBoundary{Step: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeInboundBridleEvent(tc.kind, json.RawMessage(tc.body))
			if !ok {
				t.Fatalf("decode failed for %s", tc.kind)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("decoded=%s want=%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestDecodeInboundBridleEvent_TurnErrorRoundtrip(t *testing.T) {
	body := `{"err":"provider boom","stage":"provider"}`
	got, ok := decodeInboundBridleEvent(obsforward.EventKindTurnError, json.RawMessage(body))
	if !ok {
		t.Fatal("decode failed")
	}
	te, ok := got.(bridle.TurnError)
	if !ok {
		t.Fatalf("got %T want bridle.TurnError", got)
	}
	if te.Stage != "provider" {
		t.Errorf("Stage=%q want provider", te.Stage)
	}
	if te.Err == nil || te.Err.Error() != "provider boom" {
		t.Errorf("Err=%v want 'provider boom'", te.Err)
	}
}

func TestDecodeInboundBridleEvent_UnknownKindReturnsFalse(t *testing.T) {
	_, ok := decodeInboundBridleEvent("future.bridge_event", json.RawMessage(`{}`))
	if ok {
		t.Error("unknown kind decoded as ok=true; should drop")
	}
}

func TestDecodeInboundBridleEvent_MalformedJSONReturnsFalse(t *testing.T) {
	_, ok := decodeInboundBridleEvent(obsforward.EventKindModelChunk, json.RawMessage(`{not-json`))
	if ok {
		t.Error("malformed JSON decoded ok=true; should drop")
	}
}

// TestTurnTimingWireRoundtrip is the end-to-end regression that pins bridle
// TurnTiming across the obsforward wire seam. The path under test is:
//
//	obsforward.WSForwarder.OnBridleEvent (encodeBridleEvent → json.Marshal)
//	→ broker decodeInboundBridleEvent (json.Unmarshal)
//	→ observability.Grouper.OnBridleEvent → TurnFrame.Timing non-nil
//
// A future refactor that narrows either side's struct (drops a field, changes
// a JSON tag) must break this test before it can silently lose timing data.
func TestTurnTimingWireRoundtrip(t *testing.T) {
	original := bridle.TurnDone{
		Result: bridle.TurnResult{
			Timing: bridle.TurnTiming{
				TotalSecs: 6.76,
				Rounds: []bridle.RoundTiming{{
					AssemblySecs:            0.1,
					StartupToFirstEventSecs: 1.2,
					StreamSecs:              2.3,
					PromptBytes:             42,
					MessageCount:            3,
					ToolDefCount:            1,
				}},
				Tools: []bridle.ToolTiming{{
					ID:   "t1",
					Name: "Bash",
					Secs: 0.5,
				}},
			},
		},
	}

	// --- Encode side (obsforward) ---
	// Capture the ObserveEventPayload that WSForwarder sends.
	var captured frames.ObserveEventPayload
	capturer := obsforward.SenderFunc(func(_ context.Context, env frames.Envelope) error {
		if env.Kind == frames.KindObserveEvent {
			return json.Unmarshal(env.Payload, &captured)
		}
		return nil
	})
	fwd := obsforward.New(capturer, "plumb", slog.New(slog.NewTextHandler(io.Discard, nil)))
	fwd.OnBridleEvent(original)

	if captured.EventKind != obsforward.EventKindTurnDone {
		t.Fatalf("EventKind=%q want %q", captured.EventKind, obsforward.EventKindTurnDone)
	}

	// --- Decode side (broker) ---
	decoded, ok := decodeInboundBridleEvent(captured.EventKind, captured.Event)
	if !ok {
		t.Fatal("decodeInboundBridleEvent returned ok=false")
	}
	td, ok := decoded.(bridle.TurnDone)
	if !ok {
		t.Fatalf("decoded type=%T want bridle.TurnDone", decoded)
	}

	// Deep-equal the Timing structs via JSON round-trip so the assertion is
	// field-complete: any dropped field shows up as a JSON diff.
	wantJSON, _ := json.Marshal(original.Result.Timing)
	gotJSON, _ := json.Marshal(td.Result.Timing)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("Timing mismatch after wire round-trip:\n  got  %s\n  want %s", gotJSON, wantJSON)
	}

	// Spot-check the load-bearing scalar so a refactor that swaps the JSON
	// tag name produces a clear failure message.
	if td.Result.Timing.TotalSecs != 6.76 {
		t.Errorf("TotalSecs=%v want 6.76", td.Result.Timing.TotalSecs)
	}

	// --- Grouper side (optional: timingFromBridle path) ---
	// Feed the decoded event through a real Grouper and verify TurnFrame.Timing
	// is non-nil with the expected total_secs.
	var lastFrame observability.TurnFrame
	g := observability.NewGrouper("plumb", func(f observability.Frame) {
		if f.Kind == observability.FrameTurn {
			_ = json.Unmarshal(f.Payload, &lastFrame)
		}
	})
	g.BeginTurn("t1", "", "m", "p", 0)
	g.OnBridleEvent(decoded)
	g.EndTurn()

	if lastFrame.Timing == nil {
		t.Fatal("TurnFrame.Timing is nil after Grouper.OnBridleEvent(TurnDone with Timing)")
	}
	if lastFrame.Timing.TotalSecs != 6.76 {
		t.Errorf("TurnFrame.Timing.TotalSecs=%v want 6.76", lastFrame.Timing.TotalSecs)
	}
}
