package main

import (
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TestInvokedToolsRunLevel: the tracker accumulates every tool name across the
// whole run (survives BeginTurn resets), deduped and sorted; nil when none.
func TestInvokedToolsRunLevel(t *testing.T) {
	tr := &builderRealOutputTracker{}
	if tr.invokedTools() != nil {
		t.Fatal("no tools yet → nil")
	}
	tr.BeginTurn("t1", "", "", "", 0)
	tr.OnBridleEvent(bridle.ToolCallStart{ID: "1", Name: "Bash"})
	tr.OnBridleEvent(bridle.ToolCallStart{ID: "2", Name: "mcp__nexus-vision__read_image"})
	// new turn must NOT clear the run-level set
	tr.BeginTurn("t2", "", "", "", 0)
	tr.OnBridleEvent(bridle.ToolCallStart{ID: "3", Name: "Bash"}) // dup
	tr.OnBridleEvent(bridle.ToolCallStart{ID: "4", Name: "Write"})
	got := tr.invokedTools()
	want := []string{"Bash", "Write", "mcp__nexus-vision__read_image"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// empty tool name is ignored
	tr.OnBridleEvent(bridle.ToolCallStart{ID: "5", Name: ""})
	if len(tr.invokedTools()) != 3 {
		t.Fatalf("empty tool name should be ignored")
	}
}
