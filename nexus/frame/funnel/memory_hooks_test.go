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
	// Hook-injected recall context routes to the trailing per-turn
	// user/delta zone, never the system prompt (see funnel.go's
	// buildTurnRequest: tail-injection is unconditional so a turn where
	// recall fires never busts the whole-conversation vLLM prefix cache).
	if strings.Contains(prov.last.AppendSystemPrompt, "build then ship") {
		t.Fatalf("recall context leaked into system prompt:\n%s", prov.last.AppendSystemPrompt)
	}
	got := messagesText(prov.last.Messages)
	for _, want := range []string{"<recalled-knowledge>", CommonplaceGuard, "build then ship"} {
		if !strings.Contains(got, want) {
			t.Fatalf("trailing messages missing %q:\n%s", want, got)
		}
	}
	if len(kg.searchCalls) != 1 {
		t.Fatalf("search calls = %d, want 1", len(kg.searchCalls))
	}
}

func TestDeliberate_SessionStartAdditionalContextRoutesToUserMessage(t *testing.T) {
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
	// The hook block must NOT land on the system prompt (that would bust
	// the cached prefix every turn it fires) — it must land in the
	// trailing per-turn user/delta zone instead. This is the only path;
	// there is no env-gated system-prompt alternative.
	if strings.Contains(prov.last.AppendSystemPrompt, "session memory") {
		t.Fatalf("hook context leaked into system prompt:\n%s", prov.last.AppendSystemPrompt)
	}
	if !strings.Contains(messagesText(prov.last.Messages), "session memory") {
		t.Fatalf("hook context missing from trailing messages: %+v", prov.last.Messages)
	}
}

func messagesText(msgs []bridle.ProviderMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
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
