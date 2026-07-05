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

func TestRunWorkitemCreate_MissingLedgerAddr(t *testing.T) {
	os.Unsetenv("WORKGRAPH_LEDGER_ADDR")
	got := runWorkitemCreate([]string{"--role", "builder", "--task", "x", "--criteria", "y"})
	if got != 2 {
		t.Errorf("runWorkitemCreate without WORKGRAPH_LEDGER_ADDR = %d, want 2", got)
	}
}
