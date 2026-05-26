package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// NEX-246 Slice 2: turn-frame summary line includes the signal the
// operator cares about — status, model, duration, token usage — and
// defaults label to "main" when empty (back-compat per types.go:50).
func TestSummariseTurnFrame_HappyAndDefaults(t *testing.T) {
	end := time.Date(2026, 5, 26, 10, 0, 5, 0, time.UTC)
	tf := observability.TurnFrame{
		Status:   observability.TurnComplete,
		Started:  time.Date(2026, 5, 26, 10, 0, 2, 0, time.UTC),
		Ended:    &end,
		Model:    "deepseek-chat",
		Provider: "openai",
		Usage:    &observability.UsageStats{InputTokens: 1200, OutputTokens: 400},
	}
	got := summariseTurnFrame(tf)
	for _, want := range []string{
		"label=main", "status=complete", "model=deepseek-chat",
		"duration=3s", "usage_in=1200", "usage_out=400",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summariseTurnFrame missing %q in %q", want, got)
		}
	}
}

// NEX-246 Slice 2: errored turns include the error text (truncated)
// so the operator sees what failed — failures are the headline of
// any activity window.
func TestSummariseTurnFrame_ErrorTruncated(t *testing.T) {
	tf := observability.TurnFrame{
		Label:  "filter-judge",
		Status: observability.TurnErrored,
		Error:  strings.Repeat("x", 500),
	}
	got := summariseTurnFrame(tf)
	if !strings.Contains(got, "label=filter-judge") {
		t.Errorf("missing label override: %q", got)
	}
	if !strings.Contains(got, "status=errored") {
		t.Errorf("missing errored status: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncated error in %q", got)
	}
}

// NEX-246 Slice 2: chat-frame summary keeps the from + a previewed
// content; direction arrow ← inbound, → outbound. Newlines flatten.
func TestSummariseChatFrame_DirectionAndPreview(t *testing.T) {
	out := summariseChatFrame(observability.ChatFrame{
		From: "harrow", Content: "NEX-247 plan ready\nfollow-up later",
		Direction: observability.DirectionOutbound,
	})
	if !strings.Contains(out, "from=harrow") {
		t.Errorf("missing from: %q", out)
	}
	if !strings.Contains(out, "→") {
		t.Errorf("expected outbound arrow: %q", out)
	}
	if !strings.Contains(out, "NEX-247 plan ready") {
		t.Errorf("missing preview body: %q", out)
	}

	inbound := summariseChatFrame(observability.ChatFrame{
		From: "anvil", Content: "ack", Direction: observability.DirectionInbound,
	})
	if !strings.Contains(inbound, "←") {
		t.Errorf("expected inbound arrow: %q", inbound)
	}
}

