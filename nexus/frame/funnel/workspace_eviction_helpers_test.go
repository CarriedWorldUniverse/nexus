package funnel

import (
	"strings"
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// --- Helper unit tests ------------------------------------------------------

func TestIsEvictionStub_MatchesPrefix(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"\u00abresult written to /tmp/x/foo.txt (10 lines); first 5 lines:\n...\u00bb", true},
		{"\u00abresult written to /some/path (0 lines); first 0 lines:\n\u00bb", true},
		{"some normal tool output", false},
		{"\u00abresult written to \u00bb", true},
		{"", false},
		{"\u00abresult not what we expected\u00bb", false},
	}
	for _, tc := range cases {
		got := isEvictionStub(tc.content)
		if got != tc.want {
			t.Errorf("isEvictionStub(%q) = %v; want %v", tc.content, got, tc.want)
		}
	}
}

func TestRenderEvictionStub_Formatting(t *testing.T) {
	path := "/workspace/tool-result-000001.txt"
	lines := []string{"first line", "second line"}
	stub := renderEvictionStub(path, 100, lines)

	if !strings.HasPrefix(stub, evictionStubPrefix) {
		t.Errorf("stub should start with prefix %q; got %q", evictionStubPrefix, stub)
	}
	if !strings.Contains(stub, path) {
		t.Errorf("stub should contain the workspace file path: %q", stub)
	}
	if !strings.Contains(stub, "100 lines") {
		t.Errorf("stub should report the line count: %q", stub)
	}
	if !strings.Contains(stub, "first line") || !strings.Contains(stub, "second line") {
		t.Errorf("stub should contain preview lines: %q", stub)
	}
	if !strings.HasSuffix(stub, "\u00bb") {
		t.Errorf("stub should end with the closing guillemet: %q", stub)
	}
	if !isEvictionStub(stub) {
		t.Error("renderEvictionStub output should satisfy isEvictionStub")
	}
}

func TestTruncateEvictionPreviewLine_ShortLineUnchanged(t *testing.T) {
	short := "short line"
	if got := truncateEvictionPreviewLine(short); got != short {
		t.Errorf("short line: got %q; want %q", got, short)
	}
}

func TestTruncateEvictionPreviewLine_AtBoundary(t *testing.T) {
	exact := strings.Repeat("x", evictionPreviewLineCap)
	if got := truncateEvictionPreviewLine(exact); got != exact {
		t.Errorf("line at boundary: len=%d; got len=%d", len(exact), len(got))
	}
}

func TestTruncateEvictionPreviewLine_LongLineTruncated(t *testing.T) {
	long := strings.Repeat("x", evictionPreviewLineCap+50)
	got := truncateEvictionPreviewLine(long)
	if len(got) != evictionPreviewLineCap+3 { // +3 for ellipsis "…" (3 bytes UTF-8)
		t.Errorf("truncated line len=%d; want %d (cap + ellipsis)", len(got), evictionPreviewLineCap+3)
	}
	_ = strings.HasSuffix(got, "…")
	if !strings.HasPrefix(got, strings.Repeat("x", evictionPreviewLineCap)) {
		t.Error("truncated line should keep first evictionPreviewLineCap chars")
	}
}

// --- resolveWorkspaceEviction unit tests ------------------------------------

func TestResolveWorkspaceEviction_Defaults(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "")
	t.Setenv("FUNNEL_CTX_SWEEP_PCT", "")
	c := resolveWorkspaceEviction(0)
	if c.enabled {
		t.Error("eviction should be disabled by default")
	}
	if c.resultTokenThreshold != defaultEvictResultTokens {
		t.Errorf("resultTokenThreshold = %d; want %d", c.resultTokenThreshold, defaultEvictResultTokens)
	}
	if c.sweepPercent != defaultCtxSweepPercent {
		t.Errorf("sweepPercent = %d; want %d", c.sweepPercent, defaultCtxSweepPercent)
	}
	if c.windowTokens != defaultContextWindowTokens {
		t.Errorf("windowTokens = %d; want %d", c.windowTokens, defaultContextWindowTokens)
	}
}

func TestResolveWorkspaceEviction_Enabled(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	c := resolveWorkspaceEviction(150_000)
	if !c.enabled {
		t.Error("eviction should be enabled when FUNNEL_WORKSPACE_EVICT=1")
	}
	if c.windowTokens != 150_000 {
		t.Errorf("windowTokens = %d; want 150000 (from param)", c.windowTokens)
	}
}

func TestResolveWorkspaceEviction_EnvOverrides(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "5000")
	t.Setenv("FUNNEL_CTX_SWEEP_PCT", "90")
	c := resolveWorkspaceEviction(100_000)
	if c.resultTokenThreshold != 5000 {
		t.Errorf("resultTokenThreshold = %d; want 5000", c.resultTokenThreshold)
	}
	if c.sweepPercent != 90 {
		t.Errorf("sweepPercent = %d; want 90", c.sweepPercent)
	}
}

