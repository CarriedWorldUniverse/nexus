package main

import "testing"

func TestDispatchControllerRegisterProvider(t *testing.T) {
	if got := dispatchControllerRegisterProvider("claude-api"); got != "claude-api" {
		t.Fatalf("non-empty provider = %q, want claude-api", got)
	}
	if got := dispatchControllerRegisterProvider("   "); got != "dispatch-controller" {
		t.Fatalf("empty provider fallback = %q, want dispatch-controller", got)
	}
}
