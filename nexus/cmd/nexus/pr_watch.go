// PR-lifecycle watcher (docs/network/PR-LIFECYCLE.md).
//
// `nexus pr-watch --repo owner/name [--repo ...] [--dry-run]
//                 [--personalities anvil,plumb,keel] [--max-rounds 3]
//                 [--limit 20] [--since 168h] [--gh-path gh]`
//
// One stateless pass of the PR-lifecycle state machine over each repo's
// open, non-draft PRs on builder/* branches. All lifecycle state is read
// from the PR itself (the `pr-lifecycle` marker in review/issue comments);
// the watcher keeps no store. Seeding goes through the same
// `workitem create` path the drain-seeder uses (incl. --dedupe), so a
// slow run never gets a concurrent duplicate.
//
// Reviews are pinned --personality ≠ every personality that authored a
// commit on the PR (self-review guard); fixes are seeded UNPINNED into
// the open queue (ownership rides the work item + branch — any builder
// can `cairn express` the existing line).
//
// Same polling-vs-webhook posture as triage-prs: gh CLI subprocess for
// reads, no public HTTP endpoint. The cairn-server webhook (repos we
// host) later feeds the same state machine.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// prLifecycleMarker is the machine-readable header of a lifecycle review
// comment — the watcher's entire memory. Emitted by reviewer runs, parsed
// here. Verdict ∈ {approved, changes-requested}; Head is the SHA reviewed.
type prLifecycleMarker struct {
	Verdict string
	Round   int
	Head    string
}

var prLifecycleMarkerRe = regexp.MustCompile(
	`<!--\s*pr-lifecycle:\s*verdict=(approved|changes-requested)\s+round=(\d+)\s+head=([0-9a-fA-F]{6,40})\s*-->`)

// parsePRLifecycleMarker extracts the marker from a comment body, if present.
func parsePRLifecycleMarker(body string) (prLifecycleMarker, bool) {
	m := prLifecycleMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return prLifecycleMarker{}, false
	}
	round, err := strconv.Atoi(m[2])
	if err != nil {
		return prLifecycleMarker{}, false
	}
	return prLifecycleMarker{Verdict: m[1], Round: round, Head: m[3]}, true
}

// String renders the marker in the canonical emit form (for reviewer briefs
// and tests).
func (m prLifecycleMarker) String() string {
	return fmt.Sprintf("<!-- pr-lifecycle: verdict=%s round=%d head=%s -->", m.Verdict, m.Round, m.Head)
}

// prWatchState is everything the state machine needs about one PR, all of it
// read from the PR (plus the queue's dedupe at seed time).
type prWatchState struct {
	Repo    string
	Number  int
	URL     string
	Branch  string
	HeadSHA string
	// Authors is the set of pool personalities that authored commits on the
	// PR (from <personality>-<role>@agents.carriedworld.com author emails).
	// The reviewer pin must avoid ALL of them.
	Authors map[string]bool
	// Marker is the highest-round pr-lifecycle marker found on the PR, nil
	// if the PR has never been lifecycle-reviewed.
	Marker *prLifecycleMarker
}

// prWatchAction is the one next step for a PR. Kind ∈ {seed-review,
// seed-fix, ready-to-merge, escalate, none}.
type prWatchAction struct {
	Kind   string
	Round  int    // review/fix round this action belongs to
	Reason string // human-readable, for logs / --dry-run
}

// decidePRWatchAction is the pure state machine of PR-LIFECYCLE.md §state
// machine. Draft/closed/non-builder PRs are filtered before this is called.
func decidePRWatchAction(st prWatchState, maxRounds int) prWatchAction {
	m := st.Marker
	switch {
	case m != nil && m.Verdict == "approved":
		return prWatchAction{Kind: "ready-to-merge", Round: m.Round, Reason: "review approved"}
	case m != nil && m.Round >= maxRounds:
		return prWatchAction{Kind: "escalate", Round: m.Round,
			Reason: fmt.Sprintf("round cap (%d) reached without approval", maxRounds)}
	case m == nil:
		return prWatchAction{Kind: "seed-review", Round: 1, Reason: "no lifecycle review yet"}
	case !strings.HasPrefix(st.HeadSHA, m.Head) && !strings.HasPrefix(m.Head, st.HeadSHA):
		// Head advanced past the reviewed SHA (prefix-tolerant: markers may
		// carry short SHAs) → the fixer pushed; re-review the new head.
		return prWatchAction{Kind: "seed-review", Round: m.Round + 1,
			Reason: fmt.Sprintf("head advanced past reviewed %s", m.Head)}
	case m.Verdict == "changes-requested":
		// Same head the reviewer saw → outstanding items not yet addressed.
		// Dedupe at seed time keeps this idempotent while a fix is in flight.
		return prWatchAction{Kind: "seed-fix", Round: m.Round, Reason: "changes requested, head unchanged"}
	default:
		return prWatchAction{Kind: "none", Round: m.Round, Reason: "nothing to do"}
	}
}

