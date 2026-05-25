package funnel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

func TestAlwaysPostFilter_PassesNonEmpty(t *testing.T) {
	d := AlwaysPostFilter{}.Judge(context.Background(), FilterInput{
		FinalText: "hello operator",
	})
	if !d.ShouldPost {
		t.Errorf("non-empty reply should post: got %+v", d)
	}
	if d.Reason != "" {
		t.Errorf("reason for accepted post should be empty: got %q", d.Reason)
	}
}

func TestAlwaysPostFilter_SuppressesEmpty(t *testing.T) {
	cases := []string{"", "   ", "\n\t  ", "\n"}
	for _, in := range cases {
		d := AlwaysPostFilter{}.Judge(context.Background(), FilterInput{FinalText: in})
		if d.ShouldPost {
			t.Errorf("empty reply %q should suppress", in)
		}
		if d.Reason != FilterReasonEmpty {
			t.Errorf("reason: got %q, want %q", d.Reason, FilterReasonEmpty)
		}
	}
}

func TestHardRulesFilter_CatchesSelfSuppress(t *testing.T) {
	cases := []string{
		"I don't have anything to add to this thread.",
		"Sorry, this isn't for me.",
		"I'll stay quiet here.",
		"this message isn't addressed to me, ignoring",
		"Nothing to add here, frankly.",
	}
	f := HardRulesFilter{}
	for _, in := range cases {
		d := f.Judge(context.Background(), FilterInput{FinalText: in})
		if d.ShouldPost {
			t.Errorf("expected suppress for self-suppress phrase %q, got post", in)
		}
		if d.Reason != FilterReasonSelfSuppress {
			t.Errorf("reason for %q: got %q, want %q", in, d.Reason, FilterReasonSelfSuppress)
		}
	}
}

func TestHardRulesFilter_AllowsSubstantiveReply(t *testing.T) {
	cases := []string{
		"Looking at the auth module now, will report back.",
		"I'll check the database and confirm.",
		"@operator the migration completed at 2026-05-02T05:30Z.",
		"Found three failing tests in the route package; running them under -race.",
		// Anchored phrases: "I'll stay quiet" / "nothing to add here"
		// are self-suppress only at the START of a reply. In the
		// middle of a substantive reply they're real content and
		// must NOT be suppressed.
		"After running this audit I'll stay quiet on the security implications until we discuss.",
		"Plenty to discuss, though there's nothing to add here about the migration since it landed clean.",
	}
	f := HardRulesFilter{}
	for _, in := range cases {
		d := f.Judge(context.Background(), FilterInput{FinalText: in})
		if !d.ShouldPost {
			t.Errorf("substantive reply %q should post: got %+v", in, d)
		}
	}
}

func TestHardRulesFilter_DefersToInner(t *testing.T) {
	called := false
	inner := stubFilter(func(_ FilterInput) FilterDecision {
		called = true
		return FilterDecision{ShouldPost: false, Reason: "inner_decided"}
	})
	f := HardRulesFilter{Inner: inner}
	d := f.Judge(context.Background(), FilterInput{FinalText: "Looking into it now."})
	if !called {
		t.Error("inner filter should have been consulted for non-self-suppress reply")
	}
	if d.ShouldPost {
		t.Errorf("inner returned suppress, outer should reflect: got %+v", d)
	}
	if d.Reason != "inner_decided" {
		t.Errorf("reason should propagate from inner: got %q", d.Reason)
	}
}

func TestHardRulesFilter_DefaultInnerIsAlwaysPost(t *testing.T) {
	f := HardRulesFilter{} // no Inner set
	d := f.Judge(context.Background(), FilterInput{FinalText: "hello"})
	if !d.ShouldPost {
		t.Errorf("default inner should AlwaysPost on substantive: got %+v", d)
	}
}

