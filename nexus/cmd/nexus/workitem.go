// `nexus workitem create` — a first-class subcommand for filing a ledger
// work item, replacing the hand-built Go binary (workgraph.CreateWorkItem
// called directly) that was the only way to seed one before this. Same
// wiring as the orchestrator's own ledger dial (cmd/nexus/orchestrator_wiring.go
// buildWorkgraphClient): WORKGRAPH_LEDGER_ADDR + WORKGRAPH_TLS_* env, same
// org/subject/project env-and-default convention.
//
//	nexus workitem create --role <role> {--task <text> | --task-file <path>}
//	                       [--criteria <text> ...] [--criteria-file <path>]
//	                       [--repo <owner/name>]
//	                       [--org <org>] [--subject <subject>] [--project <project>]
//	                       [--dedupe]
//
// Prints the created ledger issue key (work item id) to stdout on success.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"google.golang.org/grpc"
)

// runWorkitemSubcommand dispatches `nexus workitem <verb> [...]`.
func runWorkitemSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus workitem <create>")
		return 2
	}
	switch args[0] {
	case "create":
		return runWorkitemCreate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown workitem subcommand %q (expected: create)\n", args[0])
		return 2
	}
}

// workitemCreateConfig holds `nexus workitem create`'s parsed flags, prior
// to resolving --task/--task-file and --criteria/--criteria-file into the
// workgraph.WorkItem it maps to. Separated from flag.FlagSet so
// parseWorkitemCreateArgs and buildWorkItem are both pure functions,
// testable without a live ledger (see workers_test.go's split between
// listWorkerStatusRows and printWorkerStatusTable for the same shape).
type workitemCreateConfig struct {
	Role         string
	Task         string
	TaskFile     string
	Criteria     []string
	CriteriaFile string
	Repo         string
	Org          string
	Subject      string
	Project      string
	Dedupe       bool
}

// parseWorkitemCreateArgs parses `nexus workitem create`'s flags. Flags only
// — no positional arguments — so the flag package's stop-at-first-non-flag
// behavior (positionals must follow flags, never precede them) never bites
// here.
func parseWorkitemCreateArgs(args []string) (*workitemCreateConfig, error) {
	fs := flag.NewFlagSet("workitem create", flag.ContinueOnError)
	role := fs.String("role", "", "assignee_aspect / pool role this work item is for, e.g. builder (required)")
	task := fs.String("task", "", "task spec text (mutually exclusive with --task-file; one required)")
	taskFile := fs.String("task-file", "", "path to a file holding the task spec text (mutually exclusive with --task)")
	var criteria repeatableStringFlag
	fs.Var(&criteria, "criteria", "an acceptance-criteria line; repeat for multiple (mutually exclusive with --criteria-file)")
	criteriaFile := fs.String("criteria-file", "", "path to a file with one acceptance-criteria line per line (mutually exclusive with --criteria)")
	repo := fs.String("repo", "", "git repo (owner/name) this work item's builder should check out/branch/PR against (Phase 4, real REPO tickets); empty = respond-only work, no repo/branch/PR gate")
	org := fs.String("org", envOrDefault("WORKGRAPH_ORG", workgraph.DefaultOrg), "cwb-org presented to the ledger (default: WORKGRAPH_ORG env, then workgraph.DefaultOrg)")
	subject := fs.String("subject", envOrDefault("WORKGRAPH_SUBJECT", defaultWorkitemSubject), "cwb-subject presented to the ledger (default: WORKGRAPH_SUBJECT env, then \""+defaultWorkitemSubject+"\")")
	project := fs.String("project", envOrDefault("WORKGRAPH_PROJECT", workgraph.DefaultProject), "ledger project key the work item is filed under (default: WORKGRAPH_PROJECT env, then workgraph.DefaultProject)")
	dedupe := fs.Bool("dedupe", false, "before creating, skip (exit 0) if an existing ready/in-flight item for --role already has the same task-spec first line (see taskSpecFirstLineMatches) — for cron-seeded recurring tasks that must not pile up concurrent duplicates")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return &workitemCreateConfig{
		Role:         *role,
		Task:         *task,
		TaskFile:     *taskFile,
		Criteria:     []string(criteria),
		CriteriaFile: *criteriaFile,
		Repo:         *repo,
		Org:          *org,
		Subject:      *subject,
		Project:      *project,
		Dedupe:       *dedupe,
	}, nil
}

