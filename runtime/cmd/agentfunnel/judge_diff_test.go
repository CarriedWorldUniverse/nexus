package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPickPRNumberByTicket(t *testing.T) {
	var zero time.Time
	list := `[
		{"number":7,"headRefName":"anvil/x","title":"unrelated"},
		{"number":88,"headRefName":"builder/NET-66","title":"NET-66 eviction","createdAt":"2026-07-07T12:00:00Z"}
	]`
	num, found, err := pickPRNumberByTicket([]byte(list), "NET-66", zero)
	if err != nil || !found || num != 88 {
		t.Fatalf("got num=%d found=%v err=%v; want 88,true,nil", num, found, err)
	}
	// substring must NOT match (Unit 4 word-boundary carried through)
	if _, found, _ := pickPRNumberByTicket([]byte(list), "NET-6", zero); found {
		t.Fatal("NET-6 should not match NET-66 PR")
	}
	// provenance: a pre-run PR is not resolved
	runStart, _ := time.Parse(time.RFC3339, "2026-07-07T13:00:00Z")
	if _, found, _ := pickPRNumberByTicket([]byte(list), "NET-66", runStart); found {
		t.Fatal("PR created before run start must not be resolved")
	}
	if _, _, err := pickPRNumberByTicket([]byte(`bad`), "NET-66", zero); err == nil {
		t.Fatal("malformed json should error")
	}
}

func TestAugmentedForJudge(t *testing.T) {
	origDiff := prDiffFn
	defer func() { prDiffFn = origDiff }()
	log := slog.Default()

	t.Run("no repo → report only", func(t *testing.T) {
		prDiffFn = func(string, string, string) (string, bool, error) {
			return "", false, errors.New("should not be called")
		}
		g := &builderAcceptanceGate{} // repo == ""
		if got := g.augmentedForJudge("report", log); got != "report" {
			t.Fatalf("want report unchanged, got %q", got)
		}
	})
	t.Run("disabled via env → report only", func(t *testing.T) {
		t.Setenv("ACCEPTANCE_JUDGE_DIFF", "0")
		prDiffFn = func(string, string, string) (string, bool, error) {
			return "DIFF", true, nil
		}
		g := &builderAcceptanceGate{repo: "org/repo", ticket: "NET-1"}
		if got := g.augmentedForJudge("report", log); got != "report" {
			t.Fatalf("disabled should return report only, got %q", got)
		}
	})
	t.Run("diff found → augmented", func(t *testing.T) {
		prDiffFn = func(_, _, _ string) (string, bool, error) {
			return "--- a/f\n+++ b/f\n+CONVERGED-OK", true, nil
		}
		g := &builderAcceptanceGate{repo: "org/repo", ticket: "NET-1", branch: ""}
		got := g.augmentedForJudge("agent claims done", log)
		if !strings.Contains(got, "CONVERGED-OK") || !strings.Contains(got, "ACTUAL PR DIFF") {
			t.Fatalf("expected augmented judge input, got:\n%s", got)
		}
	})
	t.Run("gh error → report only (fail-open)", func(t *testing.T) {
		prDiffFn = func(_, _, _ string) (string, bool, error) {
			return "", false, errors.New("gh boom")
		}
		g := &builderAcceptanceGate{repo: "org/repo", ticket: "NET-1"}
		if got := g.augmentedForJudge("report", log); got != "report" {
			t.Fatalf("gh error must fail open to report only, got %q", got)
		}
	})
	t.Run("no diff yet → report only", func(t *testing.T) {
		prDiffFn = func(_, _, _ string) (string, bool, error) {
			return "", false, nil
		}
		g := &builderAcceptanceGate{repo: "org/repo", ticket: "NET-1"}
		if got := g.augmentedForJudge("report", log); got != "report" {
			t.Fatalf("no diff should return report only, got %q", got)
		}
	})
}