// personalityFromAgentEmail maps anvil-builder@agents.carriedworld.com ->
// "anvil". Non-agent emails map to "".
func personalityFromAgentEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || domain != "agents.carriedworld.com" {
		return ""
	}
	personality, _, _ := strings.Cut(local, "-")
	return personality
}

// pickReviewer returns the first personality not in authors, "" if none.
func pickReviewer(personalities []string, authors map[string]bool) string {
	for _, p := range personalities {
		p = strings.TrimSpace(p)
		if p != "" && !authors[p] {
			return p
		}
	}
	return ""
}

// reviewBrief is the seeded reviewer task: diff-grounded review + the
// comment contract of PR-LIFECYCLE.md.
func reviewBrief(st prWatchState, round int) (task, criteria string) {
	// The lifecycle verdict is posted as a PR COMMENT (gh pr comment), never a
	// formal GitHub review: all pool agents share the nexus-cw identity and
	// GitHub hard-rejects reviewing your own PR (422). The verdict's authority
	// is our pr-lifecycle marker, which the watcher reads from comments too.
	task = fmt.Sprintf(
		"Review pull request %s (repo %s, branch %s, head %s) — lifecycle review round %d. "+
			"Read the diff with: gh pr diff %d --repo %s. Judge the DIFF (correctness, security, house style), not the description. "+
			"Then post ONE comment with gh pr comment %d --repo %s --body-file <file you write>, whose body starts with the marker line "+
			"\"<!-- pr-lifecycle: verdict=<approved|changes-requested> round=%d head=%s -->\" (choose ONE verdict) followed by an \"## Outstanding\" checklist. "+
			"Every outstanding item MUST be observable: file:line — defect — required change — how to verify from the diff. "+
			"An approval has an empty Outstanding section. Include an \"## Context\" section with repo and branch. "+
			"Do NOT use gh pr review (self-review is rejected), do NOT merge, do NOT push commits, do NOT open PRs.",
		st.URL, st.Repo, st.Branch, st.HeadSHA, round, st.Number, st.Repo, st.Number, st.Repo, round, st.HeadSHA)
	criteria = fmt.Sprintf(
		"A comment exists on PR #%d in %s whose body contains a pr-lifecycle marker with round=%d and head=%s, "+
			"and every Outstanding item names a file location and a verify clause.",
		st.Number, st.Repo, round, st.HeadSHA)
	return task, criteria
}

// fixBrief is the seeded fix task: a pointer at the PR, which carries all
// the state (the review comment is the fix contract).
func fixBrief(st prWatchState, round int) (task, criteria string) {
	task = fmt.Sprintf(
		"Resolve the outstanding review items on %s (repo %s, branch %s) — fix round %d. "+
			"Read the latest pr-lifecycle review comment on the PR; every unchecked Outstanding item is your task list. "+
			"The branch already exists: after the harness provisions your working copy, work on line %s and make each item verifiable, "+
			"then cairn commit and cairn push per the harness instruction. Tick each resolved box on the PR (gh pr comment or edit), "+
			"or reply per-item with grounds if you believe an item is wrong. "+
			"Do NOT merge, do NOT open a new PR, do NOT commit built binaries.",
		st.URL, st.Repo, st.Branch, round, st.Branch)
	criteria = fmt.Sprintf(
		"Fix commits are pushed to %s in %s addressing the round-%d Outstanding items, and each item is ticked or answered on PR #%d; no binary files in the diff.",
		st.Branch, st.Repo, round, st.Number)
	return task, criteria
}

