package main

import (
	"reflect"
	"testing"
)

// NEX-303 (close-intent narrowing, post dry-run 2026-05-26):
// extractCloseIntentKeys returns only tickets the PR INTENDS to
// close — title-lead OR explicit closing keyword in body. Bare
// cross-refs are skipped (the naive any-mention regex caught 4
// false-positives in a 33-candidate dry-run set).
func TestExtractCloseIntentKeys(t *testing.T) {
	cases := []struct {
		name        string
		title, body string
		want        []string
	}{
		{
			name:  "title-lead with body cross-refs — only title key fires",
			title: "NEX-247: TicketTriage classifier (lane 4 of NEX-243)",
			body:  "Related: NEX-244, NEX-245, NEX-246 (siblings). See NEX-296 for context.",
			want:  []string{"NEX-247"},
		},
		{
			name:  "title-lead + body closing keyword — both fire",
			title: "NEX-303: nexus close-merged-tickets",
			body:  "Closes NEX-303. Related: NEX-247.",
			want:  []string{"NEX-303"},
		},
		{
			name:  "body Closes keyword, no title lead",
			title: "tighten close-intent narrowing",
			body:  "Closes NEX-303 with the bug found in dry-run.",
			want:  []string{"NEX-303"},
		},
		{
			name:  "body Fixes keyword (and variants close/closed/fix/fixes/fixed/resolve/resolves/resolved)",
			title: "x",
			body:  "Fixes NEX-1. closes NEX-2. closed NEX-3. fix NEX-4. fixed NEX-5. resolve NEX-6. resolves NEX-7. resolved NEX-8.",
			want:  []string{"NEX-1", "NEX-2", "NEX-3", "NEX-4", "NEX-5", "NEX-6", "NEX-7", "NEX-8"},
		},
		{
			name:  "colon between keyword and key (Closes: NEX-N)",
			title: "x",
			body:  "Closes: NEX-100",
			want:  []string{"NEX-100"},
		},
		{
			name:  "lowercase keyword + uppercase ticket",
			title: "x",
			body:  "closes NEX-99",
			want:  []string{"NEX-99"},
		},
		{
			name:  "title-lead NEX in middle of title — DOES NOT FIRE (not the leading ref)",
			title: "PR for the NEX-247 work",
			body:  "",
			want:  nil,
		},
		{
			name:  "body bare mention — DOES NOT FIRE (no closing keyword)",
			title: "Misc cleanup",
			body:  "Touches code referenced in NEX-100 and NEX-200.",
			want:  nil,
		},
		{
			name:  "body bare 'related to' — DOES NOT FIRE",
			title: "feat: x",
			body:  "Related: NEX-243 (parent epic). Supersedes NEX-100.",
			want:  nil,
		},
		{
			name:  "neither title nor body has anything",
			title: "Random PR title",
			body:  "Random PR body with no NEX refs at all",
			want:  nil,
		},
		{
			name:  "empty title + body",
			title: "",
			body:  "",
			want:  nil,
		},
		{
			name:  "dedup: title-lead and body keyword reference same key",
			title: "NEX-303: foo",
			body:  "Closes NEX-303",
			want:  []string{"NEX-303"},
		},
		{
			name:  "word-boundary on title-lead — ANEX-247 does not fire",
			title: "ANEX-247: not us",
			body:  "",
			want:  nil,
		},
		{
			name:  "leading whitespace on title still matches lead",
			title: "  NEX-1: padded title",
			body:  "",
			want:  []string{"NEX-1"},
		},
		{
			name:  "partial-slice form 'NEX-N Slice 2:' DOES NOT fire title-lead — operator uses body keyword when slice = ticket-done",
			title: "NEX-247 Slice 2: nexus triage-tickets polling subcommand",
			body:  "Last slice of NEX-247.",
			want:  nil,
		},
		{
			name:  "partial-slice form 'NEX-N L1:' DOES NOT fire title-lead — L3 of NEX-297 was still pending when L1 PR shipped",
			title: "NEX-297 L1: nexus test-provider CLI",
			body:  "",
			want:  nil,
		},
		{
			name:  "partial-slice form with explicit 'Closes' in body — body keyword path fires",
			title: "NEX-247 Slice 2: final slice",
			body:  "Closes NEX-247 — last slice of the lane shipped.",
			want:  []string{"NEX-247"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCloseIntentKeys(c.title, c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("extractCloseIntentKeys(title=%q, body=%q)\n  got  = %v\n  want = %v", c.title, c.body, got, c.want)
			}
		})
	}
}

// NEX-303: repeatable --repo flag accumulates values across multiple
// uses on the command line. Pin the flag.Value contract.
func TestRepeatableStringFlag_AccumulatesValues(t *testing.T) {
	var r repeatableStringFlag
	if err := r.Set("CarriedWorldUniverse/nexus"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("CarriedWorldUniverse/bridle"); err != nil {
		t.Fatal(err)
	}
	if len(r) != 2 || r[0] != "CarriedWorldUniverse/nexus" || r[1] != "CarriedWorldUniverse/bridle" {
		t.Errorf("unexpected accumulation: %v", []string(r))
	}
	want := "CarriedWorldUniverse/nexus,CarriedWorldUniverse/bridle"
	if got := r.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// NEX-303: nil flag's String() returns empty (flag library calls
// String() on a zero-valued pointer to render the default in
// --help output).
func TestRepeatableStringFlag_NilSafe(t *testing.T) {
	var r *repeatableStringFlag
	if got := r.String(); got != "" {
		t.Errorf("nil String() = %q, want empty", got)
	}
}
