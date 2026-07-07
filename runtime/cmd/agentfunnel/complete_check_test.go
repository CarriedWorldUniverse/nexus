package main

import (
	"errors"
	"log/slog"
	"testing"
)

func TestBuilderCompleteCheck(t *testing.T) {
	orig := prExistsFn
	origStats := prDiffStatsFn
	defer func() { prExistsFn = orig; prDiffStatsFn = origStats }()
	// Unit 2: the gate now also requires a non-empty diff. Stub the substance
	// lookup to a substantial PR so the "pr exists" case exercises the stop
	// path; the no-pr / error cases short-circuit at prExists before this runs.
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 20, Deletions: 2, ChangedFiles: 2}, true, nil
	}
	log := slog.Default()

	cases := []struct {
		name     string
		fn       func(repo, ticket string) (bool, error)
		wantStop bool
		wantRet  bool
	}{
		{"pr exists -> stop", func(_, _ string) (bool, error) { return true, nil }, true, true},
		{"no pr -> continue", func(_, _ string) (bool, error) { return false, nil }, false, false},
		{"check error -> continue", func(_, _ string) (bool, error) { return false, errors.New("boom") }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prExistsFn = tc.fn
			stopped := false
			got := builderCompleteCheck(func() { stopped = true }, log, "plumb", "org/repo", "NEX-1", "")()
			if got != tc.wantRet {
				t.Errorf("return = %v, want %v", got, tc.wantRet)
			}
			if stopped != tc.wantStop {
				t.Errorf("stopped = %v, want %v", stopped, tc.wantStop)
			}
		})
	}
}

func TestPrExistsRequiresRepoTicket(t *testing.T) {
	if _, err := prExists("", "NEX-1", "NEX-1"); err == nil {
		t.Error("empty repo should be unverifiable (error)")
	}
	if _, err := prExists("org/repo", "", "NEX-1"); err == nil {
		t.Error("empty ticket should be unverifiable (error)")
	}
}

// TestPrExistsFallsBackToTicketSearch (NET-46 live evidence): a worker
// commits to its own branch name instead of the conventional builder/<ticket>
// one. The head-branch-only check misses the resulting PR entirely; prExists
// must fall back to a ticket search across open PRs before reporting false.
func TestPrExistsFallsBackToTicketSearch(t *testing.T) {
	origHead := prExistsFn
	origTicket := prExistsByTicketFn
	defer func() { prExistsFn = origHead; prExistsByTicketFn = origTicket }()

	prExistsFn = func(repo, branch string) (bool, error) { return false, nil }

	t.Run("found by ticket", func(t *testing.T) {
		prExistsByTicketFn = func(repo, ticket string) (bool, error) {
			if repo != "org/repo" || ticket != "NEX-46" {
				t.Fatalf("fallback args repo=%q ticket=%q", repo, ticket)
			}
			return true, nil
		}
		ok, err := prExists("org/repo", "builder/NEX-46", "NEX-46")
		if err != nil || !ok {
			t.Fatalf("prExists = %v, %v; want true, nil", ok, err)
		}
	})

	t.Run("no pr at all", func(t *testing.T) {
		prExistsByTicketFn = func(repo, ticket string) (bool, error) { return false, nil }
		ok, err := prExists("org/repo", "builder/NEX-46", "NEX-46")
		if err != nil || ok {
			t.Fatalf("prExists = %v, %v; want false, nil", ok, err)
		}
	})
}

func TestMatchPRByTicket(t *testing.T) {
	out := []byte(`[{"number":413,"headRefName":"anvil/workers-json-flag","title":"NET-46 add workers json flag"},
		{"number":9,"headRefName":"builder/OTHER-1","title":"unrelated"}]`)
	ok, err := matchPRByTicket(out, "NET-46")
	if err != nil || !ok {
		t.Fatalf("matchPRByTicket = %v, %v; want true, nil", ok, err)
	}
	ok, err = matchPRByTicket(out, "NEX-999")
	if err != nil || ok {
		t.Fatalf("matchPRByTicket = %v, %v; want false, nil", ok, err)
	}
}
