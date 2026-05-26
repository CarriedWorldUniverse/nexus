// Auto-close NEX-* tickets when their referencing PR merges
// (NEX-303). Same operational shape as NEX-247's triage-tickets:
// polling subcommand, no public HTTP endpoint required, runs
// out of cron / launchd / on-demand.
//
// `nexus close-merged-tickets --credential <jira> --repo owner/name
//                              [--since 24h] [--limit 50] [--dry-run]
//                              [--data-dir DIR] [--timeout-s 120]`
//
// Per cycle:
//   1. `gh pr list --state merged --search "merged:>$SINCE" --json ...`
//      for each --repo
//   2. parse NEX-\d+ from each PR's title + body (case-insensitive)
//   3. for each matched ticket, fetch current status
//   4. if NOT Done: transition + post "Closed by <PR url>" comment
//   5. if already Done: skip silently (idempotent re-runs)
//
// De-dupe is implicit: checking status before transitioning means
// re-runs against a window with already-closed tickets make zero
// Jira writes. No marker comment needed (status check IS the dedup).
//
// Why polling not webhook: same constraint as NEX-247 — no public
// HTTP endpoint until interchange gateway respec lands.

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
	"sort"
	"strings"
	"time"
)

// titleLeadPattern matches the convention "NEX-247: …" at the
// START of a PR title with a colon IMMEDIATELY after the key —
// the existing PR-naming convention for "this PR implements the
// whole ticket".
//
// Deliberately excludes partial-slice forms like "NEX-247 Slice 2:"
// or "NEX-297 L1:" — those PRs ship one slice of a multi-slice
// ticket, and closing the parent ticket on the first slice's
// merge would corrupt state (verified during NEX-303 dry-run
// 2026-05-26: NEX-297 was being flagged for close because the
// L1/L2/L3 PR titles tripped the looser ` ?` matcher; L3 is
// still pending).
//
// Operator with a partial-slice PR who wants close-on-this-PR
// behaviour can write "Closes NEX-N" in the body — that path
// fires via bodyCloseKeywordPattern.
var titleLeadPattern = regexp.MustCompile(`(?i)^\s*NEX-(\d+):`)

// bodyCloseKeywordPattern matches the GitHub-style closing-keyword
// + ticket-key shape in PR bodies: "Closes NEX-247", "fixes nex-100",
// "Resolves: NEX-50", "Closed NEX-99". Mirrors GitHub's own auto-
// close behaviour but for Jira refs.
//
// Bare mentions ("see NEX-100", "related to NEX-200", "supersedes
// NEX-3") DO NOT match — those are context cross-refs, not close
// intents. Dry-run against the past week of PRs (2026-05-26) showed
// the naive regex catches ~3x too many tickets because PR
// descriptions routinely cross-reference siblings + parent epics
// for context. Closing them would silently corrupt ticket state.
//
// Allowed keywords: close, closes, closed, fix, fixes, fixed,
// resolve, resolves, resolved. Optional `:` between keyword and
// key. One whitespace required (prevents "closesNEX-1" false-
// positive but a typo'd "closes  NEX-1" is fine).
var bodyCloseKeywordPattern = regexp.MustCompile(`(?i)\b(?:close[ds]?|fix(?:e[ds])?|resolve[ds]?)\s*:?\s+NEX-(\d+)\b`)

// repeatableStringFlag implements flag.Value for collecting --repo N
// times into a slice. Lets operator specify multiple repos in one
// invocation without ad-hoc string-splitting on commas.
type repeatableStringFlag []string

func (r *repeatableStringFlag) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

func (r *repeatableStringFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func runCloseMergedTicketsSubcommand(args []string) int {
	fs := flag.NewFlagSet("close-merged-tickets", flag.ContinueOnError)
	jiraCred := fs.String("credential", "", "name of a kind=jira credential in the nexus credentials store (required)")
	var repos repeatableStringFlag
	fs.Var(&repos, "repo", "GitHub repo as owner/name; repeat flag for multiple repos (required)")
	since := fs.Duration("since", 24*time.Hour, "look at PRs merged in the last N (e.g. 24h, 72h)")
	limit := fs.Int("limit", 50, "max merged PRs to inspect per repo per run")
	dryRun := fs.Bool("dry-run", false, "list what WOULD close without writing")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 120, "wall-clock budget for the whole run in seconds")
	ghPath := fs.String("gh-path", "gh", "path to the gh CLI binary (default: gh on PATH)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *jiraCred == "" || len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "close-merged-tickets: --credential and at least one --repo are required")
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutS)*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, cleanup, code := openCredentialsStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	jc, err := loadJiraClient(ctx, store, *jiraCred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "close-merged-tickets: load jira credential: %v\n", err)
		return 1
	}

	stats := closeStats{}
	for _, repo := range repos {
		prs, err := listMergedPRs(ctx, *ghPath, repo, *since, *limit)
		if err != nil {
			log.Warn("close-merged-tickets: list merged PRs failed",
				"repo", repo, "err", err)
			stats.errored++
			continue
		}
		log.Info("close-merged-tickets: PRs to inspect",
			"repo", repo, "count", len(prs), "since", since.String())
		for _, pr := range prs {
			processPR(ctx, jc, pr, *dryRun, &stats, log)
		}
	}

	fmt.Printf("repos_scanned: %d  prs_inspected: %d  tickets_found: %d  closed: %d  already_done: %d  errored: %d  dry_run: %v\n",
		len(repos), stats.prsInspected, stats.ticketsFound, stats.closed, stats.alreadyDone, stats.errored, *dryRun)
	if stats.errored > 0 {
		return 1
	}
	return 0
}

