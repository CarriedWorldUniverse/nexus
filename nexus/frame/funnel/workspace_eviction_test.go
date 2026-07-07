package funnel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// constResultRunner satisfies bridle.ToolRunner, returning the same
// canned result JSON for every call regardless of the ToolCall.
type constResultRunner struct{ result json.RawMessage }

func (r constResultRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return r.result, nil
}

// queuedRunner satisfies bridle.ToolRunner, replaying one result per
// call in order — for tests where consecutive tool calls need
// distinct payloads (e.g. an old small result vs a new large one).
type queuedRunner struct {
	results []json.RawMessage
	pos     int
}

func (r *queuedRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	res := r.results[r.pos]
	r.pos++
	return res, nil
}

// stubPath extracts the workspace file path from an eviction stub's
// Content, e.g. "«result written to /tmp/x/tool-result-000001.txt
// (1 lines); first 1 lines:\n...»".
func stubPath(t *testing.T, stub string) string {
	t.Helper()
	rest := strings.TrimPrefix(stub, evictionStubPrefix)
	if rest == stub {
		t.Fatalf("content is not an eviction stub: %q", stub)
	}
	idx := strings.Index(rest, " (")
	if idx < 0 {
		t.Fatalf("malformed eviction stub, no ' (' marker: %q", stub)
	}
	return rest[:idx]
}

func toolResultEvent(t *testing.T, tail []bridle.SessionEvent) bridle.SessionEvent {
	t.Helper()
	for _, ev := range tail {
		if ev.Role == bridle.RoleTool {
			return ev
		}
	}
	t.Fatal("no tool-result event in session tail")
	return bridle.SessionEvent{}
}

