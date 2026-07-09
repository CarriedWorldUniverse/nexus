package main

import (
	"strings"
	"testing"
)

func TestPRLifecycleMarkerRoundTrip(t *testing.T) {
	m := prLifecycleMarker{Verdict: "changes-requested", Round: 2, Head: "4f2c91a"}
	got, ok := parsePRLifecycleMarker("blah\n" + m.String() + "\n## Outstanding")
	if !ok {
		t.Fatal("round-trip parse failed")
	}
	if got != m {
		t.Fatalf("got %+v want %+v", got, m)
	}
}

func TestPRLifecycleMarkerParse(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
	}{
		{"<!-- pr-lifecycle: verdict=approved round=1 head=abcdef1 -->", true},
		{"text before <!--  pr-lifecycle:  verdict=changes-requested  round=3  head=ABCDEF1234 --> after", true},
		{"<!-- pr-lifecycle: verdict=maybe round=1 head=abcdef1 -->", false}, // bad verdict
		{"<!-- pr-lifecycle: verdict=approved head=abcdef1 -->", false},      // no round
		{"no marker at all", false},
		{"<!-- pr-lifecycle: verdict=approved round=1 head=xyz -->", false}, // non-hex head
	}
	for _, c := range cases {
		if _, ok := parsePRLifecycleMarker(c.body); ok != c.ok {
			t.Errorf("parse(%q) ok=%v want %v", c.body, ok, c.ok)
		}
	}
}

func TestDecidePRWatchAction(t *testing.T) {
	st := func(m *prLifecycleMarker) prWatchState {
		return prWatchState{Repo: "o/r", Number: 1, HeadSHA: "aaaa111bbbb", Marker: m}
	}
	cases := []struct {
		name  string
		state prWatchState
		want  string
		round int
	}{
		{"never reviewed", st(nil), "seed-review", 1},
		{"approved is terminal", st(&prLifecycleMarker{Verdict: "approved", Round: 2, Head: "aaaa111"}), "ready-to-merge", 2},
		{"round cap escalates", st(&prLifecycleMarker{Verdict: "changes-requested", Round: 3, Head: "aaaa111"}), "escalate", 3},
		{"changes requested, head unchanged", st(&prLifecycleMarker{Verdict: "changes-requested", Round: 1, Head: "aaaa111"}), "seed-fix", 1},
		{"head advanced re-reviews", prWatchState{HeadSHA: "cccc222dddd", Marker: &prLifecycleMarker{Verdict: "changes-requested", Round: 1, Head: "aaaa111"}}, "seed-review", 2},
		{"approved wins over cap", st(&prLifecycleMarker{Verdict: "approved", Round: 5, Head: "aaaa111"}), "ready-to-merge", 5},
	}
	for _, c := range cases {
		got := decidePRWatchAction(c.state, 3)
		if got.Kind != c.want || got.Round != c.round {
			t.Errorf("%s: got %s r%d, want %s r%d (%s)", c.name, got.Kind, got.Round, c.want, c.round, got.Reason)
		}
	}
}

func TestDecidePRWatchActionShortSHATolerant(t *testing.T) {
	// Markers may carry short SHAs; a 7-char reviewed head must match the
	// full 40-char current head (no spurious re-review).
	st := prWatchState{HeadSHA: "4f2c91a0000000000000000000000000000000ff",
		Marker: &prLifecycleMarker{Verdict: "changes-requested", Round: 1, Head: "4f2c91a"}}
	got := decidePRWatchAction(st, 3)
	if got.Kind != "seed-fix" {
		t.Fatalf("short-SHA match should seed-fix, got %s (%s)", got.Kind, got.Reason)
	}
}

func TestPersonalityFromAgentEmail(t *testing.T) {
	cases := map[string]string{
		"anvil-builder@agents.carriedworld.com": "anvil",
		"plumb-builder@agents.carriedworld.com": "plumb",
		"keel-reviewer@agents.carriedworld.com": "keel",
		"nexus@darksoft.co.nz":                  "",
		"operator@carriedworld.com":             "",
	}
	for in, want := range cases {
		if got := personalityFromAgentEmail(in); got != want {
			t.Errorf("personalityFromAgentEmail(%q) = %q want %q", in, got, want)
		}
	}
}

func TestPickReviewerAvoidsAuthors(t *testing.T) {
	got := pickReviewer([]string{"anvil", "plumb", "keel"}, map[string]bool{"anvil": true})
	if got != "plumb" {
		t.Fatalf("want plumb, got %q", got)
	}
	if got := pickReviewer([]string{"anvil"}, map[string]bool{"anvil": true}); got != "" {
		t.Fatalf("no eligible reviewer should be empty, got %q", got)
	}
}

func TestBriefsCarryTheContract(t *testing.T) {
	st := prWatchState{Repo: "o/r", Number: 7, URL: "https://github.com/o/r/pull/7",
		Branch: "builder/NET-99", HeadSHA: "abc1234"}
	task, criteria := reviewBrief(st, 2)
	for _, want := range []string{"gh pr diff 7", "pr-lifecycle", "round=2", "head=abc1234", "Outstanding", "Do NOT merge"} {
		if !strings.Contains(task, want) {
			t.Errorf("review task missing %q", want)
		}
	}
	if !strings.Contains(criteria, "round=2") || !strings.Contains(criteria, "head=abc1234") {
		t.Errorf("review criteria must pin round+head: %s", criteria)
	}
	task, criteria = fixBrief(st, 2)
	for _, want := range []string{"https://github.com/o/r/pull/7", "builder/NET-99", "cairn commit", "Do NOT merge", "binaries"} {
		if !strings.Contains(task, want) {
			t.Errorf("fix task missing %q", want)
		}
	}
	if !strings.Contains(criteria, "builder/NET-99") {
		t.Errorf("fix criteria must name the branch: %s", criteria)
	}
}
