package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
)

func TestParseWorkitemCreateArgs_Defaults(t *testing.T) {
	os.Unsetenv("WORKGRAPH_ORG")
	os.Unsetenv("WORKGRAPH_SUBJECT")
	os.Unsetenv("WORKGRAPH_PROJECT")

	cfg, err := parseWorkitemCreateArgs([]string{
		"--role", "builder",
		"--task", "do the thing",
		"--criteria", "builds",
		"--criteria", "tests pass",
	})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Role != "builder" {
		t.Errorf("Role = %q, want builder", cfg.Role)
	}
	if cfg.Task != "do the thing" {
		t.Errorf("Task = %q, want %q", cfg.Task, "do the thing")
	}
	if len(cfg.Criteria) != 2 || cfg.Criteria[0] != "builds" || cfg.Criteria[1] != "tests pass" {
		t.Errorf("Criteria = %v, want [builds, tests pass]", cfg.Criteria)
	}
	if cfg.Org != workgraph.DefaultOrg {
		t.Errorf("Org = %q, want default %q", cfg.Org, workgraph.DefaultOrg)
	}
	if cfg.Subject != defaultWorkitemSubject {
		t.Errorf("Subject = %q, want default %q", cfg.Subject, defaultWorkitemSubject)
	}
	if cfg.Project != workgraph.DefaultProject {
		t.Errorf("Project = %q, want default %q", cfg.Project, workgraph.DefaultProject)
	}
}

func TestParseWorkitemCreateArgs_EnvDefaults(t *testing.T) {
	t.Setenv("WORKGRAPH_ORG", "env-org")
	t.Setenv("WORKGRAPH_SUBJECT", "env-subject")
	t.Setenv("WORKGRAPH_PROJECT", "ENVPROJ")

	cfg, err := parseWorkitemCreateArgs([]string{"--role", "builder", "--task", "x", "--criteria", "y"})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Org != "env-org" || cfg.Subject != "env-subject" || cfg.Project != "ENVPROJ" {
		t.Errorf("cfg = %+v, want env-sourced org/subject/project", cfg)
	}
}

func TestParseWorkitemCreateArgs_FlagsOverrideEnv(t *testing.T) {
	t.Setenv("WORKGRAPH_ORG", "env-org")

	cfg, err := parseWorkitemCreateArgs([]string{
		"--role", "builder", "--task", "x", "--criteria", "y",
		"--org", "flag-org",
	})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Org != "flag-org" {
		t.Errorf("Org = %q, want flag-org (flag overrides env)", cfg.Org)
	}
}

// TestParseWorkitemCreateArgs_Repo covers the Phase 4 "real REPO tickets"
// --repo flag (nexus workitem create --repo <owner/name>).
func TestParseWorkitemCreateArgs_Repo(t *testing.T) {
	cfg, err := parseWorkitemCreateArgs([]string{
		"--role", "builder", "--task", "x", "--criteria", "y",
		"--repo", "CarriedWorldUniverse/nexus",
	})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("Repo = %q, want CarriedWorldUniverse/nexus", cfg.Repo)
	}
}

// TestParseWorkitemCreateArgs_RepoDefaultsEmpty: absent --repo must default
// to "" (respond-only work, no repo/branch/PR gate), unchanged from before
// this flag existed.
func TestParseWorkitemCreateArgs_RepoDefaultsEmpty(t *testing.T) {
	cfg, err := parseWorkitemCreateArgs([]string{"--role", "builder", "--task", "x", "--criteria", "y"})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Repo != "" {
		t.Errorf("Repo = %q, want empty default", cfg.Repo)
	}
}

func TestParseWorkitemCreateArgs_Help(t *testing.T) {
	_, err := parseWorkitemCreateArgs([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}
}

func TestBuildWorkItem_RoleRequired(t *testing.T) {
	_, err := buildWorkItem(&workitemCreateConfig{Task: "x", Criteria: []string{"y"}})
	if err == nil {
		t.Fatal("expected error for missing --role")
	}
}

func TestBuildWorkItem_TaskRequired(t *testing.T) {
	_, err := buildWorkItem(&workitemCreateConfig{Role: "builder", Criteria: []string{"y"}})
	if err == nil {
		t.Fatal("expected error for missing --task/--task-file")
	}
}

func TestBuildWorkItem_TaskAndTaskFileMutuallyExclusive(t *testing.T) {
	_, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", Task: "x", TaskFile: "somefile", Criteria: []string{"y"},
	})
	if err == nil {
		t.Fatal("expected error when both --task and --task-file are set")
	}
}

