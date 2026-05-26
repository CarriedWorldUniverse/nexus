package classification

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
)

func twoEvents() []ActivityEvent {
	ts := time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC)
	return []ActivityEvent{
		{TS: ts, Aspect: "anvil", Kind: "turn", Summary: "complete model=deepseek-chat usage_in=1200"},
		{TS: ts.Add(5 * time.Minute), Aspect: "harrow", Kind: "chat", Summary: "from=harrow → operator: NEX-247 plan ready"},
	}
}

// NEX-246 Slice 1: happy path returns the model's markdown verbatim
// alongside deterministic counts. Counts are always populated even
// when the model is involved.
func TestActivitySummary_HappyPath(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: "## Summary\nanvil ran one turn, harrow handed off NEX-247.\n",
	})
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	out, err := a.Summarise(context.Background(), ActivitySummaryInput{
		Events:      twoEvents(),
		WindowStart: time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 5, 25, 14, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if !strings.Contains(out.Markdown, "harrow handed off NEX-247") {
		t.Errorf("markdown should pass through model output: %q", out.Markdown)
	}
	if out.Counts.Total != 2 {
		t.Errorf("Counts.Total = %d, want 2", out.Counts.Total)
	}
	if out.Counts.ByKind["turn"] != 1 || out.Counts.ByKind["chat"] != 1 {
		t.Errorf("Counts.ByKind = %v", out.Counts.ByKind)
	}
	if out.Counts.ByAspect["anvil"] != 1 || out.Counts.ByAspect["harrow"] != 1 {
		t.Errorf("Counts.ByAspect = %v", out.Counts.ByAspect)
	}
}

// NEX-246 Slice 1: empty input is a caller error — the summariser
// would have nothing to summarise + the prompt cost is wasted.
func TestActivitySummary_EmptyInputRejected(t *testing.T) {
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(bridlefake.NewProvider()),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := a.Summarise(context.Background(), ActivitySummaryInput{}); err == nil {
		t.Error("expected error on empty events")
	}
}

// NEX-246 Slice 1: harness error fails open. Counts are deterministic
// so the operator still sees the quantitative shape; markdown gets a
// placeholder. Silent dropping the whole digest is worse.
func TestActivitySummary_HarnessErrorReturnsCountsAndPlaceholder(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{Err: errors.New("upstream timeout")})
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	out, err := a.Summarise(context.Background(), ActivitySummaryInput{
		Events: twoEvents(),
	})
	if err != nil {
		t.Fatalf("Summarise should swallow harness error: %v", err)
	}
	if !strings.Contains(out.Markdown, "summary unavailable") {
		t.Errorf("expected placeholder markdown; got %q", out.Markdown)
	}
	if out.Counts.Total != 2 {
		t.Errorf("counts should be populated despite harness error: %v", out.Counts)
	}
}

// NEX-246 Slice 1: empty model response is treated as unavailable —
// counts still flow but markdown is the placeholder, not "".
func TestActivitySummary_EmptyModelResponsePlaceholder(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{Text: "   "})
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	out, _ := a.Summarise(context.Background(), ActivitySummaryInput{Events: twoEvents()})
	if !strings.Contains(out.Markdown, "summary unavailable") {
		t.Errorf("expected placeholder for empty model response; got %q", out.Markdown)
	}
	if out.Counts.Total != 2 {
		t.Errorf("counts should be populated: %v", out.Counts)
	}
}

