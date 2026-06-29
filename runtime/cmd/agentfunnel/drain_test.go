package main

import "testing"

// TestDefaultDrainPrompt_CarriesProcedure guards the self-contained drain prompt
// against silent loss of the load-bearing instructions — it must stand alone
// even when claude-side skill discovery is unavailable in the pod.
func TestDefaultDrainPrompt_CarriesProcedure(t *testing.T) {
	must := []string{
		"shadow-queue",                  // the queue the drain acts on
		"jira_list_ready",               // snapshot the ready set
		"DECOMPOSE",                     // epic → leaf tasks
		"!dispatch",                     // dispatch leaf to a builder
		"VERIFY acceptance",             // broker-log acceptance, not send_chat ok
		"double-dispatch guard",         // transition-on-dispatch
		"AUTO-MERGE",                    // the merge gate
		"ESCALATE",                      // the escalate path
		"Deploys ALWAYS escalate",       // deploy safety
		"one ticket per builder",        // hard rule
	}
	for _, m := range must {
		if !contains(defaultDrainPrompt, m) {
			t.Errorf("defaultDrainPrompt missing %q — the standalone drain procedure is incomplete", m)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