func TestCheapModelFilter_FailsOpenOnMisconfigured(t *testing.T) {
	// Harness/Provider/Model all empty — should fail open with
	// ShouldPost=true rather than blocking real content.
	d := (&CheapModelFilter{}).Judge(context.Background(), FilterInput{FinalText: "real reply"})
	if !d.ShouldPost {
		t.Errorf("misconfigured filter should fail open: got %+v", d)
	}
}

func TestCheapModelFilter_SuppressesEmpty(t *testing.T) {
	d := (&CheapModelFilter{}).Judge(context.Background(), FilterInput{FinalText: "  "})
	if d.ShouldPost {
		t.Errorf("empty input always suppressed: got %+v", d)
	}
}

func TestCheapModelFilter_ParsesJSON(t *testing.T) {
	tests := []struct {
		name      string
		modelText string
		want      bool
	}{
		{"clean post true", `{"post": true, "reason": "ok"}`, true},
		{"clean post false", `{"post": false, "reason": "scratch"}`, false},
		{"fenced post false", "```json\n{\"post\": false, \"reason\": \"empty\"}\n```", false},
		{"prose before json", `My verdict: {"post": true, "reason": "useful"}`, true},
		{"unparseable falls open", "maybe?", true},
		{"empty model output falls open", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedProvider{results: []bridle.ProviderResult{
				{
					FinalText:  tc.modelText,
					Usage:      bridle.Usage{InputTokens: 5, OutputTokens: 1},
					StopReason: bridle.StopReasonModelDone,
				},
			}}
			f := &CheapModelFilter{
				Harness:  bridle.NewHarness(prov),
				Provider: "scripted",
				Model:    "judge",
			}
			d := f.Judge(context.Background(), FilterInput{
				FinalText: "ambiguous reply text",
				AspectID:  "frame",
				TurnID:    "turn-test",
			})
			if d.ShouldPost != tc.want {
				t.Errorf("model said %q: got ShouldPost=%v, want %v", tc.modelText, d.ShouldPost, tc.want)
			}
		})
	}
}

func TestCheapModelFilter_FailsOpenOnProviderError(t *testing.T) {
	f := &CheapModelFilter{
		Harness:  bridle.NewHarness(erroringProvider{err: context.DeadlineExceeded}),
		Provider: "erroring",
		Model:    "m",
	}
	d := f.Judge(context.Background(), FilterInput{FinalText: "real reply", TurnID: "t1"})
	if !d.ShouldPost {
		t.Errorf("first judge error should fail open (post): got %+v", d)
	}
}

// stubFilter is a function-typed OutputFilter for tests.
type stubFilter func(in FilterInput) FilterDecision

func (s stubFilter) Judge(_ context.Context, in FilterInput) FilterDecision { return s(in) }

func TestFunnel_FilterDecisionSurfacedInResult(t *testing.T) {
	suppress := stubFilter(func(_ FilterInput) FilterDecision {
		return FilterDecision{ShouldPost: false, Reason: FilterReasonScratch}
	})
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "reply text", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Filter:   suppress,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Deliberate(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}
	if res.Filter.ShouldPost {
		t.Error("filter said suppress, result should reflect")
	}
	if res.Filter.Reason != FilterReasonScratch {
		t.Errorf("reason: got %q, want %q", res.Filter.Reason, FilterReasonScratch)
	}
	if res.TurnResult.FinalText != "reply text" {
		t.Errorf("turn result text preserved: got %q", res.TurnResult.FinalText)
	}
}

func TestFunnel_DefaultFilterIsAlwaysPost(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		// Filter intentionally unset
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := f.Deliberate(context.Background(), "ping")
	if !res.Filter.ShouldPost {
		t.Errorf("default filter should AlwaysPost non-empty: got %+v", res.Filter)
	}
}

func TestFunnel_FilterRunsAfterTurnEndEvent(t *testing.T) {
	sink := &recordingSink{}
	filterRanAfter := false
	tracking := stubFilter(func(_ FilterInput) FilterDecision {
		// At Judge time, both turn.start and turn.end should already
		// be in the sink — emit ordering invariant for Lock 5 + F1.1.
		seen := map[EventType]bool{}
		for _, e := range sink.snapshot() {
			seen[e.Type] = true
		}
		if seen[EventTurnStart] && seen[EventTurnEnd] {
			filterRanAfter = true
		}
		return FilterDecision{ShouldPost: true}
	})
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Events:   sink,
		Filter:   tracking,
	})
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if !filterRanAfter {
		t.Error("filter should run after turn.end has fired")
	}
}