func runPRWatchSubcommand(args []string) int {
	fs := flag.NewFlagSet("pr-watch", flag.ContinueOnError)
	var repos repeatableStringFlag
	fs.Var(&repos, "repo", "GitHub repo as owner/name; repeat for multiple repos (required)")
	dryRun := fs.Bool("dry-run", false, "decide + print actions, seed nothing, label nothing")
	personalitiesCSV := fs.String("personalities", strings.Join(aspects.WorkerPersonalities, ","), "comma-separated pool personalities eligible to review (default: the registered pool)")
	maxRounds := fs.Int("max-rounds", 3, "review rounds before a PR escalates to the operator")
	limit := fs.Int("limit", 20, "max open PRs per repo per pass")
	since := fs.Duration("since", 14*24*time.Hour, "only consider PRs updated in the last N")
	ghPath := fs.String("gh-path", "gh", "path to the gh CLI binary")
	branchPrefix := fs.String("branch-prefix", "builder/", "only PRs whose head branch has this prefix")
	onlyPR := fs.Int("pr", 0, "act on this PR number only (0 = all) — for staged rollouts")
	emitBriefs := fs.String("emit-briefs", "", "dry-run only: also write the full task/criteria of each would-seed action to DIR as <pr>-<kind>-{task,criteria}.txt (for manual staged seeding via --task-file/--criteria-file)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "pr-watch: at least one --repo is required")
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	personalities := strings.Split(*personalitiesCSV, ",")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	exitCode := 0
	for _, repo := range repos {
		prs, err := listWatchPRs(ctx, *ghPath, repo, *since, *limit)
		if err != nil {
			log.Error("pr-watch: list PRs", "repo", repo, "err", err)
			exitCode = 1
			continue
		}
		for _, pr := range prs {
			if pr.Draft || !strings.HasPrefix(pr.Branch, *branchPrefix) {
				continue
			}
			if *onlyPR != 0 && pr.Number != *onlyPR {
				continue
			}
			st, err := loadPRWatchState(ctx, *ghPath, repo, pr)
			if err != nil {
				log.Error("pr-watch: load PR state", "repo", repo, "pr", pr.Number, "err", err)
				exitCode = 1
				continue
			}
			action := decidePRWatchAction(st, *maxRounds)
			if *dryRun && *emitBriefs != "" {
				if err := emitBriefFiles(*emitBriefs, st, action); err != nil {
					log.Error("pr-watch: emit briefs", "pr", pr.Number, "err", err)
					exitCode = 1
				}
			}
			if err := applyPRWatchAction(ctx, *ghPath, st, action, personalities, *dryRun, log); err != nil {
				log.Error("pr-watch: apply", "repo", repo, "pr", pr.Number, "action", action.Kind, "err", err)
				exitCode = 1
			}
		}
	}
	return exitCode
}

// watchPR is the gh pr list projection pr-watch needs.
type watchPR struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Branch  string `json:"headRefName"`
	HeadSHA string `json:"headRefOid"`
	Draft   bool   `json:"isDraft"`
}

func listWatchPRs(ctx context.Context, ghPath, repo string, since time.Duration, limit int) ([]watchPR, error) {
	cutoff := time.Now().Add(-since).UTC().Format("2006-01-02")
	out, err := ghJSON(ctx, ghPath, "pr", "list",
		"--repo", repo, "--state", "open",
		"--search", "updated:>"+cutoff,
		"--json", "number,url,headRefName,headRefOid,isDraft",
		"--limit", fmt.Sprintf("%d", limit))
	if err != nil {
		return nil, err
	}
	var prs []watchPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("decode gh pr list: %w", err)
	}
	return prs, nil
}