func TestResolveWorkspaceEviction_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "not-a-number")
	t.Setenv("FUNNEL_CTX_SWEEP_PCT", "0")
	c := resolveWorkspaceEviction(0)
	if c.resultTokenThreshold != defaultEvictResultTokens {
		t.Errorf("invalid EVICT_RESULT_TOKENS should fall back to default %d; got %d",
			defaultEvictResultTokens, c.resultTokenThreshold)
	}
	if c.sweepPercent != defaultCtxSweepPercent {
		t.Errorf("invalid CTX_SWEEP_PCT should fall back to default %d; got %d",
			defaultCtxSweepPercent, c.sweepPercent)
	}
}

// --- sweepBudgetTokens unit tests -------------------------------------------

func TestSweepBudgetTokens_DisabledReturnsZero(t *testing.T) {
	c := workspaceEvictionConfig{enabled: false, windowTokens: 100_000, sweepPercent: 85}
	if got := c.sweepBudgetTokens(); got != 0 {
		t.Errorf("sweepBudgetTokens when disabled = %d; want 0", got)
	}
}

func TestSweepBudgetTokens_ComputesCorrectBudget(t *testing.T) {
	c := workspaceEvictionConfig{enabled: true, windowTokens: 100_000, sweepPercent: 85}
	if got := c.sweepBudgetTokens(); got != 85_000 {
		t.Errorf("sweepBudgetTokens = %d; want 85000", got)
	}
}

// --- workspaceDir unit tests -------------------------------------------------

func TestWorkspaceDir_CustomDir(t *testing.T) {
	f := &Funnel{cfg: Config{WorkspaceDir: "/custom/path"}}
	if got := f.workspaceDir(); got != "/custom/path" {
		t.Errorf("workspaceDir = %q; want %q", got, "/custom/path")
	}
}

func TestWorkspaceDir_DefaultFromAspectHome(t *testing.T) {
	f := &Funnel{cfg: Config{AspectHome: "/home/test"}}
	got := f.workspaceDir()
	want := "/home/test/.funnel-workspace"
	if got != want {
		t.Errorf("workspaceDir = %q; want %q", got, want)
	}
}

// --- contextBudgetSink unit tests --------------------------------------------

type recordingSinkForBudget struct {
	mu     sync.Mutex
	events []bridle.Event
}

func (s *recordingSinkForBudget) Emit(ev bridle.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func TestContextBudgetSink_LatchesBudgetWarning(t *testing.T) {
	inner := &recordingSinkForBudget{}
	sink := &contextBudgetSink{inner: inner}

	if sink.saw.Load() {
		t.Error("saw should be false before any event")
	}

	sink.Emit(bridle.ModelChunk{Text: "hello"})
	if sink.saw.Load() {
		t.Error("saw should still be false after a ModelChunk event")
	}

	sink.Emit(bridle.ContextBudgetWarning{})
	if !sink.saw.Load() {
		t.Error("saw should be true after ContextBudgetWarning")
	}

	sink.Emit(bridle.TurnDone{})
	if !sink.saw.Load() {
		t.Error("saw should stay true after subsequent events")
	}

	inner.mu.Lock()
	count := len(inner.events)
	inner.mu.Unlock()
	if count != 3 {
		t.Errorf("inner sink received %d events; want 3", count)
	}
}

// --- Edge case: non-tool events never evicted -------------------------------

func TestEvictOversizeResults_NonToolEventsSkipped(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	t.Setenv("FUNNEL_EVICT_RESULT_TOKENS", "50")
	home := t.TempDir()

	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(&scriptedProvider{}),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	bigContent := strings.Repeat("A", 10_000)

	f.mu.Lock()
	f.sessionTail = []bridle.SessionEvent{
		{Role: "assistant", Content: bigContent},
		{Role: "user", Content: bigContent},
		{Role: "system", Content: bigContent},
		{Role: "tool", Content: bigContent},
	}
	f.mu.Unlock()

	f.evictOversizeResults(4)

	tail := f.SessionTail()
	if len(tail) != 4 {
		t.Fatalf("expected 4 tail entries, got %d", len(tail))
	}

	// First three must not be evicted
	for i, role := range []string{"assistant", "user", "system"} {
		if isEvictionStub(tail[i].Content) {
			t.Errorf("entry %d (%s) was incorrectly evicted", i, role)
		}
	}

	// Tool result must be evicted
	if !isEvictionStub(tail[3].Content) {
		t.Errorf("tool result entry should have been evicted; got %.40q", tail[3].Content)
	}
}

// TestSweepContextPressure_NoToolEvents_ReturnsQuietly verifies that
// sweepContextPressure exits without error when the tail has no tool results.
func TestSweepContextPressure_NoToolEvents_ReturnsQuietly(t *testing.T) {
	t.Setenv("FUNNEL_WORKSPACE_EVICT", "1")
	home := t.TempDir()

	f, err := New(Config{
		AspectID:   "frame",
		AspectHome: home,
		Harness:    bridle.NewHarness(&scriptedProvider{}),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.sessionTail = []bridle.SessionEvent{
		{Role: "assistant", Content: strings.Repeat("A", 50_000)},
		{Role: "user", Content: strings.Repeat("B", 50_000)},
	}
	// Set a low budget so the tail is over budget but only non-tool events exist.
	f.workspaceEviction = workspaceEvictionConfig{
		enabled:              true,
		resultTokenThreshold: 20000,
		sweepPercent:         50,
		windowTokens:         10_000,
	}
	f.mu.Unlock()

	// Should not panic, spin, or block.
	f.sweepContextPressure()
}