func TestFunnel_FilterJudgingEventCarriesTurnID(t *testing.T) {
	sink := &recordingSink{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Events:   sink,
	})
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	var startID, filterID string
	for _, e := range sink.snapshot() {
		switch p := e.Payload.(type) {
		case TurnStartPayload:
			startID = p.TurnID
		case FilterJudgingPayload:
			filterID = p.TurnID
		}
	}
	if startID == "" || filterID == "" {
		t.Fatalf("missing event ids: start=%q filter=%q", startID, filterID)
	}
	if startID != filterID {
		t.Errorf("filter event should share turn_id with start: start=%q filter=%q", startID, filterID)
	}
}

// slow filter verifies the funnel doesn't wait synchronously on a
// hung filter — the funnel respects ctx in Judge because Lock 5
// applies to filter calls too.
type slowFilter struct {
	delay time.Duration
}

func (s slowFilter) Judge(ctx context.Context, _ FilterInput) FilterDecision {
	select {
	case <-time.After(s.delay):
		return FilterDecision{ShouldPost: true}
	case <-ctx.Done():
		return FilterDecision{ShouldPost: true} // fail open on cancel
	}
}

func TestFunnel_FilterRespectsContextCancellation(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Filter:   slowFilter{delay: 5 * time.Second},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = f.Deliberate(ctx, "ping")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliberate didn't return when ctx expired — filter not honoring cancellation")
	}
}

func TestParseJudgeJSON(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantPost   bool
		wantReason string
		wantClass  string
		wantErr    bool
	}{
		// Legacy binary format — backward compat.
		{"legacy post true", `{"post": true, "reason": "substantive"}`, true, "substantive", FilterClassComplete, false},
		{"legacy post false", `{"post": false, "reason": "self-suppress"}`, false, "self-suppress", FilterClassScratch, false},
		{"legacy empty reason suppress", `{"post": false, "reason": ""}`, false, FilterReasonScratch, FilterClassScratch, false},
		{"legacy empty reason post", `{"post": true, "reason": ""}`, true, "", FilterClassComplete, false},
		// Four-class format (NEX-210).
		{"class complete", `{"class": "complete", "reason": "done"}`, true, "done", FilterClassComplete, false},
		{"class scratch", `{"class": "scratch", "reason": "thin reply"}`, false, "thin reply", FilterClassScratch, false},
		{"class goal_not_met", `{"class": "goal_not_met", "reason": "needs more work"}`, true, "needs more work", FilterClassGoalNotMet, false},
		{"class goal_not_met empty reason", `{"class": "goal_not_met"}`, true, "goal_not_met", FilterClassGoalNotMet, false},
		{"class blocked", `{"class": "blocked", "reason": "waiting on operator"}`, false, "waiting on operator", FilterClassBlocked, false},
		{"class blocked empty reason", `{"class": "blocked"}`, false, "blocked", FilterClassBlocked, false},
		// Class wins over post when both present.
		{"class wins over post", `{"class": "goal_not_met", "post": false, "reason": "wip"}`, true, "wip", FilterClassGoalNotMet, false},
		// Format tolerance.
		{"fenced class json", "```json\n{\"class\": \"complete\", \"reason\": \"ok\"}\n```", true, "ok", FilterClassComplete, false},
		{"fenced legacy plain", "```\n{\"post\": false, \"reason\": \"empty\"}\n```", false, "empty", FilterClassScratch, false},
		{"prose before class object", `classification: {"class": "scratch", "reason": "no content"}`, false, "no content", FilterClassScratch, false},
		{"whitespace surrounding class", "  \n{\"class\": \"goal_not_met\", \"reason\": \"wip\"}\n  ", true, "wip", FilterClassGoalNotMet, false},
		// Error cases.
		{"unknown class", `{"class": "invalid"}`, false, "", "", true},
		{"missing class and post", `{"reason": "??"}`, false, "", "", true},
		{"not json at all", `yes substantive`, false, "", "", true},
		{"empty input", ``, false, "", "", true},
		{"long reason truncated", `{"class": "complete", "reason": "` + strings.Repeat("x", 300) + `"}`, true, strings.Repeat("x", 200) + "…", FilterClassComplete, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJudgeJSON(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got decision=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ShouldPost != tc.wantPost {
				t.Errorf("ShouldPost: got %v want %v", got.ShouldPost, tc.wantPost)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason: got %q want %q", got.Reason, tc.wantReason)
			}
			if got.Class != tc.wantClass {
				t.Errorf("Class: got %q want %q", got.Class, tc.wantClass)
			}
		})
	}
}

