package compactor

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/context/tree"
	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
)

// mockProvider stands in for a real Provider in tests. Only the
// methods the compactor exercises are implemented; the rest panic.
type mockProvider struct {
	tokenCount    int
	tokenCountErr error
	compactResult providers.CompactionResult
	compactErr    error
	compactCalls  int
	seenEntries   []providers.Entry
}

func (m *mockProvider) Invoke(context.Context, providers.InvokeRequest) (providers.InvokeResult, error) {
	panic("not used")
}
func (m *mockProvider) Stream(context.Context, providers.InvokeRequest) (providers.StreamIterator, error) {
	panic("not used")
}
func (m *mockProvider) TokenCount(_ context.Context, _ string, _ string) (int, error) {
	return m.tokenCount, m.tokenCountErr
}
func (m *mockProvider) Compact(_ context.Context, entries []providers.Entry, _ string) (providers.CompactionResult, error) {
	m.compactCalls++
	m.seenEntries = entries
	return m.compactResult, m.compactErr
}
func (m *mockProvider) Embed(context.Context, providers.EmbedRequest) (providers.EmbedResult, error) {
	return providers.EmbedResult{}, providers.ErrUnsupported
}
func (m *mockProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Chat: true, MaxContextTokens: 200_000}
}
func (m *mockProvider) Models(context.Context) ([]providers.Model, error) {
	return nil, nil
}
func (m *mockProvider) TriageModel() string { return "mock-triage" }

func openTestTree(t *testing.T) *tree.Tree {
	t.Helper()
	tr, err := tree.Open(t.TempDir(), "compact-session")
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestShouldCompactFormula(t *testing.T) {
	cases := []struct {
		name      string
		tokens    int
		policy    Policy
		want      bool
	}{
		{"under threshold", 100, Policy{Window: 1000, Reserve: 100}, false},
		{"at threshold", 900, Policy{Window: 1000, Reserve: 100}, false},
		{"over threshold", 901, Policy{Window: 1000, Reserve: 100}, true},
		{"zero window", 500, Policy{Window: 0}, false},
		{"default reserve (20% of 100k = 20k)", 81_000, Policy{Window: 100_000}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldCompact(tc.tokens, tc.policy); got != tc.want {
				t.Errorf("ShouldCompact(%d, %+v) = %v, want %v", tc.tokens, tc.policy, got, tc.want)
			}
		})
	}
}

func TestRunSkipsShortBranch(t *testing.T) {
	tr := openTestTree(t)
	ctx := context.Background()
	// Only 3 entries — below default KeepRecentTurns=6.
	for i := 0; i < 3; i++ {
		tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "x"}})
	}

	m := &mockProvider{tokenCount: 999_999}
	res, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedReason == "" {
		t.Error("expected SkippedReason")
	}
	if m.compactCalls != 0 {
		t.Error("provider.Compact should not have been called")
	}
}

func TestRunSkipsUnderThreshold(t *testing.T) {
	tr := openTestTree(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "x"}})
	}

	m := &mockProvider{tokenCount: 500}
	res, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedReason == "" {
		t.Error("expected SkippedReason for under-threshold")
	}
	if m.compactCalls != 0 {
		t.Error("Compact called when under threshold")
	}
	if res.TokensBefore != 500 {
		t.Errorf("TokensBefore = %d, want 500", res.TokensBefore)
	}
}