// defaultWorkitemSubject is the cwb-subject `nexus workitem create` presents
// to the ledger absent --subject/WORKGRAPH_SUBJECT — the same default the
// orchestrator's own ledger dial uses (buildWorkgraphClient), since a
// seeded work item and an orchestrator-created one are accountable to the
// same actor.
const defaultWorkitemSubject = "nexus-orchestrator"

// buildWorkItem resolves cfg's --task/--task-file and --criteria/
// --criteria-file choices into a workgraph.WorkItem ready for
// workgraph.Client.CreateWorkItem. Pure — no I/O beyond the file reads
// cfg's flags name, so it's unit-testable without a ledger.
func buildWorkItem(cfg *workitemCreateConfig) (workgraph.WorkItem, error) {
	if cfg.Role == "" {
		return workgraph.WorkItem{}, errors.New("--role is required")
	}
	task, err := resolveOneOf("task", cfg.Task, cfg.TaskFile, readFileTrimmed)
	if err != nil {
		return workgraph.WorkItem{}, err
	}
	criteria, err := resolveCriteria(cfg.Criteria, cfg.CriteriaFile)
	if err != nil {
		return workgraph.WorkItem{}, err
	}
	return workgraph.WorkItem{
		Role:               cfg.Role,
		TaskSpec:           task,
		AcceptanceCriteria: criteria,
		Repo:               cfg.Repo,
	}, nil
}

// resolveOneOf picks exactly one of inline/filePath for a required text
// flag pair (e.g. --task / --task-file), mirroring credential.go's
// readBundle exclusivity check for --bundle/--bundle-file/--bundle-stdin.
// name is the flag's base name, used only in error messages.
func resolveOneOf(name, inline, filePath string, readFile func(string) (string, error)) (string, error) {
	switch {
	case inline != "" && filePath != "":
		return "", fmt.Errorf("--%s and --%s-file are mutually exclusive — pick one", name, name)
	case inline != "":
		return inline, nil
	case filePath != "":
		return readFile(filePath)
	default:
		return "", fmt.Errorf("--%s or --%s-file is required", name, name)
	}
}

// resolveCriteria merges --criteria (repeatable) and --criteria-file (one
// line per line, blank lines skipped) into the final acceptance-criteria
// list. Unlike --task/--task-file, both may be set — the lists append
// rather than conflict, since "repeat --criteria AND also read a file of
// more" is a reasonable v1 shape and there's no ambiguity to resolve (unlike
// --task inline vs --task-file, which pick between two candidate values for
// ONE field).
func resolveCriteria(inline []string, filePath string) ([]string, error) {
	out := append([]string(nil), inline...)
	if filePath != "" {
		lines, err := readFileLines(filePath)
		if err != nil {
			return nil, fmt.Errorf("--criteria-file: %w", err)
		}
		out = append(out, lines...)
	}
	if len(out) == 0 {
		return nil, errors.New("--criteria (repeatable) or --criteria-file is required (at least one acceptance criterion)")
	}
	return out, nil
}

// readFileTrimmed reads path and trims a single trailing newline (the
// common shape of a task-spec file written by an editor), matching how
// --task inline text has no trailing newline either.
func readFileTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// readFileLines reads path and splits it into non-empty, trimmed lines —
// one acceptance criterion per line, per --criteria-file's contract.
func readFileLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// taskSpecFirstLineMatches reports whether a and b's task specs name the
// same task, per --dedupe's fallback semantics (see findDuplicateWorkItem —
// this is the fallback used only when an existing item's Summary is
// unavailable): compare only the first line (a TaskSpec may run to many
// lines — e.g. full task prose — but a recurring cron-seeded task's
// identity lives in its first line, see workgraph.Summarize), each side
// normalized via normalizeTaskLine so leading/trailing whitespace, a
// trailing newline, or the description having been reflowed/rewrapped
// somewhere between write and read (hard newlines or repeated spaces
// inserted at wrap points, the words themselves unchanged) never causes a
// false mismatch.
func taskSpecFirstLineMatches(a, b string) bool {
	firstLine := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		return normalizeTaskLine(s)
	}
	return firstLine(a) == firstLine(b)
}