func TestBuildJudgeUserMessage_IncludesToolsUsed(t *testing.T) {
	msg := buildJudgeUserMessage(FilterInput{
		AspectID:    "shadow",
		FinalText:   "done",
		TriggerFrom: "operator",
		TriggerText: "summarize the changes",
		ToolNames:   []string{"Bash", "Read", "Write"},
	})
	if !strings.Contains(msg, "TOOLS USED:\nBash, Read, Write") {
		t.Errorf("expected TOOLS USED section with comma-joined names; got:\n%s", msg)
	}
}

func TestBuildJudgeUserMessage_TruncatesLongToolList(t *testing.T) {
	names := make([]string, 30)
	for i := range names {
		names[i] = "tool"
	}
	msg := buildJudgeUserMessage(FilterInput{
		AspectID:  "shadow",
		FinalText: "done",
		ToolNames: names,
	})
	if !strings.Contains(msg, "(+10 more)") {
		t.Errorf("expected +10 more truncation suffix; got:\n%s", msg)
	}
}

func TestBuildJudgeUserMessage_IncludesPartialMarker(t *testing.T) {
	msg := buildJudgeUserMessage(FilterInput{
		AspectID:  "shadow",
		FinalText: "partial...",
		Partial:   true,
	})
	if !strings.Contains(msg, "PARTIAL:") {
		t.Errorf("expected PARTIAL marker when Partial=true; got:\n%s", msg)
	}
}

func TestBuildJudgeUserMessage_OmitsSectionsWhenAbsent(t *testing.T) {
	msg := buildJudgeUserMessage(FilterInput{
		AspectID:  "shadow",
		FinalText: "hello",
	})
	if strings.Contains(msg, "TOOLS USED") {
		t.Errorf("TOOLS USED leaked when ToolNames empty; got:\n%s", msg)
	}
	if strings.Contains(msg, "PARTIAL") {
		t.Errorf("PARTIAL marker leaked when Partial=false; got:\n%s", msg)
	}
}

// sequencedJudgeProvider returns a scripted mix of judge results /
// errors per call, in order. Used to drive NEX-292 degradation tests
// without a live model.
type sequencedJudgeProvider struct {
	items []sequencedJudgeItem
	pos   int
}

type sequencedJudgeItem struct {
	json string // when err == nil, returned as ProviderResult.FinalText
	err  error
}

func (*sequencedJudgeProvider) Name() bridle.ProviderID { return "sequenced" }
func (*sequencedJudgeProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}
func (p *sequencedJudgeProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	if p.pos >= len(p.items) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	item := p.items[p.pos]
	p.pos++
	if item.err != nil {
		return bridle.ProviderResult{}, item.err
	}
	return bridle.ProviderResult{
		FinalText:  item.json,
		StopReason: bridle.StopReasonModelDone,
	}, nil
}

func newDegradedFilter(t *testing.T, items []sequencedJudgeItem, cooldown time.Duration) *CheapModelFilter {
	t.Helper()
	prov := &sequencedJudgeProvider{items: items}
	return &CheapModelFilter{
		Harness:          bridle.NewHarness(prov),
		Provider:         "sequenced",
		Model:            "haiku",
		DegradedCooldown: cooldown,
	}
}

