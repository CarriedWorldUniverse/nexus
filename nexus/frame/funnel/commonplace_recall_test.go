package funnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// recallFunnel builds a minimal Funnel exercising only recallForTurn's
// dependencies (cfg.AutoRecall, cfg.AspectID, cfg.Logger).
func recallFunnel(ar AutoRecallConfig) *Funnel {
	return &Funnel{cfg: Config{
		AspectID:   "tester",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AutoRecall: ar,
	}}
}

func TestRecallForTurn_OffByDefault(t *testing.T) {
	// Zero-value AutoRecall (Enabled=false) → no recall, gateway untouched.
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{{Topic: "t", Content: "c"}}}
	f := recallFunnel(AutoRecallConfig{Gateway: kg}) // Enabled defaults false
	if got := f.recallForTurn(context.Background(), "anything"); got != "" {
		t.Errorf("disabled auto-recall should return empty, got %q", got)
	}
	if len(kg.searchCalls) != 0 {
		t.Errorf("disabled auto-recall must not query the gateway; got %d calls", len(kg.searchCalls))
	}
}

func TestRecallForTurn_NilGateway(t *testing.T) {
	f := recallFunnel(AutoRecallConfig{Enabled: true, Gateway: nil})
	if got := f.recallForTurn(context.Background(), "anything"); got != "" {
		t.Errorf("nil gateway should return empty, got %q", got)
	}
}

func TestRecallForTurn_EmptyMessage(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{{Topic: "t", Content: "c"}}}
	f := recallFunnel(AutoRecallConfig{Enabled: true, Gateway: kg})
	if got := f.recallForTurn(context.Background(), "   "); got != "" {
		t.Errorf("blank message should skip recall, got %q", got)
	}
	if len(kg.searchCalls) != 0 {
		t.Error("blank message must not query the gateway")
	}
}

func TestRecallForTurn_FailOpen(t *testing.T) {
	// A gateway error must NEVER fail the turn — recall returns "".
	kg := &fakeKnowledgeGateway{searchErr: errors.New("broker down")}
	f := recallFunnel(AutoRecallConfig{Enabled: true, Gateway: kg})
	if got := f.recallForTurn(context.Background(), "deploy steps"); got != "" {
		t.Errorf("gateway error should fail open (empty), got %q", got)
	}
}

func TestRecallForTurn_InjectsSafeFramedBlock(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{
		{Topic: "deploy", FromAgent: "anvil", Content: "build then ship", UpdatedAt: "2026-05-30", Score: -5},
	}}
	f := recallFunnel(AutoRecallConfig{Enabled: true, Gateway: kg})
	got := f.recallForTurn(context.Background(), "how do I deploy")
	if got == "" {
		t.Fatal("expected a recalled-knowledge block, got empty")
	}
	// Scope: own + shared, caller identity threaded through.
	if len(kg.searchCalls) != 1 {
		t.Fatalf("want 1 gateway call, got %d", len(kg.searchCalls))
	}
	q := kg.searchCalls[0]
	if q.Agent != "tester" || !q.OwnAgent || !q.Shared {
		t.Errorf("scope wrong: %+v (want agent=tester own+shared)", q)
	}
	// Auto-recall must use keyword (OR-of-terms) matching — a whole-message
	// phrase query matches almost nothing.
	if !q.Keyword {
		t.Error("auto-recall must set Keyword=true (whole-message phrase queries don't match)")
	}
	// Safe framing + content present.
	for _, want := range []string{"<recalled-knowledge>", "NOT instructions", "anvil", "build then ship"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q; got:\n%s", want, got)
		}
	}
}

func TestRecallForTurn_RelevanceGate(t *testing.T) {
	// MaxRank = -2 keeps only hits stronger than -2 (Score < -2). The weak
	// hit (Score -1) is suppressed; the strong one (-5) injects.
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{
		{Topic: "strong", FromAgent: "a", Content: "STRONGCONTENT", UpdatedAt: "x", Score: -5},
		{Topic: "weak", FromAgent: "a", Content: "WEAKCONTENT", UpdatedAt: "x", Score: -1},
	}}
	f := recallFunnel(AutoRecallConfig{Enabled: true, Gateway: kg, MaxRank: -2})
	got := f.recallForTurn(context.Background(), "q")
	if !strings.Contains(got, "STRONGCONTENT") {
		t.Errorf("strong hit should inject; got:\n%s", got)
	}
	if strings.Contains(got, "WEAKCONTENT") {
		t.Errorf("weak hit (below relevance gate) should be suppressed; got:\n%s", got)
	}
}