// mergedPR is the projection from `gh pr list` we care about for
// auto-close: enough to find ticket references + post a useful
// "closed by" comment with the PR url.
type mergedPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Repo   string `json:"-"` // populated by listMergedPRs, not from gh
}

// closeStats accumulates the per-run counters for the summary line.
type closeStats struct {
	prsInspected int
	ticketsFound int
	closed       int
	alreadyDone  int
	errored      int
}

// listMergedPRs shells out to `gh pr list --state merged --search
// "merged:>$SINCE" --json ...` and returns the parsed PRs. Uses gh
// rather than the GitHub REST API directly because the operator
// already has gh authenticated (it's the auth path for their
// merge workflow) — no new credential to manage.
func listMergedPRs(ctx context.Context, ghPath, repo string, since time.Duration, limit int) ([]mergedPR, error) {
	cutoff := time.Now().Add(-since).UTC().Format("2006-01-02")
	args := []string{
		"pr", "list",
		"--repo", repo,
		"--state", "merged",
		"--search", "merged:>" + cutoff,
		"--json", "number,title,body,url",
		"--limit", fmt.Sprintf("%d", limit),
	}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// gh's stderr is usually the useful diagnostic.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("gh pr list failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []mergedPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}
	for i := range prs {
		prs[i].Repo = repo
	}
	return prs, nil
}

// processPR finds CLOSE-INTENT NEX-* references in one PR + closes
// each referenced ticket (unless already Done). Bare cross-refs do
// NOT fire close — see extractCloseIntentKeys for the precise
// match-shape rationale.
func processPR(ctx context.Context, jc *triageJiraClient, pr mergedPR, dryRun bool, stats *closeStats, log *slog.Logger) {
	stats.prsInspected++
	keys := extractCloseIntentKeys(pr.Title, pr.Body)
	if len(keys) == 0 {
		return
	}
	stats.ticketsFound += len(keys)
	for _, key := range keys {
		closeOne(ctx, jc, key, pr, dryRun, stats, log)
	}
}

// closeOne handles the per-ticket transition + comment with the
// status-check dedup. Stat counts are mutated directly so the
// caller's summary line stays accurate even when individual
// tickets error.
func closeOne(ctx context.Context, jc *triageJiraClient, key string, pr mergedPR, dryRun bool, stats *closeStats, log *slog.Logger) {
	status, err := jc.getStatus(ctx, key)
	if err != nil {
		log.Warn("close-merged-tickets: get status failed",
			"key", key, "pr", pr.URL, "err", err)
		stats.errored++
		return
	}
	if status == "Done" {
		log.Debug("close-merged-tickets: already done, skipping",
			"key", key, "pr", pr.URL)
		stats.alreadyDone++
		return
	}
	if dryRun {
		log.Info("close-merged-tickets: would close",
			"key", key, "current_status", status, "pr", pr.URL, "repo", pr.Repo)
		stats.closed++ // count what we WOULD close so the summary is meaningful
		return
	}
	comment := fmt.Sprintf("Closed by merged PR %s (auto-close via nexus close-merged-tickets)", pr.URL)
	if err := jc.transitionTo(ctx, key, "Done", comment); err != nil {
		log.Warn("close-merged-tickets: transition failed",
			"key", key, "pr", pr.URL, "err", err)
		stats.errored++
		return
	}
	log.Info("close-merged-tickets: closed",
		"key", key, "from_status", status, "pr", pr.URL, "repo", pr.Repo)
	stats.closed++
}

// extractCloseIntentKeys returns unique NEX-* keys (uppercased)
// that this PR INTENDS to close. Two extraction sources:
//
//   - title: NEX-N at the start of the title (existing PR-naming
//     convention — "NEX-247: TicketTriage classifier ...")
//   - body:  GitHub-style closing keywords ("Closes NEX-247",
//     "fixes nex-100", "Resolves: NEX-50")
//
// Bare body mentions ("see NEX-247", "related to NEX-243",
// "supersedes NEX-100") DO NOT match. PR descriptions routinely
// cross-reference sibling tickets, parent epics, and unfixed
// root-cause tickets for context; closing them would silently
// corrupt ticket state. Verified during NEX-303 dry-run 2026-05-26:
// the naive any-mention regex caught 33 candidates including 4
// false-positives (epic parent, two siblings, an unfixed root-
// cause); the close-intent regex correctly narrows to the ~one
// genuinely-closed ticket per PR.
//
// Sorted for deterministic ordering across callers / logs / tests.
func extractCloseIntentKeys(title, body string) []string {
	seen := make(map[string]struct{}, 4)
	if m := titleLeadPattern.FindStringSubmatch(title); m != nil {
		seen["NEX-"+m[1]] = struct{}{}
	}
	for _, m := range bodyCloseKeywordPattern.FindAllStringSubmatch(body, -1) {
		seen["NEX-"+m[1]] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