// NEX-292: first judge error fails open AND emits an entry SystemNotice
// AND marks the aspect as degraded so the cooldown floor engages on the
// next call.
func TestCheapModelFilter_NEX292_FirstJudgeErrorFailsOpenWithNotice(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{err: context.DeadlineExceeded},
	}, 30*time.Second)

	d := f.Judge(context.Background(), FilterInput{
		FinalText: "candidate reply", AspectID: "anvil", TurnID: "t1",
	})

	if !d.ShouldPost {
		t.Errorf("first judge error should still post (fail-open): got %+v", d)
	}
	if d.SystemNotice == "" {
		t.Errorf("first judge error should emit entry SystemNotice; got empty")
	}
	if !strings.Contains(d.SystemNotice, "anvil") || !strings.Contains(d.SystemNotice, "judge unavailable") {
		t.Errorf("SystemNotice should name the aspect + state degradation; got %q", d.SystemNotice)
	}
	if d.Reason != "judge-degraded-fail-open" {
		t.Errorf("Reason should mark fail-open-degraded; got %q", d.Reason)
	}
}

// NEX-292: a second judge error inside the cooldown window is
// suppressed (ShouldPost=false). This is the cascade-prevention path —
// without it, every echo-amplified inbound message would re-trigger
// fail-open.
func TestCheapModelFilter_NEX292_SecondErrorWithinCooldownIsSuppressed(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{err: context.DeadlineExceeded},
		{err: context.DeadlineExceeded},
	}, 1*time.Hour) // long cooldown so the second call definitely falls inside

	_ = f.Judge(context.Background(), FilterInput{
		FinalText: "first reply", AspectID: "anvil", TurnID: "t1",
	})
	d := f.Judge(context.Background(), FilterInput{
		FinalText: "second reply", AspectID: "anvil", TurnID: "t2",
	})

	if d.ShouldPost {
		t.Errorf("second judge error inside cooldown should suppress reply; got %+v", d)
	}
	if d.Reason != "judge-degraded-rate-limited" {
		t.Errorf("rate-limited reason should be set; got %q", d.Reason)
	}
	if d.SystemNotice != "" {
		t.Errorf("subsequent failures should not re-post the entry notice; got %q", d.SystemNotice)
	}
}

// NEX-292: a second judge error AFTER the cooldown window passes
// re-opens fail-open (the cooldown floor expired) — but does NOT
// re-emit the entry notice because the aspect is still degraded;
// the notice fires once per degradation period, not once per cooldown
// window.
func TestCheapModelFilter_NEX292_SecondErrorAfterCooldownFailsOpenSilently(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{err: context.DeadlineExceeded},
		{err: context.DeadlineExceeded},
	}, 1*time.Millisecond) // tiny cooldown so the second call passes the floor

	_ = f.Judge(context.Background(), FilterInput{
		FinalText: "first reply", AspectID: "anvil", TurnID: "t1",
	})
	time.Sleep(5 * time.Millisecond)
	d := f.Judge(context.Background(), FilterInput{
		FinalText: "second reply", AspectID: "anvil", TurnID: "t2",
	})

	if !d.ShouldPost {
		t.Errorf("second judge error past cooldown should fail open again; got %+v", d)
	}
	if d.SystemNotice != "" {
		t.Errorf("entry notice already posted; should not re-emit; got %q", d.SystemNotice)
	}
}