// NEX-246 Slice 1: per-call ModelOverride beats env beats default.
// Verified against the request the recording provider saw.
func TestActivitySummary_ModelOverride(t *testing.T) {
	t.Setenv("NEXUS_ACTIVITY_SUMMARY_MODEL", "")
	prov := &recordingProvider{reply: "ok"}
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, err := a.Summarise(context.Background(), ActivitySummaryInput{
		Events:        twoEvents(),
		ModelOverride: "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if got := prov.LastRequest().Model; got != "claude-haiku-4-5" {
		t.Errorf("model on wire = %q, want claude-haiku-4-5", got)
	}
}

// NEX-246 Slice 1: env-var fallback when no per-call override.
func TestActivitySummary_ModelEnvVar(t *testing.T) {
	t.Setenv("NEXUS_ACTIVITY_SUMMARY_MODEL", "deepseek-v4-flash")
	prov := &recordingProvider{reply: "ok"}
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := a.Summarise(context.Background(), ActivitySummaryInput{Events: twoEvents()}); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if got := prov.LastRequest().Model; got != "deepseek-v4-flash" {
		t.Errorf("model on wire = %q, want deepseek-v4-flash", got)
	}
}

// NEX-246 Slice 1: when input exceeds maxActivitySummaryEvents the
// prompt slices to the TAIL (most recent) and the user message
// declares the truncation explicitly. Counts always reflect the full
// input, never the truncated slice.
func TestActivitySummary_EventCapTruncatesTailAndDeclares(t *testing.T) {
	prov := &recordingProvider{reply: "ok"}
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	ts := time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC)
	events := make([]ActivityEvent, maxActivitySummaryEvents+50)
	for i := range events {
		events[i] = ActivityEvent{
			TS: ts.Add(time.Duration(i) * time.Second),
			Aspect: "anvil", Kind: "turn",
			Summary: "event " + strings.Repeat("x", 3),
		}
	}
	out, err := a.Summarise(context.Background(), ActivitySummaryInput{Events: events})
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if out.Counts.Total != len(events) {
		t.Errorf("Counts.Total = %d, want %d (full input)", out.Counts.Total, len(events))
	}
	user := ""
	for _, m := range prov.LastRequest().Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	if !strings.Contains(user, "older 50 truncated") {
		t.Errorf("user message should declare truncation count; got:\n%s", user)
	}
}

// NEX-246 Slice 1: counts aggregation correctness — multi-kind +
// multi-aspect tally.
func TestComputeActivityCounts(t *testing.T) {
	ts := time.Now()
	events := []ActivityEvent{
		{TS: ts, Aspect: "anvil", Kind: "turn"},
		{TS: ts, Aspect: "anvil", Kind: "turn"},
		{TS: ts, Aspect: "harrow", Kind: "turn"},
		{TS: ts, Aspect: "harrow", Kind: "chat"},
		{TS: ts, Aspect: "shadow", Kind: "presence"},
	}
	c := computeActivityCounts(events)
	if c.Total != 5 {
		t.Errorf("Total = %d, want 5", c.Total)
	}
	if c.ByKind["turn"] != 3 || c.ByKind["chat"] != 1 || c.ByKind["presence"] != 1 {
		t.Errorf("ByKind = %v", c.ByKind)
	}
	if c.ByAspect["anvil"] != 2 || c.ByAspect["harrow"] != 2 || c.ByAspect["shadow"] != 1 {
		t.Errorf("ByAspect = %v", c.ByAspect)
	}
}

// NEX-246 Slice 1: joinSortedCounts orders by count desc then key
// asc for stable prompt input — model sees the same shape every run.
func TestJoinSortedCounts_StableOrdering(t *testing.T) {
	got := joinSortedCounts(map[string]int{"chat": 2, "turn": 5, "presence": 2})
	if got != "turn=5 chat=2 presence=2" {
		t.Errorf("joinSortedCounts = %q", got)
	}
}

// NEX-246 Slice 1: window timestamps + counts flow into the user
// message so the model knows what range it's summarising.
func TestActivitySummary_UserMessageIncludesWindowAndCounts(t *testing.T) {
	prov := &recordingProvider{reply: "ok"}
	a := &ActivitySummary{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, err := a.Summarise(context.Background(), ActivitySummaryInput{
		Events:      twoEvents(),
		WindowStart: time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 5, 25, 14, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	user := ""
	for _, m := range prov.LastRequest().Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	for _, want := range []string{
		"WINDOW: 2026-05-25T13:00:00Z → 2026-05-25T14:30:00Z",
		"TOTAL EVENTS: 2",
		"BY KIND:",
		"BY ASPECT:",
		"aspect=anvil",
		"aspect=harrow",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user message missing %q\n---\n%s", want, user)
		}
	}
}
