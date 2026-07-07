package main

import (
	"errors"
	"log/slog"
	"testing"
)

// TestBuilderPRVerifierSubstanceGate proves the Unit 2 wiring: a PR that
// EXISTS but has an empty diff must NOT verify as complete, while an existing
// PR with a real diff does.
func TestBuilderPRVerifierSubstanceGate(t *testing.T) {
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn = origExists; prDiffStatsFn = origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil } // PR exists
	log := slog.Default()

	t.Run("exists but empty diff → not verified", func(t *testing.T) {
		prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
			return prDiffStats{Additions: 0, Deletions: 0, ChangedFiles: 0}, true, nil
		}
		if builderPRVerifier(log, "plumb", "org/repo", "NET-1", "")() {
			t.Fatal("empty-diff PR should NOT verify (Unit 2)")
		}
	})
	t.Run("exists with real diff → verified", func(t *testing.T) {
		prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
			return prDiffStats{Additions: 30, Deletions: 4, ChangedFiles: 3}, true, nil
		}
		if !builderPRVerifier(log, "plumb", "org/repo", "NET-1", "")() {
			t.Fatal("substantial PR should verify")
		}
	})
	t.Run("disabled (floor 0) → back-compat, exists is enough", func(t *testing.T) {
		t.Setenv("ACCEPTANCE_MIN_DIFF_LINES", "0")
		prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
			return prDiffStats{}, false, errors.New("should not gate when disabled")
		}
		if !builderPRVerifier(log, "plumb", "org/repo", "NET-1", "")() {
			t.Fatal("with substance gate disabled, an existing PR must verify")
		}
	})
}

func TestParsePRDiffStatsHead(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantFound bool
		wantAdd   int
		wantFiles int
		wantErr   bool
	}{
		{"empty list → not found", `[]`, false, 0, 0, false},
		{"one PR", `[{"additions":40,"deletions":3,"changedFiles":2}]`, true, 40, 2, false},
		{"takes first of many", `[{"additions":5,"deletions":0,"changedFiles":1},{"additions":99,"deletions":9,"changedFiles":9}]`, true, 5, 1, false},
		{"zero-diff PR is found but empty", `[{"additions":0,"deletions":0,"changedFiles":0}]`, true, 0, 0, false},
		{"malformed json → err", `not json`, false, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := parsePRDiffStatsHead([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found && (got.Additions != tc.wantAdd || got.ChangedFiles != tc.wantFiles) {
				t.Fatalf("stats = %+v, want add=%d files=%d", got, tc.wantAdd, tc.wantFiles)
			}
		})
	}
}

func TestSelectPRDiffStatsByTicket(t *testing.T) {
	list := `[
		{"headRefName":"anvil/workers-json-flag","title":"workers json","additions":12,"deletions":1,"changedFiles":2},
		{"headRefName":"builder/NET-99","title":"NET-99 eviction","additions":80,"deletions":4,"changedFiles":3}
	]`
	// match by ticket in head branch
	got, found, err := selectPRDiffStatsByTicket([]byte(list), "NET-99")
	if err != nil || !found || got.Additions != 80 || got.ChangedFiles != 3 {
		t.Fatalf("NET-99 match: got=%+v found=%v err=%v", got, found, err)
	}
	// match by ticket in title (NET-99 also appears in title — same row, fine)
	if _, found, _ := selectPRDiffStatsByTicket([]byte(list), "NOPE-1"); found {
		t.Fatalf("NOPE-1 should not match")
	}
	if _, _, err := selectPRDiffStatsByTicket([]byte(`bad`), "NET-99"); err == nil {
		t.Fatalf("malformed json should error")
	}
}

func TestMinAcceptanceDiffLines(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 1},
		{"  ", 1},
		{"5", 5},
		{"0", 0},
		{"-3", 1},   // negative → default
		{"abc", 1},  // malformed → default
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("ACCEPTANCE_MIN_DIFF_LINES", tc.env)
			if got := minAcceptanceDiffLines(); got != tc.want {
				t.Fatalf("minAcceptanceDiffLines(%q) = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

func TestPRSubstantial(t *testing.T) {
	origHead := prDiffStatsFn
	origTicket := prDiffStatsByTicketFn
	defer func() { prDiffStatsFn = origHead; prDiffStatsByTicketFn = origTicket }()

	head := func(s prDiffStats, found bool, err error) func(string, string) (prDiffStats, bool, error) {
		return func(string, string) (prDiffStats, bool, error) { return s, found, err }
	}
	ticket := func(s prDiffStats, found bool, err error) func(string, string) (prDiffStats, bool, error) {
		return func(string, string) (prDiffStats, bool, error) { return s, found, err }
	}

	cases := []struct {
		name     string
		floor    int
		headFn   func(string, string) (prDiffStats, bool, error)
		ticketFn func(string, string) (prDiffStats, bool, error)
		want     bool
		wantErr  bool
	}{
		{
			name:   "floor 0 disables → substantial",
			floor:  0,
			headFn: head(prDiffStats{}, false, errors.New("should not be called")),
			want:   true,
		},
		{
			name:   "non-empty own-branch PR passes",
			floor:  1,
			headFn: head(prDiffStats{Additions: 40, Deletions: 3, ChangedFiles: 2}, true, nil),
			want:   true,
		},
		{
			name:   "empty own-branch PR fails",
			floor:  1,
			headFn: head(prDiffStats{Additions: 0, Deletions: 0, ChangedFiles: 0}, true, nil),
			want:   false,
		},
		{
			name:   "diff below floor fails",
			floor:  50,
			headFn: head(prDiffStats{Additions: 10, Deletions: 5, ChangedFiles: 1}, true, nil),
			want:   false,
		},
		{
			name:     "not on own branch → ticket fallback, substantial",
			floor:    1,
			headFn:   head(prDiffStats{}, false, nil),
			ticketFn: ticket(prDiffStats{Additions: 12, Deletions: 1, ChangedFiles: 2}, true, nil),
			want:     true,
		},
		{
			name:     "no PR anywhere → not substantial, no error",
			floor:    1,
			headFn:   head(prDiffStats{}, false, nil),
			ticketFn: ticket(prDiffStats{}, false, nil),
			want:     false,
		},
		{
			name:    "gh error on head → fail-closed with error",
			floor:   1,
			headFn:  head(prDiffStats{}, false, errors.New("gh boom")),
			want:    false,
			wantErr: true,
		},
		{
			name:     "gh error on ticket fallback → fail-closed with error",
			floor:    1,
			headFn:   head(prDiffStats{}, false, nil),
			ticketFn: ticket(prDiffStats{}, false, errors.New("gh boom")),
			want:     false,
			wantErr:  true,
		},
		{
			name:   "changedFiles 0 but lines >0 → fails (rename/mode-only guard)",
			floor:  1,
			headFn: head(prDiffStats{Additions: 2, Deletions: 0, ChangedFiles: 0}, true, nil),
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prDiffStatsFn = tc.headFn
			if tc.ticketFn != nil {
				prDiffStatsByTicketFn = tc.ticketFn
			} else {
				prDiffStatsByTicketFn = ticket(prDiffStats{}, false, errors.New("ticket fn should not be called"))
			}
			got, err := prSubstantial("org/repo", "builder/NET-99", "NET-99", tc.floor)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("prSubstantial = %v, want %v", got, tc.want)
			}
		})
	}
}