// NEX-292: a successful verdict after a degradation window clears the
// flag and emits a recovery SystemNotice so the operator/users see
// the loop close.
func TestCheapModelFilter_NEX292_RecoveryEmitsNotice(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{err: context.DeadlineExceeded},                                // enters degradation
		{json: `{"class": "complete", "reason": "substantive"}`},       // recovery
		{json: `{"class": "complete", "reason": "still substantive"}`}, // next healthy verdict
	}, 30*time.Second)

	_ = f.Judge(context.Background(), FilterInput{FinalText: "r1", AspectID: "anvil", TurnID: "t1"})
	d2 := f.Judge(context.Background(), FilterInput{FinalText: "r2", AspectID: "anvil", TurnID: "t2"})

	if !d2.ShouldPost {
		t.Errorf("verdict 'complete' should post: got %+v", d2)
	}
	if d2.SystemNotice == "" || !strings.Contains(d2.SystemNotice, "recovered") {
		t.Errorf("first healthy verdict after degradation should emit recovery notice; got %q", d2.SystemNotice)
	}

	d3 := f.Judge(context.Background(), FilterInput{FinalText: "r3", AspectID: "anvil", TurnID: "t3"})
	if d3.SystemNotice != "" {
		t.Errorf("subsequent healthy verdicts should not re-emit recovery notice; got %q", d3.SystemNotice)
	}
}

// NEX-292: verdict path is untouched in the healthy case — no
// SystemNotice, no degradation marker, behaviour is exactly as
// pre-NEX-292.
func TestCheapModelFilter_NEX292_HealthyVerdictPathUnchanged(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{json: `{"class": "complete", "reason": "substantive"}`},
	}, 30*time.Second)

	d := f.Judge(context.Background(), FilterInput{
		FinalText: "candidate", AspectID: "anvil", TurnID: "t1",
	})

	if !d.ShouldPost {
		t.Errorf("complete verdict should post: got %+v", d)
	}
	if d.SystemNotice != "" {
		t.Errorf("healthy verdict should not emit any SystemNotice; got %q", d.SystemNotice)
	}
}

// NEX-300: judge TurnRequest carries Temperature=0, bounded
// MaxOutputTokens, and a strict json_schema ResponseFormat
// matching the four-class verdict shape — leaning on NEX-299 Pass 2
// to make the parse_failure fail-open path effectively unreachable
// on capable providers (OpenAI, DeepSeek /v1). Asserts via the
// scripted provider's captured-request inspection; no live API.
// NEX-300 + NEX-297 L2 follow-up: by default the judge sends
// response_format=json_object — the PORTABLE variant that works on
// both OpenAI and DeepSeek /v1. NEX-297 L2 found that json_schema
// strict (the original NEX-300 default) returns 400 on DeepSeek /v1,
// so strict is now opt-in via EnforceJSONSchema. Temperature=0 +
// MaxOutputTokens still apply regardless.
func TestCheapModelFilter_NEX300_DefaultUsesJSONObjectResponseFormat(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:  `{"class": "complete", "reason": "substantive"}`,
			Usage:      bridle.Usage{InputTokens: 5, OutputTokens: 1},
			StopReason: bridle.StopReasonModelDone,
		},
	}}
	f := &CheapModelFilter{
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "judge",
		// EnforceJSONSchema deliberately false — pin the default behaviour.
	}
	_ = f.Judge(context.Background(), FilterInput{
		FinalText: "candidate reply",
		AspectID:  "anvil",
		TurnID:    "t1",
	})

	if prov.last.Temperature == nil || *prov.last.Temperature != 0.0 {
		t.Errorf("judge Temperature = %v, want 0.0", prov.last.Temperature)
	}
	if prov.last.MaxOutputTokens <= 0 || prov.last.MaxOutputTokens > 500 {
		t.Errorf("judge MaxOutputTokens = %d, want bounded positive value", prov.last.MaxOutputTokens)
	}
	rf := prov.last.ResponseFormat
	if rf == nil {
		t.Fatal("judge TurnRequest should set ResponseFormat (got nil)")
	}
	if rf.Type != "json_object" {
		t.Errorf("default judge ResponseFormat.Type = %q, want json_object (portable across OpenAI + DeepSeek /v1)", rf.Type)
	}
	if rf.Strict {
		t.Errorf("default judge ResponseFormat.Strict = true, want false (strict is opt-in only)")
	}
	if len(rf.Schema) != 0 {
		t.Errorf("default judge ResponseFormat.Schema should be empty for json_object; got %s", string(rf.Schema))
	}
}