func TestBuildWorkItem_CriteriaRequired(t *testing.T) {
	_, err := buildWorkItem(&workitemCreateConfig{Role: "builder", Task: "x"})
	if err == nil {
		t.Fatal("expected error for missing --criteria/--criteria-file")
	}
}

func TestBuildWorkItem_TaskFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.txt")
	if err := os.WriteFile(path, []byte("do the thing from a file\n"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}

	wi, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", TaskFile: path, Criteria: []string{"builds"},
	})
	if err != nil {
		t.Fatalf("buildWorkItem: %v", err)
	}
	if wi.TaskSpec != "do the thing from a file" {
		t.Errorf("TaskSpec = %q, want trimmed file contents", wi.TaskSpec)
	}
}

func TestBuildWorkItem_CriteriaFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "criteria.txt")
	if err := os.WriteFile(path, []byte("builds\n\ntests pass\n"), 0o600); err != nil {
		t.Fatalf("write criteria file: %v", err)
	}

	wi, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", Task: "x", CriteriaFile: path,
	})
	if err != nil {
		t.Fatalf("buildWorkItem: %v", err)
	}
	if len(wi.AcceptanceCriteria) != 2 || wi.AcceptanceCriteria[0] != "builds" || wi.AcceptanceCriteria[1] != "tests pass" {
		t.Errorf("AcceptanceCriteria = %v, want [builds, tests pass] (blank line skipped)", wi.AcceptanceCriteria)
	}
}

func TestBuildWorkItem_CriteriaFlagAndFileAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "criteria.txt")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write criteria file: %v", err)
	}

	wi, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", Task: "x", Criteria: []string{"from-flag"}, CriteriaFile: path,
	})
	if err != nil {
		t.Fatalf("buildWorkItem: %v", err)
	}
	if len(wi.AcceptanceCriteria) != 2 || wi.AcceptanceCriteria[0] != "from-flag" || wi.AcceptanceCriteria[1] != "from-file" {
		t.Errorf("AcceptanceCriteria = %v, want [from-flag, from-file] (flag then file, appended)", wi.AcceptanceCriteria)
	}
}

func TestBuildWorkItem_MapsRoleAndFields(t *testing.T) {
	wi, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", Task: "do the thing", Criteria: []string{"builds", "tests pass"},
	})
	if err != nil {
		t.Fatalf("buildWorkItem: %v", err)
	}
	if wi.Role != "builder" || wi.TaskSpec != "do the thing" ||
		len(wi.AcceptanceCriteria) != 2 || wi.AcceptanceCriteria[0] != "builds" || wi.AcceptanceCriteria[1] != "tests pass" {
		t.Errorf("wi = %+v, want role/task/criteria mapped straight through", wi)
	}
}

// TestBuildWorkItem_MapsRepo: buildWorkItem must carry cfg.Repo straight
// onto workgraph.WorkItem.Repo (Phase 4 "real REPO tickets").
func TestBuildWorkItem_MapsRepo(t *testing.T) {
	wi, err := buildWorkItem(&workitemCreateConfig{
		Role: "builder", Task: "fix the bug", Criteria: []string{"builds"},
		Repo: "CarriedWorldUniverse/nexus",
	})
	if err != nil {
		t.Fatalf("buildWorkItem: %v", err)
	}
	if wi.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("Repo = %q, want CarriedWorldUniverse/nexus", wi.Repo)
	}
}

func TestRunWorkitemSubcommand_UnknownVerb(t *testing.T) {
	if got := runWorkitemSubcommand([]string{"bogus"}); got != 2 {
		t.Errorf("runWorkitemSubcommand(bogus) = %d, want 2", got)
	}
}

func TestRunWorkitemSubcommand_NoArgs(t *testing.T) {
	if got := runWorkitemSubcommand(nil); got != 2 {
		t.Errorf("runWorkitemSubcommand(nil) = %d, want 2", got)
	}
}

// TestParseWorkitemCreateArgs_Dedupe covers the --dedupe flag (default
// false, settable true), added for the drain-seeder's dedupe follow-up.
func TestParseWorkitemCreateArgs_Dedupe(t *testing.T) {
	cfg, err := parseWorkitemCreateArgs([]string{
		"--role", "builder", "--task", "x", "--criteria", "y", "--dedupe",
	})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if !cfg.Dedupe {
		t.Errorf("Dedupe = false, want true")
	}
}

