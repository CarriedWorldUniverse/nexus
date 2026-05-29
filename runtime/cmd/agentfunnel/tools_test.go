package main

import (
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TestToolsForNativeProviderIncludesLocalAndComms verifies that a
// native-API provider gets BOTH the host comms tools (lane 2) and the
// local coding tools (lane 1) on its tool surface, while claude-code
// still gets nil (it self-supplies its tool surface natively).
func TestToolsForNativeProviderIncludesLocalAndComms(t *testing.T) {
	defs := toolsForProviderAgent(bridle.ProviderOpenAI)
	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = true
	}
	// bash/write/read are local lane; send_chat is the comms lane.
	for _, want := range []string{"bash", "write", "read", "send_chat"} {
		if !got[want] {
			t.Errorf("native tool surface missing %q", want)
		}
	}
	if toolsForProviderAgent("claude-code") != nil {
		t.Error("claude-code should still get nil (self-supplies tools)")
	}
	if toolsForProviderAgent("claudecode") != nil {
		t.Error("claudecode alias should still get nil (self-supplies tools)")
	}
}