// NEX-300 + NEX-297 L2 follow-up: when an operator sets
// EnforceJSONSchema=true (per-aspect or network-default) the judge
// upgrades to OpenAI's strict structured-outputs mode. Only safe on
// verified-capable providers — see judgeResponseFormat doc + NEX-298.
func TestCheapModelFilter_NEX300_EnforceJSONSchemaOptsIntoStrict(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:  `{"class": "complete", "reason": "substantive"}`,
			Usage:      bridle.Usage{InputTokens: 5, OutputTokens: 1},
			StopReason: bridle.StopReasonModelDone,
		},
	}}
	f := &CheapModelFilter{
		Harness:           bridle.NewHarness(prov),
		Provider:          "scripted",
		Model:             "judge",
		EnforceJSONSchema: true,
	}
	_ = f.Judge(context.Background(), FilterInput{
		FinalText: "candidate reply",
		AspectID:  "anvil",
		TurnID:    "t1",
	})

	rf := prov.last.ResponseFormat
	if rf == nil {
		t.Fatal("opt-in strict mode: ResponseFormat is nil")
	}
	if rf.Type != "json_schema" {
		t.Errorf("opt-in: ResponseFormat.Type = %q, want json_schema", rf.Type)
	}
	if !rf.Strict {
		t.Errorf("opt-in: ResponseFormat.Strict = false, want true")
	}
	if rf.Name == "" {
		t.Errorf("opt-in: ResponseFormat.Name must be non-empty (OpenAI strict mode requires it)")
	}
	// Schema must decode and carry the four-class enum.
	var parsed struct {
		Type                 string                     `json:"type"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(rf.Schema, &parsed); err != nil {
		t.Fatalf("opt-in: Schema invalid JSON: %v", err)
	}
	if parsed.Type != "object" {
		t.Errorf("opt-in: schema.type = %q, want object", parsed.Type)
	}
	if parsed.AdditionalProperties {
		t.Errorf("opt-in: schema.additionalProperties must be false for strict mode")
	}
	wantReqFields := map[string]bool{"class": true, "reason": true}
	for _, r := range parsed.Required {
		delete(wantReqFields, r)
	}
	if len(wantReqFields) > 0 {
		t.Errorf("opt-in: schema.required missing fields: %v", wantReqFields)
	}
	classRaw, hasClass := parsed.Properties["class"]
	if !hasClass {
		t.Fatalf("opt-in: schema.properties.class missing")
	}
	if !strings.Contains(string(classRaw), `"complete"`) ||
		!strings.Contains(string(classRaw), `"scratch"`) ||
		!strings.Contains(string(classRaw), `"goal_not_met"`) ||
		!strings.Contains(string(classRaw), `"blocked"`) {
		t.Errorf("opt-in: class enum should cover the four NEX-210 labels; got %s", string(classRaw))
	}
}

// NEX-292: DegradedCooldown < 0 is the explicit opt-out — restores
// pre-NEX-292 unconditional fail-open, no rate limit, no notice.
// Safe only for single-aspect deployments.
func TestCheapModelFilter_NEX292_NegativeCooldownDisablesRateLimit(t *testing.T) {
	f := newDegradedFilter(t, []sequencedJudgeItem{
		{err: context.DeadlineExceeded},
		{err: context.DeadlineExceeded},
		{err: context.DeadlineExceeded},
	}, -1)

	for i, turnID := range []string{"t1", "t2", "t3"} {
		d := f.Judge(context.Background(), FilterInput{
			FinalText: "reply", AspectID: "anvil", TurnID: turnID,
		})
		if !d.ShouldPost {
			t.Errorf("call %d: legacy mode should always post on judge error; got %+v", i, d)
		}
		if d.SystemNotice != "" {
			t.Errorf("call %d: legacy mode should not emit SystemNotice; got %q", i, d.SystemNotice)
		}
	}
}