// NEX-246 Slice 2: filter-decision frame shows post/drop verdict +
// class + reason — drives operator intuition about how strict the
// post-turn filter has been over the window.
func TestSummariseFrame_FilterDecision(t *testing.T) {
	payload, _ := json.Marshal(observability.FilterDecisionFrame{
		MainTurnID: "t1", ShouldPost: false, Class: "noise", Reason: "no question",
	})
	out := summariseFrame(observability.Frame{
		Kind: observability.FrameFilterDecision, Payload: payload,
	})
	for _, want := range []string{"filter: drop", "class=noise", "reason=no question"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

// NEX-246 Slice 2: presence frame renders connection state + reason;
// empty reason gets a placeholder so the operator-facing output
// doesn't read "presence: connected ()".
func TestSummariseFrame_Presence(t *testing.T) {
	connectedPayload, _ := json.Marshal(observability.PresenceFrame{Connected: true, Reason: "registered"})
	out := summariseFrame(observability.Frame{Kind: observability.FramePresence, Payload: connectedPayload})
	if !strings.Contains(out, "connected") || !strings.Contains(out, "registered") {
		t.Errorf("connected/registered missing: %q", out)
	}

	disconnectedPayload, _ := json.Marshal(observability.PresenceFrame{Connected: false})
	out2 := summariseFrame(observability.Frame{Kind: observability.FramePresence, Payload: disconnectedPayload})
	if !strings.Contains(out2, "disconnected") || !strings.Contains(out2, "(no reason)") {
		t.Errorf("disconnected/placeholder missing: %q", out2)
	}
}

// NEX-246 Slice 2: unknown FrameKind returns "" so framesToActivity
// Events drops it instead of pushing a meaningless event into the
// prompt. Defensive against future kind additions.
func TestSummariseFrame_UnknownKindEmpty(t *testing.T) {
	if got := summariseFrame(observability.Frame{Kind: "unknown_future_kind"}); got != "" {
		t.Errorf("expected empty for unknown kind; got %q", got)
	}
}

// NEX-246 Slice 2: framesToActivityEvents respects the cutoff — the
// broker's retained tail can hold frames older than --since.
// Conversion drops them so the digest reflects the requested window.
func TestFramesToActivityEvents_CutoffAndSkipsUnknown(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	chatPayload, _ := json.Marshal(observability.ChatFrame{From: "anvil", Content: "ok"})

	in := []observability.Frame{
		// Older than cutoff — dropped
		{Kind: observability.FrameChat, Aspect: "anvil", TS: now.Add(-2 * time.Hour), Payload: chatPayload},
		// Inside window — kept
		{Kind: observability.FrameChat, Aspect: "anvil", TS: now.Add(-30 * time.Minute), Payload: chatPayload},
		// Inside window but unknown kind — dropped (empty summary)
		{Kind: "future_kind", Aspect: "harrow", TS: now.Add(-15 * time.Minute), Payload: chatPayload},
	}
	cutoff := now.Add(-1 * time.Hour)
	out := framesToActivityEvents(in, cutoff)
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (cutoff+unknown filtered)", len(out))
	}
	if out[0].Aspect != "anvil" || out[0].Kind != "chat" {
		t.Errorf("kept wrong event: %+v", out[0])
	}
}

// NEX-246 Slice 2: digest output puts markdown blurb first, then
// quantitative counts — counts always render even if the markdown is
// the placeholder (fail-open path from Slice 1).
func TestRenderActivityDigest_MarkdownThenCounts(t *testing.T) {
	out := classification.ActivitySummaryOutput{
		Markdown: "anvil ran 3 turns; one filter-judge dropped a low-signal main.",
		Counts: classification.ActivitySummaryCounts{
			Total:    5,
			ByKind:   map[string]int{"turn": 3, "chat": 2},
			ByAspect: map[string]int{"anvil": 5},
		},
	}
	rendered := renderActivityDigest(out, 1*time.Hour, []string{"anvil"})

	mdAt := strings.Index(rendered, "anvil ran 3 turns")
	countsAt := strings.Index(rendered, "## Counts")
	if mdAt < 0 || countsAt < 0 || mdAt > countsAt {
		t.Errorf("markdown should appear before Counts:\n%s", rendered)
	}
	for _, want := range []string{
		"Activity summary (last 1h0m0s",
		"aspects: anvil",
		"Total: 5",
		"turn=3 chat=2",
		"anvil=5",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered missing %q\n---\n%s", want, rendered)
		}
	}
}

// NEX-246 Slice 2: empty maps render "(none)" — empty windows should
// still produce intelligible output (paired with the subcommand's
// "(no activity in window)" early-exit when there are zero events).
func TestJoinKVMap_EmptyShowsNone(t *testing.T) {
	if got := joinKVMap(map[string]int{}); got != "(none)" {
		t.Errorf("joinKVMap empty = %q, want (none)", got)
	}
	got := joinKVMap(map[string]int{"a": 2, "b": 5, "c": 5})
	// b and c both 5: sorted desc-count then asc-name → b, c, then a
	if got != "b=5 c=5 a=2" {
		t.Errorf("joinKVMap ordering: got %q", got)
	}
}

// NEX-246 Slice 2: URL normaliser preserves /connect suffix, rewrites
// http(s)→ws(s) scheme. Mirrors the nexus-watch contract operators
// already rely on.
func TestToActivitySummaryWSURL(t *testing.T) {
	cases := map[string]string{
		"https://broker.tail:7888":         "wss://broker.tail:7888/connect",
		"http://localhost:7888":            "ws://localhost:7888/connect",
		"wss://broker:7888/connect":        "wss://broker:7888/connect",
		"wss://broker:7888/connect/":       "wss://broker:7888/connect",
	}
	for in, want := range cases {
		if got := toActivitySummaryWSURL(in); got != want {
			t.Errorf("toActivitySummaryWSURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// NEX-246 Slice 2: auth requires JWT and is mutually exclusive
// between --operator-token and --operator-token-file. Guards against
// operator passing both by accident and one silently winning.
func TestResolveActivitySummaryAuth(t *testing.T) {
	if _, err := resolveActivitySummaryAuth("", "", "wss://x/connect", false); err == nil {
		t.Error("expected error when no token supplied")
	}
	if _, err := resolveActivitySummaryAuth("a", "b", "wss://x/connect", false); err == nil {
		t.Error("expected error when both token forms supplied")
	}
	auth, err := resolveActivitySummaryAuth("jwt-here", "", "https://broker:7888", false)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if auth.jwt != "jwt-here" {
		t.Errorf("jwt = %q", auth.jwt)
	}
	if auth.wsURL != "wss://broker:7888/connect" {
		t.Errorf("wsURL = %q", auth.wsURL)
	}
}
