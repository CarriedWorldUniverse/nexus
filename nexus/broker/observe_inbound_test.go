package broker

import (
	"encoding/json"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
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
