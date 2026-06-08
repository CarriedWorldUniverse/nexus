package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFrameCarriesRunID(t *testing.T) {
	f := Frame{Kind: FrameTurn, Aspect: "anvil", RunID: "run-z"}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"run_id":"run-z"`) {
		t.Fatalf("run_id not serialized: %s", b)
	}
	var back Frame
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.RunID != "run-z" {
		t.Fatalf("run_id round-trip = %q", back.RunID)
	}
}
