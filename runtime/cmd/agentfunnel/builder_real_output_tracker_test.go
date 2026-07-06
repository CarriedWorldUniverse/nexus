package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// TestBuilderRealOutputTrackerCapturesToolResults (NET-46 live evidence): the
// judge only ever saw the model's streamed TEXT — a `gh pr create` tool
// result carrying the real PR URL never reached verification, so genuine
// evidence was invisible and a real, completed run was wrongly rejected.
// The tracker must fold tool-call results into its snapshot.
func TestBuilderRealOutputTrackerCapturesToolResults(t *testing.T) {
	h := &builderRealOutputTracker{}
	h.BeginTurn("t1", "main", "m", "p", 0)
	h.OnBridleEvent(bridle.ModelChunk{Text: "I opened the PR."})
	h.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "gh_pr_create"})
	h.OnBridleEvent(bridle.ToolCallResult{ID: "1", Result: json.RawMessage(`"https://github.com/org/repo/pull/413"`)})

	got := h.snapshot()
	if !strings.Contains(got, "I opened the PR.") {
		t.Errorf("snapshot lost streamed text: %q", got)
	}
	if !strings.Contains(got, "gh_pr_create") {
		t.Errorf("snapshot missing tool name: %q", got)
	}
	if !strings.Contains(got, "pull/413") {
		t.Errorf("snapshot missing tool result evidence: %q", got)
	}
}

func TestBuilderRealOutputTrackerCapturesToolErrors(t *testing.T) {
	h := &builderRealOutputTracker{}
	h.BeginTurn("t1", "main", "m", "p", 0)
	h.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "some_tool"})
	h.OnBridleEvent(bridle.ToolCallResult{ID: "1", Err: "boom: permission denied"})

	got := h.toolResultsSnapshot()
	if !strings.Contains(got, "boom: permission denied") {
		t.Errorf("tool error not captured: %q", got)
	}
	if !strings.Contains(got, "some_tool") {
		t.Errorf("tool name not captured: %q", got)
	}
}

// TestBuilderRealOutputTrackerToolResultsCapKeepsTail: a large tool result
// (e.g. a file read) must not blow the judge's context — cap the
// accumulation and keep the TAIL, since recent evidence matters most.
func TestBuilderRealOutputTrackerToolResultsCapKeepsTail(t *testing.T) {
	h := &builderRealOutputTracker{}
	h.BeginTurn("t1", "main", "m", "p", 0)

	// Well over builderToolResultCap once combined.
	big := strings.Repeat("A", builderToolResultCap)
	h.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "read"})
	h.OnBridleEvent(bridle.ToolCallResult{ID: "1", Result: json.RawMessage(`"` + big + `"`)})
	h.OnBridleEvent(bridle.ToolCallStart{ID: "2", Name: "read"})
	h.OnBridleEvent(bridle.ToolCallResult{ID: "2", Result: json.RawMessage(`"TAIL_MARKER_LAST"`)})

	got := h.toolResultsSnapshot()
	if len(got) > builderToolResultCap {
		t.Fatalf("tool results not capped: len=%d, want <= %d", len(got), builderToolResultCap)
	}
	if !strings.Contains(got, "TAIL_MARKER_LAST") {
		preview := got
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Errorf("cap dropped the most recent evidence (should keep tail): %q", preview)
	}
}

// TestBuilderRealOutputTrackerResetsToolResultsOnNewTurn: a stale prior
// turn's tool results must never leak into the NEXT turn's verification,
// mirroring the existing text-reset guarantee.
func TestBuilderRealOutputTrackerResetsToolResultsOnNewTurn(t *testing.T) {
	h := &builderRealOutputTracker{}
	h.BeginTurn("t1", "main", "m", "p", 0)
	h.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "tool"})
	h.OnBridleEvent(bridle.ToolCallResult{ID: "1", Result: json.RawMessage(`"turn one evidence"`)})
	if !strings.Contains(h.toolResultsSnapshot(), "turn one evidence") {
		t.Fatal("setup: expected turn one evidence captured")
	}

	h.BeginTurn("t2", "main", "m", "p", 0)
	if got := h.toolResultsSnapshot(); got != "" {
		t.Errorf("tool results not reset on new turn: %q", got)
	}
}