// loadPRWatchState reads the lifecycle state OFF the PR: commit author
// personalities + the highest-round pr-lifecycle marker across reviews and
// issue comments.
func loadPRWatchState(ctx context.Context, ghPath, repo string, pr watchPR) (prWatchState, error) {
	st := prWatchState{
		Repo: repo, Number: pr.Number, URL: pr.URL,
		Branch: pr.Branch, HeadSHA: pr.HeadSHA,
		Authors: map[string]bool{},
	}
	// Commit authors → personalities (self-review guard).
	out, err := ghJSON(ctx, ghPath, "api", fmt.Sprintf("repos/%s/pulls/%d/commits", repo, pr.Number))
	if err != nil {
		return st, fmt.Errorf("list commits: %w", err)
	}
	var commits []struct {
		Commit struct {
			Author struct {
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(out, &commits); err != nil {
		return st, fmt.Errorf("decode commits: %w", err)
	}
	for _, c := range commits {
		if p := personalityFromAgentEmail(c.Commit.Author.Email); p != "" {
			st.Authors[p] = true
		}
	}
	// Marker: scan PR reviews and issue comments; keep the highest round.
	bodies, err := prCommentBodies(ctx, ghPath, repo, pr.Number)
	if err != nil {
		return st, err
	}
	for _, b := range bodies {
		if m, ok := parsePRLifecycleMarker(b); ok {
			if st.Marker == nil || m.Round > st.Marker.Round {
				mm := m
				st.Marker = &mm
			}
		}
	}
	return st, nil
}

func prCommentBodies(ctx context.Context, ghPath, repo string, number int) ([]string, error) {
	var bodies []string
	for _, endpoint := range []string{
		fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number),
		fmt.Sprintf("repos/%s/issues/%d/comments", repo, number),
	} {
		out, err := ghJSON(ctx, ghPath, "api", endpoint)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", endpoint, err)
		}
		var items []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(out, &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", endpoint, err)
		}
		for _, it := range items {
			bodies = append(bodies, it.Body)
		}
	}
	return bodies, nil
}

// applyPRWatchAction executes (or, in dry-run, prints) the decided action.
// Seeding goes through runWorkitemCreate — the same path the drain-seeder
// uses — so --dedupe, role validation, and ledger wiring stay single-sourced.
func applyPRWatchAction(ctx context.Context, ghPath string, st prWatchState, action prWatchAction, personalities []string, dryRun bool, log *slog.Logger) error {
	prefix := fmt.Sprintf("pr-watch %s#%d [%s r%d]", st.Repo, st.Number, action.Kind, action.Round)
	switch action.Kind {
	case "none":
		log.Info(prefix, "reason", action.Reason)
		return nil
	case "seed-review":
		reviewer := pickReviewer(personalities, st.Authors)
		if reviewer == "" {
			return fmt.Errorf("no eligible reviewer personality (authors=%v)", st.Authors)
		}
		task, criteria := reviewBrief(st, action.Round)
		// Deliberately NO --repo: a review's deliverable is a COMMENT, not a
		// branch/PR. Repo-less items dispatch respond-only (no workspace, no
		// branch instruction, no PR gate); the worker still gets gh bridged.
		args := []string{"--role", "reviewer", "--personality", reviewer,
			"--task", task, "--criteria", criteria, "--dedupe"}
		if dryRun {
			log.Info(prefix, "reason", action.Reason, "would-seed", "reviewer", "personality", reviewer)
			fmt.Printf("%s DRY: workitem create %s\n", prefix, summarizeArgs(args))
			return nil
		}
		return seedWorkItem(args)
	case "seed-fix":
		task, criteria := fixBrief(st, action.Round)
		args := []string{"--role", "builder",
			"--repo", st.Repo, "--task", task, "--criteria", criteria, "--dedupe"}
		if dryRun {
			log.Info(prefix, "reason", action.Reason, "would-seed", "fix (unpinned)")
			fmt.Printf("%s DRY: workitem create %s\n", prefix, summarizeArgs(args))
			return nil
		}
		return seedWorkItem(args)
	case "ready-to-merge", "escalate":
		label := map[string]string{"ready-to-merge": "ready-to-merge", "escalate": "escalated"}[action.Kind]
		if dryRun {
			log.Info(prefix, "reason", action.Reason, "would-label", label, "would-notify", "operator")
			fmt.Printf("%s DRY: label %q + notify operator\n", prefix, label)
			return nil
		}
		if out, err := exec.CommandContext(ctx, ghPath, "pr", "edit",
			fmt.Sprintf("%d", st.Number), "--repo", st.Repo, "--add-label", label).CombinedOutput(); err != nil {
			return fmt.Errorf("label %s: %v: %s", label, err, strings.TrimSpace(string(out)))
		}
		// Notification seam: the label is the durable signal; a chat-channel
		// notification rides the comms-digest path (PR-LIFECYCLE.md §terminal)
		// and is wired in the live phase.
		log.Info(prefix, "labeled", label, "reason", action.Reason)
		return nil
	default:
		return fmt.Errorf("unknown action %q", action.Kind)
	}
}

// emitBriefFiles writes the full task/criteria for a would-seed action so an
// operator (or a staged rollout) can seed manually with --task-file /
// --criteria-file, guaranteed byte-identical to what live seeding would send.
func emitBriefFiles(dir string, st prWatchState, action prWatchAction) error {
	var task, criteria string
	switch action.Kind {
	case "seed-review":
		task, criteria = reviewBrief(st, action.Round)
	case "seed-fix":
		task, criteria = fixBrief(st, action.Round)
	default:
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := fmt.Sprintf("%d-%s", st.Number, action.Kind)
	if err := os.WriteFile(dir+"/"+base+"-task.txt", []byte(task), 0o644); err != nil {
		return err
	}
	return os.WriteFile(dir+"/"+base+"-criteria.txt", []byte(criteria), 0o644)
}

// seedWorkItem funnels through the existing workitem-create CLI path.
func seedWorkItem(args []string) error {
	if code := runWorkitemCreate(args); code != 0 {
		return fmt.Errorf("workitem create exited %d", code)
	}
	return nil
}

// summarizeArgs renders seeding args for dry-run output with long values
// truncated (task/criteria bodies are printed elsewhere in full via logs).
func summarizeArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if len(a) > 90 {
			a = a[:87] + "..."
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

func ghJSON(ctx context.Context, ghPath string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ghPath, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