func TestRunCompactsAndAdvancesHead(t *testing.T) {
	tr := openTestTree(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 10; i++ {
		e, _ := tr.Append(ctx, tree.Entry{
			Kind:    tree.KindTurnUser,
			Payload: map[string]any{"text": "turn"},
		})
		ids = append(ids, e.ID)
	}

	m := &mockProvider{
		tokenCount: 950,
		compactResult: providers.CompactionResult{
			Summary:      "concise summary of older turns",
			Model:        "mock-triage",
			TokensBefore: 900,
			TokensAfter:  120,
		},
	}

	res, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100, KeepRecentTurns: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.compactCalls != 1 {
		t.Errorf("Compact calls = %d, want 1", m.compactCalls)
	}
	if res.SkippedReason != "" {
		t.Errorf("expected no skip, got: %s", res.SkippedReason)
	}
	// 10 entries total, keep 3 → 7 summarised.
	if len(m.seenEntries) != 7 {
		t.Errorf("summariser saw %d entries, want 7", len(m.seenEntries))
	}
	if res.FirstKeptEntryID != ids[7] {
		t.Errorf("FirstKeptEntryID = %s, want ids[7]=%s", res.FirstKeptEntryID, ids[7])
	}
	if res.CompactionEntry.Kind != tree.KindCompaction {
		t.Errorf("CompactionEntry kind = %q, want %q", res.CompactionEntry.Kind, tree.KindCompaction)
	}

	// Head should now point at the compaction entry.
	head, _ := tr.Head()
	if head != res.CompactionEntry.ID {
		t.Errorf("head = %s, want compaction entry %s", head, res.CompactionEntry.ID)
	}

	// The compaction entry's payload should carry the summary fields.
	payload := res.CompactionEntry.Payload
	if payload["firstKeptEntryId"] != ids[7] {
		t.Errorf("payload.firstKeptEntryId = %v, want %s", payload["firstKeptEntryId"], ids[7])
	}
	if payload["summary"] != "concise summary of older turns" {
		t.Errorf("payload.summary = %v", payload["summary"])
	}
}

func TestRunPreservesActiveSystemPrompt(t *testing.T) {
	tr := openTestTree(t)
	ctx := context.Background()

	// Layout: user, user, [stale system.prompt], user, user, user,
	// [active system.prompt], user, user, user, user, user, user, user
	// Default keep=6 would cut in the middle of the later turns; the
	// ACTIVE (latest) system prompt must be preserved, the STALE one
	// summarised with the rest of the early turns.
	tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "t0"}})
	tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "t1"}})
	staleSP, _ := tr.Append(ctx, tree.Entry{Kind: tree.KindSystemPrompt, Payload: map[string]any{"text": "stale prompt"}})
	tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "t2"}})
	tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "t3"}})
	tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "t4"}})
	activeSP, _ := tr.Append(ctx, tree.Entry{Kind: tree.KindSystemPrompt, Payload: map[string]any{"text": "active prompt"}})
	for i := 0; i < 7; i++ {
		tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "later"}})
	}

	m := &mockProvider{
		tokenCount:    999,
		compactResult: providers.CompactionResult{Summary: "sum", Model: "mock"},
	}
	res, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100, KeepRecentTurns: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstKeptEntryID != activeSP.ID {
		t.Errorf("FirstKeptEntryID = %s, want the ACTIVE system prompt (%s). Stale was %s",
			res.FirstKeptEntryID, activeSP.ID, staleSP.ID)
	}
	// Summariser should see everything before the active prompt —
	// including the stale system prompt.
	seenStale := false
	for _, e := range m.seenEntries {
		if e.ID == staleSP.ID {
			seenStale = true
		}
		if e.ID == activeSP.ID {
			t.Error("active prompt leaked into summariser input")
		}
	}
	if !seenStale {
		t.Error("stale prompt should have been included in summarised set")
	}
}

func TestRunProviderErrors(t *testing.T) {
	tr := openTestTree(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		tr.Append(ctx, tree.Entry{Kind: tree.KindTurnUser, Payload: map[string]any{"text": "x"}})
	}

	t.Run("token count error", func(t *testing.T) {
		m := &mockProvider{tokenCountErr: errors.New("upstream died")}
		_, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100})
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("compact error", func(t *testing.T) {
		m := &mockProvider{
			tokenCount: 999,
			compactErr: errors.New("summariser failed"),
		}
		_, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100, KeepRecentTurns: 3})
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("empty summary rejected", func(t *testing.T) {
		m := &mockProvider{
			tokenCount:    999,
			compactResult: providers.CompactionResult{Summary: "   "},
		}
		_, err := Run(ctx, tr, m, Policy{Window: 1000, Reserve: 100, KeepRecentTurns: 3})
		if err == nil {
			t.Error("expected error for empty summary")
		}
	})
}

func TestRunNilArgs(t *testing.T) {
	ctx := context.Background()
	if _, err := Run(ctx, nil, &mockProvider{}, Policy{}); err == nil {
		t.Error("expected error for nil tree")
	}
	tr := openTestTree(t)
	if _, err := Run(ctx, tr, nil, Policy{}); err == nil {
		t.Error("expected error for nil provider")
	}
}

func TestPolicyDefaults(t *testing.T) {
	// window of 100k → reserve defaults to max(20k, 20% window) = 20k
	p := Policy{Window: 100_000}
	if got := p.effectiveReserve(); got != 20_000 {
		t.Errorf("effectiveReserve(100k window) = %d, want 20000", got)
	}
	// tiny window → reserve floors at 10k
	small := Policy{Window: 20_000}
	if got := small.effectiveReserve(); got != 10_000 {
		t.Errorf("effectiveReserve(20k window) = %d, want 10000", got)
	}
	// default keep
	if got := (Policy{}).effectiveKeep(); got != 6 {
		t.Errorf("effectiveKeep() = %d, want 6", got)
	}
}
