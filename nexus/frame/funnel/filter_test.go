package funnel

import (
	"context"
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
	d := CheapModelFilter{}.Judge(context.Background(), FilterInput{FinalText: "real reply"})
	if !d.ShouldPost {
		t.Errorf("misconfigured filter should fail open: got %+v", d)
	}
}

func TestCheapModelFilter_SuppressesEmpty(t *testing.T) {
	d := CheapModelFilter{}.Judge(context.Background(), FilterInput{FinalText: "  "})
	if d.ShouldPost {
		t.Errorf("empty input always suppressed: got %+v", d)
	}
}

func TestCheapModelFilter_ParsesYesNo(t *testing.T) {
	tests := []struct {
		name      string
		modelText string
		want      bool
	}{
		{"clean yes", "yes", true},
		{"clean no", "no", false},
		{"trailing punctuation no", "no.", false},
		{"trailing newline no", "no\n", false},
		{"capitalized YES", "YES", true},
		{"capitalized NO", "NO", false},
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
			f := CheapModelFilter{
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
	f := CheapModelFilter{
		Harness:  bridle.NewHarness(erroringProvider{err: context.DeadlineExceeded}),
		Provider: "erroring",
		Model:    "m",
	}
	d := f.Judge(context.Background(), FilterInput{FinalText: "real reply", TurnID: "t1"})
	if !d.ShouldPost {
		t.Errorf("provider error should fail open (post): got %+v", d)
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
