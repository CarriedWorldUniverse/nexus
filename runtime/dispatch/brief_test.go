package dispatch

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
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

func TestParseBrief_DispatchDirectives(t *testing.T) {
	t.Run("dashboard composed command round-trips", func(t *testing.T) {
		b, err := ParseBrief([]byte("!dispatch anvil%codex-cli repo=org/repo ticket=NEX-1 do the thing"))
		if err != nil {
			t.Fatal(err)
		}
		if b.Agent != "anvil" {
			t.Errorf("agent = %q, want anvil", b.Agent)
		}
		if b.Provider != "codex-cli" {
			t.Errorf("provider = %q, want codex-cli", b.Provider)
		}
		if b.Repo != "org/repo" {
			t.Errorf("repo = %q, want org/repo", b.Repo)
		}
		if b.Ticket != "NEX-1" {
			t.Errorf("ticket = %q, want NEX-1", b.Ticket)
		}
		if b.Thread != "NEX-1" {
			t.Errorf("thread = %q, want NEX-1", b.Thread)
		}
		if b.Task != "do the thing" {
			t.Errorf("task = %q, want do the thing", b.Task)
		}
	})

	t.Run("repo + ticket + branch directives", func(t *testing.T) {
		b, err := ParseBrief([]byte("!dispatch anvil%codex-cli repo=CarriedWorldUniverse/nexus ticket=NEX-510 branch=feat/x implement the thing"))
		if err != nil {
			t.Fatal(err)
		}
		if b.Agent != "anvil" || b.Provider != "codex-cli" {
			t.Fatalf("agent/provider = %q/%q", b.Agent, b.Provider)
		}
		if b.Repo != "CarriedWorldUniverse/nexus" {
			t.Errorf("repo = %q", b.Repo)
		}
		if b.Ticket != "NEX-510" {
			t.Errorf("ticket = %q, want NEX-510 (explicit, not hash)", b.Ticket)
		}
		if b.Branch != "feat/x" {
			t.Errorf("branch = %q", b.Branch)
		}
		if b.Task != "implement the thing" {
			t.Errorf("task = %q", b.Task)
		}
		if b.Thread != "NEX-510" {
			t.Errorf("thread = %q, want = ticket", b.Thread)
		}
	})

	t.Run("no directives stays backward-compatible (hash ticket, empty repo)", func(t *testing.T) {
		b, err := ParseBrief([]byte("!dispatch anvil fix the bug"))
		if err != nil {
			t.Fatal(err)
		}
		if b.Repo != "" || b.Branch != "" {
			t.Errorf("repo/branch should be empty: %+v", b)
		}
		if b.Task != "fix the bug" {
			t.Errorf("task = %q", b.Task)
		}
		if b.Ticket == "" || b.Ticket != b.Thread {
			t.Errorf("expected generated hash ticket = thread: %+v", b)
		}
	})

	t.Run("unknown key=value is task text, not consumed", func(t *testing.T) {
		b, err := ParseBrief([]byte("!dispatch anvil repo=org/r set the flag x=1"))
		if err != nil {
			t.Fatal(err)
		}
		if b.Repo != "org/r" {
			t.Errorf("repo = %q", b.Repo)
		}
		if b.Task != "set the flag x=1" {
			t.Errorf("task = %q, want the x=1 preserved in task", b.Task)
		}
	})

	t.Run("directives but no task is an error", func(t *testing.T) {
		if _, err := ParseBrief([]byte("!dispatch anvil repo=org/r ticket=NEX-1")); err == nil {
			t.Fatal("expected error when only directives and no task")
		}
	})
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

	// Verify omitempty: broker-set fields must not appear in the wire
	// format when zero (root dispatches must not emit run_id:"" etc.)
	bEmpty := Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}
	raw, _ := json.Marshal(bEmpty)
	if bytes.Contains(raw, []byte("run_id")) {
		t.Errorf("omitempty: run_id should be absent when zero, got %s", raw)
	}
}

// TestBriefRoleAtSpawnFields is a table test of the M1 Unit 3 round-trip:
// Role, WorkItemID, SkillAllowlist, PolicyFragment, and Personality must
// survive a JSON marshal/unmarshal (the !dispatch header ⇄ ConfigMap
// path) unchanged, and must be entirely absent from the wire format when
// the brief carries none of them — the additive/back-compat invariant.
func TestBriefRoleAtSpawnFields(t *testing.T) {
	frag := &funnel.ToolPolicy{
		DefaultAllow:   false,
		Tools:          map[string]bool{"write": false},
		WritePathAllow: []string{"tests/"},
	}
	b := Brief{
		Agent:          "anvil",
		Ticket:         "NEX-999",
		Thread:         "NEX-999",
		Role:           "you are a tester. write and run tests only.",
		WorkItemID:     "work-item-42",
		SkillAllowlist: []string{"test-run", "bash", "read"},
		PolicyFragment: frag,
		Personality:    "anvil",
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var got Brief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Role != b.Role {
		t.Errorf("Role: got %q, want %q", got.Role, b.Role)
	}
	if got.WorkItemID != b.WorkItemID {
		t.Errorf("WorkItemID: got %q, want %q", got.WorkItemID, b.WorkItemID)
	}
	if len(got.SkillAllowlist) != 3 || got.SkillAllowlist[0] != "test-run" {
		t.Errorf("SkillAllowlist: got %v", got.SkillAllowlist)
	}
	if got.PolicyFragment == nil || got.PolicyFragment.DefaultAllow != false || !reflect.DeepEqual(got.PolicyFragment.WritePathAllow, frag.WritePathAllow) {
		t.Errorf("PolicyFragment: got %+v, want %+v", got.PolicyFragment, frag)
	}
	if got.Personality != b.Personality {
		t.Errorf("Personality: got %q, want %q", got.Personality, b.Personality)
	}

	// Additive/back-compat: a brief with none of these set must emit none
	// of these keys on the wire (existing !dispatch consumers unaffected).
	bEmpty2 := Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}
	raw2, _ := json.Marshal(bEmpty2)
	for _, key := range []string{"role", "work_item_id", "skill_allowlist", "policy_fragment", "personality"} {
		if bytes.Contains(raw2, []byte(`"`+key+`"`)) {
			t.Errorf("omitempty: %q should be absent when zero, got %s", key, raw2)
		}
	}
}

// TestBriefConfigMapData is a table test of briefConfigMapData: the
// ConfigMap Data map that becomes -brief-file plus the role-at-spawn
// overlay files (role.md, policy.json). An empty Brief reproduces
// exactly today's single-key ConfigMap.
func TestBriefConfigMapData(t *testing.T) {
	tests := []struct {
		name string
		b    Brief
		want map[string]string
	}{
		{
			name: "no overlay reproduces today's single-key ConfigMap",
			b:    Brief{Task: "do the thing"},
			want: map[string]string{"brief.md": "do the thing"},
		},
		{
			name: "role adds role.md",
			b:    Brief{Task: "do the thing", Role: "you are a builder"},
			want: map[string]string{"brief.md": "do the thing", "role.md": "you are a builder"},
		},
		{
			name: "policy fragment adds policy.json",
			b:    Brief{Task: "do the thing", PolicyFragment: &funnel.ToolPolicy{DefaultAllow: false}},
			want: map[string]string{"brief.md": "do the thing", "policy.json": `{"default_allow":false}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := briefConfigMapData(tc.b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("briefConfigMapData() = %v, want %v", got, tc.want)
			}
		})
	}
}
