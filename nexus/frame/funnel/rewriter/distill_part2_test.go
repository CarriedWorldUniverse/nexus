package rewriter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// recordingDistiller logs every DistillToolResult call so tests can
// assert what tool name was passed in.
type recordingDistiller struct {
	stubDistiller
	tools []string
}

func (r *recordingDistiller) DistillToolResult(ctx context.Context, tool, content string) (string, error) {
	r.tools = append(r.tools, tool)
	return r.stubDistiller.DistillToolResult(ctx, tool, content)
}

// Tool name plumbing: the distiller should receive the right tool name
// (Bash, Agent, Read, etc.) for each tool_result, looked up via the
// preceding assistant tool_use block by tool_use_id.
func TestDistillTurn_ToolNameRouting(t *testing.T) {
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		// Two tool_uses with different names.
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_bash", "name": "Bash", "input": map[string]any{"command": "ls"}},
					map[string]any{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"prompt": "go"}},
				},
			},
		},
		// Their tool_results.
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_bash", "content": []any{map[string]any{"type": "text", "text": long}}},
				},
			},
		},
		{
			"type": "user",
			"uuid": "u2",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_agent", "content": []any{map[string]any{"type": "text", "text": long}}},
				},
			},
		},
	})

	d := &recordingDistiller{stubDistiller: stubDistiller{suffix: "named"}}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})

	if _, err := rw.DistillTurn(context.Background(), "u2"); err != nil {
		t.Fatal(err)
	}

	if len(d.tools) != 2 {
		t.Fatalf("expected 2 distill calls, got %d (%v)", len(d.tools), d.tools)
	}
	wantTools := []string{"Bash", "Agent"}
	for i, want := range wantTools {
		if d.tools[i] != want {
			t.Errorf("call %d: got tool=%q, want %q", i, d.tools[i], want)
		}
	}
}

// Unknown tool_use_id (no preceding tool_use record) → tool name is
// empty string. Distiller still gets called; the prompt builder falls
// back to a generic shape.
func TestDistillTurn_UnknownToolUseID_EmptyName(t *testing.T) {
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_orphan", // no matching tool_use
						"content":     []any{map[string]any{"type": "text", "text": long}},
					},
				},
			},
		},
	})

	d := &recordingDistiller{stubDistiller: stubDistiller{suffix: "orphan"}}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})

	if _, err := rw.DistillTurn(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if len(d.tools) != 1 || d.tools[0] != "" {
		t.Errorf("expected one call with empty tool name, got %v", d.tools)
	}
}

// Agent tool_results carry a top-level toolUseResult.content that
// duplicates the report. Confirm the rewriter syncs it to the
// distilled string while preserving every other toolUseResult field
// (status, agentId, usage, etc).
func TestDistillTurn_AgentToolUseResultSync(t *testing.T) {
	report := longString(3000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"prompt": "audit"}},
				},
			},
		},
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_agent",
						"content":     []any{map[string]any{"type": "text", "text": report}},
					},
				},
			},
			"toolUseResult": map[string]any{
				"status":            "completed",
				"agentId":           "agt_xyz",
				"agentType":         "Explore",
				"prompt":            "audit",
				"content":           report,
				"totalDurationMs":   12345,
				"totalTokens":       6789,
				"totalToolUseCount": 4,
				"usage": map[string]any{
					"input_tokens":  100,
					"output_tokens": 200,
				},
				"toolStats": map[string]any{"Read": 4},
			},
		},
	})
	d := &stubDistiller{suffix: "agent-sync"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})

	if _, err := rw.DistillTurn(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}

	out := readJSONL(t, path)
	rec := out[1] // user record
	tur, ok := rec["toolUseResult"].(map[string]any)
	if !ok {
		t.Fatalf("toolUseResult missing or wrong type: %#v", rec["toolUseResult"])
	}

	// content was rewritten to the distilled string.
	if got, ok := tur["content"].(string); !ok || !strings.HasPrefix(got, "[distilled]") {
		t.Errorf("toolUseResult.content not synced to distilled: %v", tur["content"])
	}
	// Critical preservation properties: every other field intact.
	for _, field := range []string{"status", "agentId", "agentType", "prompt", "totalDurationMs", "totalTokens", "totalToolUseCount", "usage", "toolStats"} {
		if tur[field] == nil {
			t.Errorf("toolUseResult.%s stripped during sync", field)
		}
	}
	if tur["status"] != "completed" || tur["agentId"] != "agt_xyz" {
		t.Errorf("toolUseResult fields mutated unexpectedly: %v", tur)
	}
	usage, _ := tur["usage"].(map[string]any)
	if usage == nil || usage["input_tokens"] == nil {
		t.Errorf("toolUseResult.usage block stripped: %v", tur["usage"])
	}
}

