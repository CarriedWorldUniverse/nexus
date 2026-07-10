package gates

import (
	"errors"
	"testing"
	"time"
)

func TestTicketWordMatch(t *testing.T) {
	cases := []struct {
		s, ticket string
		want      bool
	}{
		{"builder/NET-66", "NET-66", true},
		{"builder/NET-66", "NET-6", false},
		{"builder/NET-661", "NET-66", false},
		{"aNET-66", "NET-66", false},
		{"NET-66x", "NET-66", false},
		{"NET-66", "NET-66", true},
		{"anything", "", false},
	}
	for _, tc := range cases {
		if got := TicketWordMatch(tc.s, tc.ticket); got != tc.want {
			t.Errorf("TicketWordMatch(%q,%q) = %v, want %v", tc.s, tc.ticket, got, tc.want)
		}
	}
}

func TestMatchPRByTicketProvenance(t *testing.T) {
	runStart, _ := time.Parse(time.RFC3339, "2026-07-07T12:00:00Z")
	older := `[{"number":1,"headRefName":"builder/NET-66","title":"NET-66","createdAt":"2026-07-07T11:00:00Z"}]`
	newer := `[{"number":2,"headRefName":"builder/NET-66","title":"NET-66","createdAt":"2026-07-07T12:30:00Z"}]`

	if ok, _ := MatchPRByTicket([]byte(older), "NET-66", runStart); ok {
		t.Fatal("PR created BEFORE run start must not be credited (Unit 4)")
	}
	if ok, _ := MatchPRByTicket([]byte(newer), "NET-66", runStart); !ok {
		t.Fatal("PR created after run start should be credited")
	}
}

func TestParsePRDiffStatsHead(t *testing.T) {
	got, found, err := ParsePRDiffStatsHead([]byte(`[{"additions":40,"deletions":3,"changedFiles":2}]`))
	if err != nil || !found || got.Additions != 40 {
		t.Fatalf("got=%+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := ParsePRDiffStatsHead([]byte(`[]`)); found {
		t.Fatal("empty list should not be found")
	}
	if _, _, err := ParsePRDiffStatsHead([]byte(`bad`)); err == nil {
		t.Fatal("malformed json should error")
	}
}

func TestPRExists(t *testing.T) {
	existsFn := func(repo, branch string) (bool, error) { return true, nil }
	existsByTicketFn := func(repo, ticket string) (bool, error) { return false, nil }

	ok, err := PRExists("org/repo", "builder/NET-1", "NET-1", existsFn, existsByTicketFn)
	if err != nil || !ok {
		t.Fatalf("PRExists = %v, %v; want true, nil", ok, err)
	}

	if _, err := PRExists("", "b", "t", existsFn, existsByTicketFn); err == nil {
		t.Fatal("empty repo should error")
	}
}

func TestPRSubstantial(t *testing.T) {
	statsFn := func(repo, branch string) (PRDiffStats, bool, error) {
		return PRDiffStats{Additions: 10, Deletions: 2, ChangedFiles: 1}, true, nil
	}
	statsByTicketFn := func(repo, ticket string) (PRDiffStats, bool, error) {
		return PRDiffStats{}, false, errors.New("should not be called")
	}
	ok, err := PRSubstantial("org/repo", "builder/NET-1", "NET-1", 1, statsFn, statsByTicketFn)
	if err != nil || !ok {
		t.Fatalf("PRSubstantial = %v, %v; want true, nil", ok, err)
	}
	if ok, err := PRSubstantial("org/repo", "b", "t", 0, statsFn, statsByTicketFn); err != nil || !ok {
		t.Fatalf("floor 0 should back-compat pass: %v %v", ok, err)
	}
}

func TestDiffTouchesTestFile(t *testing.T) {
	withTest := "diff --git a/foo_test.go b/foo_test.go\n+++ b/foo_test.go\n@@\n+func TestX\n"
	withoutTest := "diff --git a/foo.go b/foo.go\n+++ b/foo.go\n@@\n+func Foo\n"
	if !DiffTouchesTestFile(withTest) {
		t.Error("should detect test file")
	}
	if DiffTouchesTestFile(withoutTest) {
		t.Error("should not detect test file")
	}
}

func TestCriteriaMentionsTests(t *testing.T) {
	if !CriteriaMentionsTests("must pass all TESTS") {
		t.Error("should match case-insensitively")
	}
	if CriteriaMentionsTests("build the widget") {
		t.Error("should not match")
	}
}
