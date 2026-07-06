package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	funnelhooks "github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/hooks"
)

type testHookHandler func(context.Context, map[string]any) (funnelhooks.Decision, error)

func (h testHookHandler) Run(ctx context.Context, payload map[string]any) (funnelhooks.Decision, error) {
	return h(ctx, payload)
}

func TestDeliberate_ReadMemoryHookPreservesAutoRecallInjection(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{{
		Topic: "deploy", FromAgent: "anvil", Content: "build then ship", UpdatedAt: "2026-06-08", Score: -5,
	}}}
	prov := &scriptedProvider{results: []bridle.ProviderResult{{FinalText: "ack"}}}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test system prompt",
		Harness:      bridle.NewHarness(prov),
		Provider:     "scripted",
		Model:        "test-model",
		Runner:       noopRunner{},
		AutoRecall:   AutoRecallConfig{Enabled: true, Gateway: kg},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Deliberate(context.Background(), "how do I deploy"); err != nil {
		t.Fatalf("Deliberate failed: %v", err)
	}
	got := prov.last.AppendSystemPrompt
	for _, want := range []string{"<recalled-knowledge>", CommonplaceGuard, "build then ship"} {
		if !strings.Contains(got, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, got)
		}
	}
	if len(kg.searchCalls) != 1 {
		t.Fatalf("search calls = %d, want 1", len(kg.searchCalls))
	}
}

func TestDeliberate_SessionStartAdditionalContextInjectsAtTopOfTurn(t *testing.T) {
	engine := funnelhooks.New()
	if err := engine.Register("SessionStart", "*", 0, testHookHandler(func(_ context.Context, payload map[string]any) (funnelhooks.Decision, error) {
		if payload["hook_event_name"] != "SessionStart" {
			t.Fatalf("hook_event_name = %v", payload["hook_event_name"])
		}
		return funnelhooks.Decision{AdditionalContext: "session memory"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	f, prov := newTestFunnel(t, bridle.ProviderResult{FinalText: "ack"})
	f.cfg.Hooks = engine

	if _, err := f.Deliberate(context.Background(), "hello"); err != nil {
		t.Fatalf("Deliberate failed: %v", err)
	}
	if !strings.Contains(prov.last.AppendSystemPrompt, "session memory") {
		t.Fatalf("SessionStart additionalContext not injected:\n%s", prov.last.AppendSystemPrompt)
	}
}

func TestDeliberate_SessionStartAdditionalContext_PrefixStableRoutesToUserMessage(t *testing.T) {
	t.Setenv("FUNNEL_PREFIX_STABLE", "1")

	engine := funnelhooks.New()
	if err := engine.Register("SessionStart", "*", 0, testHookHandler(func(_ context.Context, _ map[string]any) (funnelhooks.Decision, error) {
		return funnelhooks.Decision{AdditionalContext: "session memory"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	f, prov := newTestFunnel(t, bridle.ProviderResult{FinalText: "ack"})
	f.cfg.Hooks = engine

	if _, err := f.Deliberate(context.Background(), "hello"); err != nil {
		t.Fatalf("Deliberate failed: %v", err)
	}
	// FUNNEL_PREFIX_STABLE=1: the hook block must NOT land on the system
	// prompt (that would bust the cached prefix every turn it fires) —
	// it must land in the trailing per-turn user/delta zone instead.
	if strings.Contains(prov.last.AppendSystemPrompt, "session memory") {
		t.Fatalf("FUNNEL_PREFIX_STABLE=1: hook context leaked into system prompt:\n%s", prov.last.AppendSystemPrompt)
	}
	gotInUserMessage := false
	for _, m := range prov.last.Messages {
		if strings.Contains(m.Content, "session memory") {
			gotInUserMessage = true
			break
		}
	}
	if !gotInUserMessage {
		t.Fatalf("FUNNEL_PREFIX_STABLE=1: hook context missing from trailing messages: %+v", prov.last.Messages)
	}
}

func TestDeliberate_WriteMemoryHookCapturesExplicitMarker(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	f, _ := newTestFunnel(t, bridle.ProviderResult{FinalText: "Commonplace: release decision\nUse blue/green for NEX-509.\n---\nnormal reply"})
	f.cfg.AutoRecall.Gateway = kg
	registerBuiltInMemoryHooks(f.cfg.Hooks, f.cfg)

	if _, err := f.Deliberate(context.Background(), "ship"); err != nil {
		t.Fatalf("Deliberate failed: %v", err)
	}
	if len(kg.storeCalls) != 1 {
		t.Fatalf("store calls = %d, want 1", len(kg.storeCalls))
	}
	got := kg.storeCalls[0]
	if got.FromAgent != "frame" || got.Topic != "release decision" || !strings.Contains(got.Content, "blue/green") {
		b, _ := json.Marshal(got)
		t.Fatalf("unexpected store call: %s", b)
	}
}

func TestWriteMemoryHookStoreFailureFailsOpen(t *testing.T) {
	kg := &fakeKnowledgeGateway{storeErr: errors.New("commonplace down")}
	h := writeMemoryHook{cfg: Config{
		AspectID:   "frame",
		AutoRecall: AutoRecallConfig{Gateway: kg},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	_, err := h.Run(context.Background(), map[string]any{
		"final_text": "Memory: lesson\nDo not fail the turn.",
	})
	if err != nil {
		t.Fatalf("write hook must fail open, got error: %v", err)
	}
}
