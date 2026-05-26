package main

import (
	"reflect"
	"testing"
)

// NEX-303: extractNexKeys finds every unique NEX-* reference in
// arbitrary text (titles, PR bodies, commit messages all flow in).
// Case-insensitive match handles drift between "NEX-100" / "nex-100".
// Word-boundary requirement prevents false-positives from substrings
// like "ANEX-1000" or "NEX-100x".
func TestExtractNexKeys_FindsUniqueUppercased(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single ref in title",
			in:   "NEX-247: TicketTriage classifier (lane 4 of NEX-243)",
			want: []string{"NEX-243", "NEX-247"},
		},
		{
			name: "multiple refs in body, deduped",
			in:   "Closes NEX-100. Related: NEX-200, NEX-100, nex-300.",
			want: []string{"NEX-100", "NEX-200", "NEX-300"},
		},
		{
			name: "lowercase normalised to uppercase",
			in:   "fixes nex-42",
			want: []string{"NEX-42"},
		},
		{
			name: "mixed case + various separators",
			in:   "NEX-1 nex-2 Nex-3 [NEX-4] (NEX-5)",
			want: []string{"NEX-1", "NEX-2", "NEX-3", "NEX-4", "NEX-5"},
		},
		{
			name: "no refs",
			in:   "just a regular PR title",
			want: nil,
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "word-boundary respected — no false positive on ANEX prefix or NEX-100x suffix",
			// \b is between word/non-word chars. "ANEX-1000" has A-N
			// as word/word (no boundary before N), so the regex won't
			// match. "NEX-100xyz" has 0-x as word/word (no boundary
			// after the digits), so it also won't match. Both
			// exclusions are deliberate — operator who writes
			// "NEX-100" inside a longer identifier is referencing
			// something else, not the ticket.
			in:   "ANEX-1000 is not nex; NEX-100xyz also not",
			want: nil,
		},
		{
			name: "word-boundary allows real refs with surrounding punctuation",
			in:   "fixes NEX-1, closes (NEX-2), also [NEX-3].",
			want: []string{"NEX-1", "NEX-2", "NEX-3"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractNexKeys(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("extractNexKeys(%q)\n  got  = %v\n  want = %v", c.in, got, c.want)
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