// Non-Agent tool_results: toolUseResult is either absent or carries
// small structured data. The sync function should leave non-string
// content shapes alone rather than corrupt them.
func TestDistillTurn_NonAgentToolUseResult_NotMutated(t *testing.T) {
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_bash", "name": "Bash", "input": map[string]any{"command": "ls"}},
				},
			},
		},
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_bash",
						"content":     []any{map[string]any{"type": "text", "text": long}},
					},
				},
			},
			// Bash's toolUseResult shape: structured, not a string.
			"toolUseResult": map[string]any{
				"interrupted":  false,
				"isImage":      false,
				"stdout":       long,
				"stderr":       "",
				"returnCodeInterpretation": "Success",
			},
		},
	})
	d := &stubDistiller{suffix: "bash"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})
	if _, err := rw.DistillTurn(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	out := readJSONL(t, path)
	rec := out[1]
	tur, ok := rec["toolUseResult"].(map[string]any)
	if !ok {
		t.Fatalf("toolUseResult missing")
	}
	// content field is absent in this shape; the sync function
	// short-circuits. stdout (the bigger field) is left intact —
	// we only sync the .content key, not stdout.
	if got, ok := tur["stdout"].(string); !ok || got != long {
		t.Errorf("Bash toolUseResult.stdout was mutated by sync — should not happen")
	}
}

// Per-tool prompt builder: confirm the Bash/Read/Grep/Agent/empty
// branches all produce distinct shapes.
func TestBuildToolResultPrompt_PerToolBranches(t *testing.T) {
	cases := []struct {
		tool    string
		content string
		mustHave string
	}{
		{"Bash", "$ ls\nfoo bar", "Tool: Bash"},
		{"Read", "1 line\n2 line", "Tool: Read"},
		{"Grep", "match.go:5: foo", "Tool: Grep"},
		{"Agent", "audit report ...", "Tool: Agent"},
		{"Task", "subagent ...", "Tool: Agent"},
		{"Edit", "applied", "Tool: Edit"},
		{"Write", "applied", "Tool: Write"},
		{"", "raw", "Tool output:"},
		{"UnknownThing", "x", "Tool: UnknownThing"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			got := buildToolResultPrompt(tc.tool, tc.content)
			if !strings.Contains(got, tc.mustHave) {
				t.Errorf("tool=%q: prompt missing %q\ngot: %q", tc.tool, tc.mustHave, got)
			}
			if !strings.Contains(got, tc.content) {
				t.Errorf("tool=%q: prompt missing content payload", tc.tool)
			}
		})
	}
}

// Agent record where toolUseResult exists but has no content key.
// syncToolUseResult must short-circuit cleanly — no error, no
// mutation. Future schema additions might look similar.
func TestDistillTurn_AgentToolUseResult_NoContentKey(t *testing.T) {
	report := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"prompt": "x"}},
				},
			},
		},
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_agent",
						"content":     []any{map[string]any{"type": "text", "text": report}},
					},
				},
			},
			"toolUseResult": map[string]any{
				"status":  "completed",
				"agentId": "agt_xyz",
				// no `content` key
			},
		},
	})
	d := &stubDistiller{suffix: "no-content-key"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})
	if _, err := rw.DistillTurn(context.Background(), "u1"); err != nil {
		t.Fatalf("expected clean run, got %v", err)
	}
	out := readJSONL(t, path)
	tur := out[1]["toolUseResult"].(map[string]any)
	if tur["status"] != "completed" || tur["agentId"] != "agt_xyz" {
		t.Errorf("toolUseResult fields mutated when no content to sync: %v", tur)
	}
	if _, hasContent := tur["content"]; hasContent {
		t.Errorf("sync added a content field that wasn't there originally")
	}
}

// Agent record with two text blocks: first below threshold, second
// above. Sync should fire from the single distilled block (canonical
// is set, rewriteCount == 1).
func TestDistillTurn_AgentMultiBlock_PartialThreshold_SyncFires(t *testing.T) {
	short := "short prelude"
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_agent", "name": "Agent", "input": map[string]any{"prompt": "x"}},
				},
			},
		},
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_agent",
						"content": []any{
							map[string]any{"type": "text", "text": short},
							map[string]any{"type": "text", "text": long},
						},
					},
				},
			},
			"toolUseResult": map[string]any{
				"status":  "completed",
				"content": short + long, // single string covers both
			},
		},
	})
	d := &stubDistiller{suffix: "multi-partial"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})
	if _, err := rw.DistillTurn(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	out := readJSONL(t, path)
	tur := out[1]["toolUseResult"].(map[string]any)
	if got, ok := tur["content"].(string); !ok || !strings.HasPrefix(got, "[distilled]") {
		t.Errorf("sync should fire when only one block above threshold (single canonical), got %v", tur["content"])
	}
}

// Agent.toolUseResult.content with the wrong shape (object, not
// string) should be left untouched — guards against crashes if
// claude-code changes the schema.
func TestSyncToolUseResult_NonStringContent_LeftAlone(t *testing.T) {
	rec := rawRecord{}
	rec["message"] = json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"tu_x","content":[{"type":"text","text":"x"}]}]}`)
	// content as object, not string
	rec["toolUseResult"] = json.RawMessage(`{"content":{"weird":"shape"}}`)
	if err := syncToolUseResult(rec, map[string]string{"tu_x": "[distilled] x"}); err != nil {
		t.Errorf("expected no error on object-shaped content, got %v", err)
	}
	// Field unchanged.
	var tur map[string]any
	_ = json.Unmarshal(rec["toolUseResult"], &tur)
	if c, ok := tur["content"].(map[string]any); !ok || c["weird"] != "shape" {
		t.Errorf("non-string content was mutated: %v", tur["content"])
	}
}
