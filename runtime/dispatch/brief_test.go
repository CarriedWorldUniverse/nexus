package dispatch

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestParseBrief(t *testing.T) {
	msg := "dispatch to a k3s builder\n```json\n" +
		`{"agent":"anvil","provider":"openai","repo":"CarriedWorldUniverse/nexus","ticket":"NEX-999","thread":"NEX-999"}` +
		"\n```\nImplement the flag and open a PR.\n"
	b, err := ParseBrief([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if b.Agent != "anvil" || b.Ticket != "NEX-999" || b.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("fields wrong: %+v", b)
	}
	if b.Provider != "openai" {
		t.Errorf("provider = %q, want openai", b.Provider)
	}
	if b.Task == "" || b.Task[:9] != "Implement" {
		t.Errorf("Task = %q, want the trailing free text", b.Task)
	}
}

func TestParseBrief_DispatchCommand(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantAgent    string
		wantProvider string
		wantTask     string
	}{
		{
			name:      "default provider",
			body:      "!dispatch anvil implement the flag",
			wantAgent: "anvil",
			wantTask:  "implement the flag",
		},
		{
			name:         "provider override",
			body:         "!dispatch anvil%openai implement the flag",
			wantAgent:    "anvil",
			wantProvider: "openai",
			wantTask:     "implement the flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := ParseBrief([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if b.Agent != tt.wantAgent || b.Provider != tt.wantProvider || b.Task != tt.wantTask {
				t.Fatalf("brief = %+v", b)
			}
			if b.Ticket == "" || b.Thread == "" {
				t.Fatalf("command brief should get ticket/thread: %+v", b)
			}
			if b.Ticket != b.Thread {
				t.Fatalf("ticket = %q, thread = %q; want same generated id", b.Ticket, b.Thread)
			}
		})
	}
}

func TestParseBrief_MissingAgent(t *testing.T) {
	if _, err := ParseBrief([]byte("```json\n{\"ticket\":\"NEX-1\"}\n```\nx")); err == nil {
		t.Fatal("expected error when agent missing")
	}
}

func TestBriefNewFields(t *testing.T) {
	b := Brief{
		Agent:       "anvil",
		Ticket:      "NEX-999",
		Thread:      "NEX-999",
		RunID:       "run-abc123",
		ParentRunID: "run-parent",
		PoolSlot:    "builder-2",
		Task:        "do the thing",
	}
	data, _ := json.Marshal(b)
	var got Brief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run-abc123" {
		t.Errorf("RunID: got %q", got.RunID)
	}
	if got.ParentRunID != "run-parent" {
		t.Errorf("ParentRunID: got %q", got.ParentRunID)
	}
	if got.PoolSlot != "builder-2" {
		t.Errorf("PoolSlot: got %q", got.PoolSlot)
	}

	// Verify omitempty: broker-set fields must not appear in the wire
	// format when zero (root dispatches must not emit run_id:"" etc.)
	bEmpty := Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}
	raw, _ := json.Marshal(bEmpty)
	if bytes.Contains(raw, []byte("run_id")) {
		t.Errorf("omitempty: run_id should be absent when zero, got %s", raw)
	}
}