func TestParseWorkitemCreateArgs_DedupeDefaultsFalse(t *testing.T) {
	cfg, err := parseWorkitemCreateArgs([]string{"--role", "builder", "--task", "x", "--criteria", "y"})
	if err != nil {
		t.Fatalf("parseWorkitemCreateArgs: %v", err)
	}
	if cfg.Dedupe {
		t.Errorf("Dedupe = true, want false default")
	}
}

// TestTaskSpecFirstLineMatches covers taskSpecFirstLineMatches's match/
// no-match/whitespace cases (--dedupe's pure matching helper).
func TestTaskSpecFirstLineMatches(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "exact match",
			a:    "Drain the shadow-queue: foo",
			b:    "Drain the shadow-queue: foo",
			want: true,
		},
		{
			name: "match ignoring rest-of-body differences",
			a:    "Drain the shadow-queue: foo\nsome details here",
			b:    "Drain the shadow-queue: foo\ndifferent details entirely",
			want: true,
		},
		{
			name: "match ignoring surrounding whitespace",
			a:    "  Drain the shadow-queue: foo  \n",
			b:    "Drain the shadow-queue: foo",
			want: true,
		},
		{
			name: "match ignoring leading/trailing blank lines",
			a:    "\n\nDrain the shadow-queue: foo\n",
			b:    "Drain the shadow-queue: foo",
			want: true, // TrimSpace strips the leading/trailing blank lines entirely before the first-line split.
		},
		{
			name: "no match, different task",
			a:    "Drain the shadow-queue: foo",
			b:    "Drain the shadow-queue: bar",
			want: false,
		},
		{
			name: "no match, empty vs non-empty",
			a:    "",
			b:    "Drain the shadow-queue: foo",
			want: false,
		},
		{
			name: "both empty match",
			a:    "",
			b:    "   \n  ",
			want: true,
		},
		{
			// Encodes part of the coordinator-reported live-failure
			// hypothesis: a description that comes back reformatted with
			// doubled/tabbed internal whitespace (same words, same single
			// line) must still match — normalizeTaskLine's strings.Fields
			// collapses any whitespace run to one space. (A reflow that
			// inserts hard newlines INSIDE the first line is not something
			// a first-line split can defend against — that's why
			// findDuplicateWorkItem prefers the narrower Summary field,
			// see TestFindDuplicateWorkItem_PrefersSummaryOverTaskSpec.)
			name: "match despite doubled/tabbed internal whitespace (same words, same line)",
			a:    "Drain the shadow-queue: list ready items in the shadow Jira queue and dispatch/triage each per the queue runbook",
			b:    "Drain the shadow-queue:  list ready items in the shadow Jira queue and\tdispatch/triage each  per the queue runbook",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskSpecFirstLineMatches(tc.a, tc.b); got != tc.want {
				t.Errorf("taskSpecFirstLineMatches(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestFindDuplicateWorkItem covers findDuplicateWorkItem's scan over
// ListReady's results for a first-line match.
func TestFindDuplicateWorkItem(t *testing.T) {
	wi := workgraph.WorkItem{TaskSpec: "Drain the shadow-queue: foo"}
	existing := []workgraph.WorkItem{
		{ID: "PROJ-1", TaskSpec: "Some other task"},
		{ID: "PROJ-2", TaskSpec: "Drain the shadow-queue: foo\nrest of body"},
	}
	if got := findDuplicateWorkItem(wi, existing); got != "PROJ-2" {
		t.Errorf("findDuplicateWorkItem = %q, want PROJ-2", got)
	}
}

func TestFindDuplicateWorkItem_NoMatch(t *testing.T) {
	wi := workgraph.WorkItem{TaskSpec: "Drain the shadow-queue: foo"}
	existing := []workgraph.WorkItem{
		{ID: "PROJ-1", TaskSpec: "Some other task"},
	}
	if got := findDuplicateWorkItem(wi, existing); got != "" {
		t.Errorf("findDuplicateWorkItem = %q, want empty (no match)", got)
	}
}

// TestFindDuplicateWorkItem_PrefersSummaryOverTaskSpec is the regression
// test for the coordinator-reported live failure: with NET-41/NET-42 both
// open for role builder and the seeder's --task text unchanged, --dedupe
// created NET-43 instead of skipping. Root cause per file:line evidence
// (see this ticket's report): no confirmed Description-round-trip mutation
// in the pinned ledger v0.1.4 (nexus/workgraph/adapter.go's GetWorkItem —
// TaskSpec: issue.GetDescription() — and the vendored ledger's toProtoIssue
// are a pure passthrough of the stored column), but Description/TaskSpec is
// a wide field with more surface for something downstream to reformat than
// Summary (a narrow field with one producer: CreateWorkItem's Summarize
// call). This test encodes that defensive posture directly: even when an
// existing item's TaskSpec has been mangled somehow (simulating any such
// future or unconfirmed transformation) so the fallback
// taskSpecFirstLineMatches would miss, a matching Summary must still find
// the duplicate.
func TestFindDuplicateWorkItem_PrefersSummaryOverTaskSpec(t *testing.T) {
	task := "Drain the shadow-queue: list ready items in the shadow Jira queue and dispatch/triage each per the queue runbook"
	wi := workgraph.WorkItem{TaskSpec: task}
	existing := []workgraph.WorkItem{
		{
			ID: "NET-41",
			// TaskSpec deliberately mangled relative to wi's — simulates an
			// unconfirmed round-trip transformation. Summary, however,
			// matches workgraph.Summarize(task) exactly, as CreateWorkItem
			// always derives it.
			TaskSpec: "Drain the shadow-queue: list ready items in the shadow\nJira queue and dispatch/triage each per the queue runbook — rendered",
			Summary:  workgraph.Summarize(task),
		},
	}
	if got := findDuplicateWorkItem(wi, existing); got != "NET-41" {
		t.Errorf("findDuplicateWorkItem = %q, want NET-41 (Summary match despite mangled TaskSpec)", got)
	}
}

// TestFindDuplicateWorkItem_SummaryMismatchNoFallback: once an existing
// item HAS a Summary, findDuplicateWorkItem trusts it exclusively for that
// item (Summary is the narrower, preferred signal) — it does not also
// fall back to a TaskSpec comparison that happens to match. A populated but
// differing Summary means "not the same task", full stop.
func TestFindDuplicateWorkItem_SummaryMismatchNoFallback(t *testing.T) {
	wi := workgraph.WorkItem{TaskSpec: "Drain the shadow-queue: foo"}
	existing := []workgraph.WorkItem{
		{ID: "PROJ-1", TaskSpec: "Drain the shadow-queue: foo", Summary: "a different summary entirely"},
	}
	if got := findDuplicateWorkItem(wi, existing); got != "" {
		t.Errorf("findDuplicateWorkItem = %q, want empty (populated Summary mismatch must not fall back to TaskSpec match)", got)
	}
}

// TestFindDuplicateWorkItem_DrainSeederFixture is the fixture-based
// regression test for the coordinator-reported live failure, using the
// exact drain-seeder task text and the shape workgraph.Client.ListReady
// actually returns for an already-dispatched item (Status/Summary/TaskSpec
// all populated by GetWorkItem — see workgraph/adapter_test.go's
// TestListReady_RoundTripsTaskSpecAndSummary for the end-to-end confirmation
// that a real create->transition->ListReady round trip through the fake
// ledger preserves both fields unchanged).
func TestFindDuplicateWorkItem_DrainSeederFixture(t *testing.T) {
	task := "Drain the shadow-queue: list ready items in the shadow Jira queue and dispatch/triage each per the queue runbook"
	candidate := workgraph.WorkItem{Role: "builder", TaskSpec: task}
	existing := []workgraph.WorkItem{
		{
			ID: "NET-41", Role: "builder", TaskSpec: task, Summary: workgraph.Summarize(task),
			Status: workgraph.StatusDispatched,
		},
	}
	if got := findDuplicateWorkItem(candidate, existing); got != "NET-41" {
		t.Errorf("findDuplicateWorkItem = %q, want NET-41", got)
	}
}

func TestRunWorkitemCreate_MissingLedgerAddr(t *testing.T) {
	os.Unsetenv("WORKGRAPH_LEDGER_ADDR")
	got := runWorkitemCreate([]string{"--role", "builder", "--task", "x", "--criteria", "y"})
	if got != 2 {
		t.Errorf("runWorkitemCreate without WORKGRAPH_LEDGER_ADDR = %d, want 2", got)
	}
}