// TestWorkspaceEviction_OversizeResultWrittenToFile drives funnel v2
// §2's first eviction rule: a single tool result over
// FUNNEL_EVICT_RESULT_TOKENS is written to a workspace file, replaced
// in-window with the pointer stub.
func TestWorkspaceEviction_OversizeResultWrittenToFile(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	home := t.TempDir()

	bigResult := strings.Repeat("A", 100_000) // ~25k tokens at chars/4
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "done reading", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
	}}
	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     constResultRunner{result: json.RawMessage(`"` + bigResult + `"`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "read the file"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	tail := f.SessionTail()
	toolEv := toolResultEvent(t, tail)
	if !strings.HasPrefix(toolEv.Content, evictionStubPrefix) {
		t.Fatalf("tool result should have been evicted to a stub, got %d bytes: %.100q", len(toolEv.Content), toolEv.Content)
	}

	path := stubPath(t, toolEv.Content)
	if !strings.HasPrefix(path, filepath.Join(home, ".funnel-workspace")) {
		t.Errorf("evicted file path %q not under expected workspace dir", path)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("workspace file not written: %v", err)
	}
	// The runner's result JSON is `"` + bigResult + `"` (a JSON string
	// literal) — Content is the raw provider-message text, quotes and all.
	if !strings.Contains(string(written), bigResult) {
		t.Error("workspace file does not contain the original oversize result")
	}
}

// TestWorkspaceEviction_CustomThreshold verifies that
// FUNNEL_EVICT_RESULT_TOKENS overrides the default 20k threshold: a
// result small enough to survive the default threshold is evicted when
// the threshold is lowered below its size.
func TestWorkspaceEviction_CustomThreshold(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "500") // override: evict > 500 tokens (~2000 chars)
	home := t.TempDir()

	// ~3k chars = ~750 estimated tokens — under the 20k default but over the 500-token custom bar.
	medResult := strings.Repeat("M", 3_000)
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "done", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
	}}
	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     constResultRunner{result: json.RawMessage(`"` + medResult + `"`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "read"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	toolEv := toolResultEvent(t, f.SessionTail())
	if !strings.HasPrefix(toolEv.Content, evictionStubPrefix) {
		t.Fatalf("result should have been evicted at the 500-token custom threshold, got %.80q", toolEv.Content)
	}
}

// TestWorkspaceEviction_DisabledByDefault verifies the eviction rollout
// posture (FUNNEL-V2-DESIGN.md "Rollout": env-gated, off unless
// explicitly flipped on) — with no env var set, a huge tool result
// stays in-window untouched and no workspace file is written.
func TestWorkspaceEviction_DisabledByDefault(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "")
	home := t.TempDir()

	bigResult := strings.Repeat("A", 100_000)
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "done reading", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
	}}
	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     constResultRunner{result: json.RawMessage(`"` + bigResult + `"`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "read the file"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	toolEv := toolResultEvent(t, f.SessionTail())
	if strings.HasPrefix(toolEv.Content, evictionStubPrefix) {
		t.Fatal("eviction ran despite FUNNEL_WORKSPACE_EVICT being unset")
	}
	if !strings.Contains(toolEv.Content, bigResult) {
		t.Error("tool result content was altered despite eviction being disabled")
	}
	if _, err := os.Stat(filepath.Join(home, ".funnel-workspace")); !os.IsNotExist(err) {
		t.Errorf("workspace dir should not exist when eviction is disabled, stat err=%v", err)
	}
}

// TestWorkspaceEviction_ContextPressureSweep_OldestFirstBatched drives
// funnel v2 §2's second eviction rule: bridle's PromptBudget check
// (wired to workspaceEviction.sweepBudgetTokens()) warns mid-turn-2
// once the newly-appended tool result pushes the assembled prompt over
// budget; commitTurnState's sweep then evicts the OLDEST tool result
// first and stops as soon as the tail is back under budget — proving
// both "oldest first" and "batched" (it must NOT also evict turn 2's
// own much-larger result once the budget is already satisfied).
func TestWorkspaceEviction_ContextPressureSweep_OldestFirstBatched(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	home := t.TempDir()

	oldResult := strings.Repeat("B", 12_000) // ~3k tokens — turn 1's result
	newResult := strings.Repeat("C", 24_000) // ~6k tokens — turn 2's result
	runner := &queuedRunner{results: []json.RawMessage{
		json.RawMessage(`"` + oldResult + `"`),
		json.RawMessage(`"` + newResult + `"`),
	}}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// Turn 1: one tool call, then done. Small usage so cumulative
		// tokens never approach the (default) compaction threshold —
		// this test isolates the sweep, not compaction.
		{ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "turn 1 done", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
		// Turn 2: one tool call whose result is what pushes the
		// assembled prompt over budget, then done.
		{ToolCalls: []bridle.ToolInvocation{{ID: "t2", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "turn 2 done", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
	}}
	f, err := New(Config{
		AspectID:            "frame",
		AspectHome:          home,
		Harness:             bridle.NewHarness(prov),
		Provider:            "scripted",
		Model:               "m",
		Runner:              runner,
		ContextWindowTokens: 10_000, // budget = 85% = 8_500 tokens
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "first"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// Turn 1 alone (~3k tokens) must stay well under the 8.5k budget —
	// no premature sweep.
	preTurn2 := f.SessionTail()
	if strings.HasPrefix(toolResultEvent(t, preTurn2).Content, evictionStubPrefix) {
		t.Fatal("turn 1's result was evicted before any pressure existed")
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "second"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	tail := f.SessionTail()
	var toolEvents []bridle.SessionEvent
	for _, ev := range tail {
		if ev.Role == bridle.RoleTool {
			toolEvents = append(toolEvents, ev)
		}
	}
	if len(toolEvents) != 2 {
		t.Fatalf("expected 2 tool-result events in tail, got %d: %+v", len(toolEvents), toolEvents)
	}

	// Oldest (turn 1's) result must be evicted...
	if !strings.HasPrefix(toolEvents[0].Content, evictionStubPrefix) {
		t.Errorf("oldest tool result should have been swept to a stub, got %.80q", toolEvents[0].Content)
	}
	// ...but turn 2's own (larger) result must survive untouched —
	// evicting the single oldest entry already satisfies the budget,
	// so the batched sweep must stop there rather than over-evicting.
	if toolEvents[1].Content != string(json.RawMessage(`"`+newResult+`"`)) {
		t.Errorf("newest tool result should NOT have been evicted (batched sweep over-evicted), got %.80q", toolEvents[1].Content)
	}
}

// TestCommitTurnState_EvictionOrderedBeforeCompaction proves the
// design doc's resolved "ordering vs compaction" open question: a
// compaction rotate must never discard a freshly-written eviction
// pointer. Turn 1 both evicts an oversize result AND crosses the
// compaction threshold; turn 2's maybeCompact runs before turn 2's own
// main call and must see the STUB in the tail it hands to the
// summarize call, not the raw (already-evicted) content.
func TestCommitTurnState_EvictionOrderedBeforeCompaction(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "50") // low bar: evict readily
	home := t.TempDir()

	bigResult := strings.Repeat("A", 2_000) // ~500 tokens, over the 50-token bar
	prov := &recordingScriptedProvider{results: []bridle.ProviderResult{
		// Turn 1: tool call (evicted post-commit) + usage that crosses
		// the compaction threshold.
		{ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{FinalText: "turn 1 done", Usage: bridle.Usage{InputTokens: 800, OutputTokens: 300}},
		// Turn 2's compaction summarize call.
		{FinalText: "compact briefing", Usage: bridle.Usage{InputTokens: 50, OutputTokens: 50}},
		// Turn 2's main call, run against the post-compaction tail.
		{FinalText: "turn 2 done", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 10}},
	}}
	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     constResultRunner{result: json.RawMessage(`"` + bigResult + `"`)},
		Compaction: CompactionPolicy{ThresholdTokens: 1_000, MaxSummaryTokens: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "first"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// Confirm the eviction actually happened before turn 2 (otherwise
	// the ordering assertion below is vacuous).
	if !strings.HasPrefix(toolResultEvent(t, f.SessionTail()).Content, evictionStubPrefix) {
		t.Fatal("turn 1's oversize result should have been evicted already")
	}

	f.Receive(bridle.InboxItem{From: "operator", Content: "second"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(prov.reqs) != 4 {
		t.Fatalf("expected 4 provider calls (turn1 x2, compact summarize, turn2 main), got %d", len(prov.reqs))
	}

	// prov.reqs[2] is the compact summarize call — its lowered message
	// list must carry the stub, never the raw oversize content.
	summarizeReq := prov.reqs[2]
	var sawStub, sawRaw bool
	for _, m := range summarizeReq.Messages {
		if strings.HasPrefix(m.Content, evictionStubPrefix) {
			sawStub = true
		}
		if strings.Contains(m.Content, bigResult) {
			sawRaw = true
		}
	}
	if !sawStub {
		t.Error("compaction's summarize call should have seen the eviction stub in the tail")
	}
	if sawRaw {
		t.Error("compaction's summarize call saw the raw oversize content — the rotate discarded (or raced past) the fresh eviction pointer")
	}
}

// recordingScriptedProvider is scriptedProvider plus a record of every
// ProviderRequest it saw, in order — needed to inspect an
// intermediate call (the compact summarize call) rather than just the
// last one.
type recordingScriptedProvider struct {
	results []bridle.ProviderResult
	pos     int
	reqs    []bridle.ProviderRequest
}

func (p *recordingScriptedProvider) Name() bridle.ProviderID { return "scripted" }

func (p *recordingScriptedProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

func (p *recordingScriptedProvider) RunTurn(_ context.Context, req bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	p.reqs = append(p.reqs, req)
	if p.pos >= len(p.results) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	r := p.results[p.pos]
	p.pos++
	if r.StopReason == "" {
		r.StopReason = bridle.StopReasonModelDone
	}
	return r, nil
}
