package claudeapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus-cw/nexus/runtime/providers"
)

func TestNewEmptyKey(t *testing.T) {
	_, err := New("")
	if !errors.Is(err, providers.ErrAuth) {
		t.Errorf("New(\"\") err = %v, want ErrAuth", err)
	}
}

func TestNewFromAspectHome(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		home := t.TempDir()
		_, err := NewFromAspectHome(home)
		if !errors.Is(err, providers.ErrAuth) {
			t.Errorf("err = %v, want ErrAuth", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		home := t.TempDir()
		credsDir := filepath.Join(home, ".credentials")
		if err := os.MkdirAll(credsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credsDir, CredentialsFile), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewFromAspectHome(home)
		if !errors.Is(err, providers.ErrAuth) {
			t.Errorf("err = %v, want ErrAuth", err)
		}
	})

	t.Run("empty api_key", func(t *testing.T) {
		home := t.TempDir()
		credsDir := filepath.Join(home, ".credentials")
		if err := os.MkdirAll(credsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		blob, _ := json.Marshal(Credentials{APIKey: ""})
		if err := os.WriteFile(filepath.Join(credsDir, CredentialsFile), blob, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewFromAspectHome(home)
		if !errors.Is(err, providers.ErrAuth) {
			t.Errorf("err = %v, want ErrAuth", err)
		}
	})

	t.Run("valid", func(t *testing.T) {
		home := t.TempDir()
		credsDir := filepath.Join(home, ".credentials")
		if err := os.MkdirAll(credsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		blob, _ := json.Marshal(Credentials{APIKey: "sk-ant-test-key"})
		if err := os.WriteFile(filepath.Join(credsDir, CredentialsFile), blob, 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := NewFromAspectHome(home)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if p == nil {
			t.Fatal("nil provider")
		}
	})
}

func TestBuildMessages(t *testing.T) {
	req := providers.InvokeRequest{
		Context: []providers.Entry{
			{Kind: providers.EntryTurnUser, Payload: map[string]any{"text": "hello"}},
			{Kind: providers.EntryTurnAssistant, Payload: map[string]any{"text": "hi there"}},
			{Kind: providers.EntryTurnUser, Payload: map[string]any{"text": "follow-up"}},
			{Kind: providers.EntrySystemPrompt, Payload: map[string]any{"text": "ignored"}},
		},
		Prompt: "new question",
	}
	msgs, err := buildMessages(req)
	if err != nil {
		t.Fatalf("buildMessages err = %v", err)
	}

	// 3 from Context + 1 Prompt. System-prompt entries skipped.
	if got, want := len(msgs), 4; got != want {
		t.Errorf("len(msgs) = %d, want %d", got, want)
	}
}

func TestBuildMessagesSkipsEmptyText(t *testing.T) {
	req := providers.InvokeRequest{
		Context: []providers.Entry{
			{Kind: providers.EntryTurnUser, Payload: map[string]any{}}, // no text
			{Kind: providers.EntryTurnAssistant, Payload: nil},         // nil payload
			{Kind: providers.EntryTurnUser, Payload: map[string]any{"text": "real"}},
		},
	}
	msgs, err := buildMessages(req)
	if err != nil {
		t.Fatalf("buildMessages err = %v", err)
	}
	if got, want := len(msgs), 1; got != want {
		t.Errorf("len(msgs) = %d, want %d", got, want)
	}
}

func TestBuildMessagesRejectsToolResultUntilPart7(t *testing.T) {
	req := providers.InvokeRequest{
		Context: []providers.Entry{
			{ID: "entry-123", Kind: providers.EntryTurnToolResult, Payload: map[string]any{"text": "tool output"}},
		},
	}
	_, err := buildMessages(req)
	if !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestNormaliseStopReason(t *testing.T) {
	cases := map[string]providers.StopReason{
		"end_turn":      providers.StopEndTurn,
		"tool_use":      providers.StopToolUse,
		"max_tokens":    providers.StopMaxTokens,
		"stop_sequence": providers.StopEndTurn,
		"weird":         providers.StopErrorReason,
		"":              providers.StopErrorReason,
	}
	for raw, want := range cases {
		if got := normaliseStopReason(raw); got != want {
			t.Errorf("normaliseStopReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCapabilitiesShape(t *testing.T) {
	p := &Provider{}
	caps := p.Capabilities()
	if !caps.Chat {
		t.Error("Chat should be true")
	}
	if caps.Embeddings {
		t.Error("Embeddings should be false (Anthropic has no embeddings endpoint)")
	}
	if !caps.ToolUse {
		t.Error("ToolUse should be true")
	}
	if !caps.SupportsTriage {
		t.Error("SupportsTriage should be true")
	}
	if len(caps.ThinkingLevels) == 0 {
		t.Error("ThinkingLevels should not be empty")
	}
}

func TestEmbedReturnsUnsupported(t *testing.T) {
	p := &Provider{}
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "x"})
	if !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("Embed err = %v, want ErrUnsupported", err)
	}
}

func TestStreamReturnsUnsupported(t *testing.T) {
	p := &Provider{}
	_, err := p.Stream(context.Background(), providers.InvokeRequest{})
	if !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("Stream err = %v, want ErrUnsupported", err)
	}
}

func TestCompactRejectsEmptyEntries(t *testing.T) {
	p := &Provider{}
	_, err := p.Compact(context.Background(), nil, "")
	if err == nil {
		t.Error("expected error for empty entries")
	}
}

func TestCompactionRoleFor(t *testing.T) {
	cases := map[providers.EntryKind]string{
		providers.EntryTurnUser:       "user",
		providers.EntryTurnAssistant:  "assistant",
		providers.EntryTurnToolResult: "tool_result",
		providers.EntrySystemPrompt:   "system",
		providers.EntryCompaction:     "prior_summary",
		providers.EntryBranchSummary:  "branch_summary",
	}
	for kind, want := range cases {
		if got := compactionRoleFor(kind); got != want {
			t.Errorf("compactionRoleFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestEstimateTokensRoughlyProportional(t *testing.T) {
	if got := estimateTokens("abcd"); got != 1 {
		t.Errorf("estimateTokens(4 chars) = %d, want 1", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Errorf("estimateTokens(\"\") = %d, want 0", got)
	}
}

func TestTriageModel(t *testing.T) {
	p := &Provider{}
	if got := p.TriageModel(); got != TriageModelID {
		t.Errorf("TriageModel = %q, want %q", got, TriageModelID)
	}
}

func TestModelsListsExpected(t *testing.T) {
	p := &Provider{}
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("no models returned")
	}
	foundOpus, foundHaiku := false, false
	for _, m := range models {
		if m.ID == DefaultModel {
			foundOpus = true
		}
		if m.ID == TriageModelID {
			foundHaiku = true
		}
	}
	if !foundOpus {
		t.Error("DefaultModel missing from Models list")
	}
	if !foundHaiku {
		t.Error("TriageModelID missing from Models list")
	}
}

func TestExtractSchemaFlatObject(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	}
	props, req := extractSchema(schema)
	m, ok := props.(map[string]any)
	if !ok {
		t.Fatalf("want map, got %T", props)
	}
	if _, ok := m["path"]; !ok {
		t.Error("properties missing 'path'")
	}
	if got, want := len(req), 1; got != want {
		t.Errorf("required len = %d, want %d", got, want)
	}
}

func TestExtractSchemaNil(t *testing.T) {
	props, req := extractSchema(nil)
	if props == nil {
		t.Error("want non-nil properties")
	}
	if req != nil {
		t.Errorf("required = %v, want nil", req)
	}
}

func TestExtractSchemaNonFlatPassesThrough(t *testing.T) {
	// A schema using $ref or oneOf at the root should be passed
	// through verbatim rather than collapsed to an empty map.
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"$ref": "#/definitions/pathQuery"},
			map[string]any{"$ref": "#/definitions/idQuery"},
		},
	}
	props, _ := extractSchema(schema)
	m, ok := props.(map[string]any)
	if !ok {
		t.Fatalf("want map, got %T", props)
	}
	if _, ok := m["oneOf"]; !ok {
		t.Error("non-flat schema should be passed through, oneOf missing")
	}
}

func TestExtractRequired(t *testing.T) {
	schema := map[string]any{"required": []any{"path", "content"}}
	req := extractRequired(schema)
	if got, want := len(req), 2; got != want {
		t.Errorf("len(required) = %d, want %d", got, want)
	}
}