// normalizeTaskLine collapses any run of whitespace (spaces, tabs,
// newlines) to a single space and trims the ends, via strings.Fields —
// guards the fallback TaskSpec comparison (taskSpecFirstLineMatches)
// against a description that survives a read-back reflowed/rewrapped
// (e.g. a markdown-rendering or word-wrap pass reinserting whitespace at
// different points) rather than byte-identical to what was written.
func normalizeTaskLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// findDuplicateWorkItem scans existing (as returned by workgraph.Client.
// ListReady for wi.Role — queued + dispatched/in-flight items, see
// ListReady's doc comment) for one that names the same task as wi.
//
// Primary comparison: wi's would-be Summary (workgraph.Summarize(wi.
// TaskSpec) — wi is a not-yet-created draft, so it has no ledger-assigned
// Summary of its own yet) against each existing item's real WorkItem.Summary
// (GetWorkItem's read of the ledger issue's summary column). Summary is
// preferred over TaskSpec/Description: it's a narrow, single-producer field
// (CreateWorkItem always derives it via the same Summarize call, never from
// caller input), so it's less exposed than TaskSpec/Description to
// reformatting by anything else that might touch the issue between write and
// read.
//
// Fallback (only when an existing item's Summary is empty — e.g. very old
// ledger data from before this field existed): taskSpecFirstLineMatches
// against TaskSpec/Description directly, whitespace-normalized.
//
// Returns the first match's id, or "" if none.
func findDuplicateWorkItem(wi workgraph.WorkItem, existing []workgraph.WorkItem) string {
	wantSummary := workgraph.Summarize(wi.TaskSpec)
	for _, e := range existing {
		if e.Summary != "" {
			if e.Summary == wantSummary {
				return e.ID
			}
			continue
		}
		if taskSpecFirstLineMatches(wi.TaskSpec, e.TaskSpec) {
			return e.ID
		}
	}
	return ""
}

// runWorkitemCreate parses args, dials the sovereign ledger (same env
// convention as the orchestrator's buildWorkgraphClient: WORKGRAPH_LEDGER_ADDR
// + WORKGRAPH_TLS_*), files the work item, and prints its id.
func runWorkitemCreate(args []string) int {
	cfg, err := parseWorkitemCreateArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wi, err := buildWorkItem(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workitem create: %v\n", err)
		return 2
	}

	addr := os.Getenv("WORKGRAPH_LEDGER_ADDR")
	if addr == "" {
		fmt.Fprintln(os.Stderr, "workitem create: WORKGRAPH_LEDGER_ADDR is required")
		return 2
	}
	dialCreds, err := workgraph.DialCreds()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workitem create: workgraph TLS config: %v\n", err)
		return 1
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		fmt.Fprintf(os.Stderr, "workitem create: dial %q: %v\n", addr, err)
		return 1
	}
	defer conn.Close()

	client := workgraph.New(conn, cfg.Org, cfg.Subject, cfg.Project)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.EnsureProject(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "workitem create: ensure project %q/%q: %v\n", cfg.Org, cfg.Project, err)
		return 1
	}
	if cfg.Dedupe {
		existing, err := client.ListReady(ctx, wi.Role, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "workitem create: dedupe check: %v\n", err)
			return 1
		}
		if dupID := findDuplicateWorkItem(wi, existing); dupID != "" {
			fmt.Fprintf(os.Stdout, "skipped: %s already open\n", dupID)
			return 0
		}
	}
	id, err := client.CreateWorkItem(ctx, wi)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workitem create: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, id)
	return 0
}
