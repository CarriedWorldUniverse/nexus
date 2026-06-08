// PR triage subcommand for nexus (NEX-244 caller, lane 1 of NEX-243).
//
// `nexus triage-prs --classifier-credential <provider-cred>
//                   --repo owner/name [--repo ...]
//                   [--since 24h] [--limit 20] [--dry-run]
//                   [--data-dir DIR] [--timeout-s 300]`
//
// Polls open PRs for each --repo, classifies each via PRTriage
// (NEX-244 foundation), posts the verdict as a marker-prefixed
// comment on the PR. De-dupe via marker: re-runs skip PRs that
// already have our comment.
//
// Same polling-vs-webhook constraint as triage-tickets +
// close-merged-tickets: no public HTTP endpoint until interchange
// gateway respec lands.
//
// Uses gh CLI subprocess for BOTH read (gh pr list, gh pr diff,
// gh pr view --comments) and write (gh pr comment). Operator already
// has gh authenticated — no new credential surface to manage. The
// only nexus-managed credential is the classifier provider key.

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
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// prTriageMarker prefixes every comment this subcommand posts so the
// next poll can recognise an already-triaged PR + skip it. Same
// pattern as triage-tickets / close-merged-tickets.
const prTriageMarker = "🤖 nexus-pr-triage:"

func runTriagePRsSubcommand(args []string) int {
	fs := flag.NewFlagSet("triage-prs", flag.ContinueOnError)
	classifierCred := fs.String("classifier-credential", "", "name of a kind=provider credential for the classifier (required)")
	classifierProvider := fs.String("classifier-provider", "openai", "bridle provider: claude-api | openai (default openai per NEX-298)")
	classifierModel := fs.String("classifier-model", "deepseek-chat", "default model for the classifier (env NEXUS_PR_TRIAGE_MODEL wins; per-call ModelOverride wins over both)")
	classifierBaseURL := fs.String("classifier-base-url", "", "override classifier provider base URL (empty = credential's base_url then SDK default)")
	var repos repeatableStringFlag
	fs.Var(&repos, "repo", "GitHub repo as owner/name; repeat for multiple repos (required)")
	since := fs.Duration("since", 24*time.Hour, "only triage PRs updated in the last N (e.g. 24h, 72h)")
	limit := fs.Int("limit", 20, "max open PRs to triage per repo per run")
	dryRun := fs.Bool("dry-run", false, "classify + log verdicts but don't post comments")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 300, "wall-clock budget for the whole run in seconds")
	ghPath := fs.String("gh-path", "gh", "path to the gh CLI binary (default: gh on PATH)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *classifierCred == "" || len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "triage-prs: --classifier-credential and at least one --repo are required")
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

	classifier, err := buildPRTriageClassifier(ctx, store, *classifierCred, *classifierProvider, *classifierModel, *classifierBaseURL, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "triage-prs: build classifier: %v\n", err)
		return 1
	}

	stats := prTriageStats{}
	for _, repo := range repos {
		prs, err := listOpenPRs(ctx, *ghPath, repo, *since, *limit)
		if err != nil {
			log.Warn("triage-prs: list open PRs failed", "repo", repo, "err", err)
			stats.errored++
			continue
		}
		log.Info("triage-prs: PRs to inspect",
			"repo", repo, "count", len(prs), "since", since.String())
		for _, pr := range prs {
			processOpenPR(ctx, *ghPath, classifier, pr, *dryRun, &stats, log)
		}
	}

	fmt.Printf("repos_scanned: %d  prs_inspected: %d  triaged: %d  skipped_existing: %d  errored: %d  dry_run: %v\n",
		len(repos), stats.prsInspected, stats.triaged, stats.skippedExisting, stats.errored, *dryRun)
	if stats.errored > 0 {
		return 1
	}
	return 0
}

// openPR is the projection from `gh pr list` plus the repo
// (gh doesn't include it in --json output when --repo is set).
type openPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Repo   string `json:"-"` // populated by listOpenPRs
}

type prTriageStats struct {
	prsInspected    int
	triaged         int
	skippedExisting int
	errored         int
}

// buildPRTriageClassifier mirrors buildTicketTriageClassifier
// (triage_tickets.go). Same credential-store + provider-dispatch +
// base-URL precedence.
func buildPRTriageClassifier(ctx context.Context, store *credentials.Store, credName, providerName, model, baseURLOverride string, log *slog.Logger) (*classification.PRTriage, error) {
	cred, err := store.Get(ctx, credName)
	if err != nil {
		return nil, fmt.Errorf("get classifier credential %q: %w", credName, err)
	}
	if cred.Kind != credentials.KindProvider {
		return nil, fmt.Errorf("classifier credential %q is kind=%q, want provider", credName, cred.Kind)
	}
	bundle, err := store.ProviderBundle(cred)
	if err != nil {
		return nil, fmt.Errorf("unwrap provider bundle: %w", err)
	}
	endpoint := baseURLOverride
	if endpoint == "" {
		endpoint = bundle.BaseURL
	}
	var provider bridle.Provider
	var providerID bridle.ProviderID
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "claude-api", "claude", "anthropic":
		provider = claudeprovider.NewWithBaseURL(bundle.Key, endpoint)
		providerID = bridle.ProviderClaude
	case "openai":
		provider = openaiprovider.NewWithBaseURL(bundle.Key, endpoint)
		providerID = bridle.ProviderOpenAI
	default:
		return nil, fmt.Errorf("unsupported --classifier-provider %q (claude-api | openai)", providerName)
	}
	return &classification.PRTriage{
		Harness:  bridle.NewHarness(provider),
		Provider: providerID,
		Model:    model,
		Logger:   log,
	}, nil
}

// listOpenPRs shells out to `gh pr list --state open` for one repo.
// --search uses GitHub's date qualifier on PR updated time so a long-
// dormant PR with no recent activity doesn't get re-triaged forever.
func listOpenPRs(ctx context.Context, ghPath, repo string, since time.Duration, limit int) ([]openPR, error) {
	cutoff := time.Now().Add(-since).UTC().Format("2006-01-02")
	args := []string{
		"pr", "list",
		"--repo", repo,
		"--state", "open",
		"--search", "updated:>" + cutoff,
		"--json", "number,title,url",
		"--limit", fmt.Sprintf("%d", limit),
	}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("gh pr list failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []openPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}
	for i := range prs {
		prs[i].Repo = repo
	}
	return prs, nil
}

// processOpenPR triages a single open PR — skip if already commented,
// otherwise classify + post verdict comment.
func processOpenPR(ctx context.Context, ghPath string, classifier *classification.PRTriage, pr openPR, dryRun bool, stats *prTriageStats, log *slog.Logger) {
	stats.prsInspected++

	// De-dupe: skip if our marker comment already present. In dry-run
	// mode we still classify so operator sees verdicts, but the
	// already-commented PRs get noted.
	hasMarker, err := prHasMarkerComment(ctx, ghPath, pr.Repo, pr.Number, prTriageMarker)
	if err != nil {
		log.Warn("triage-prs: check existing comments failed",
			"repo", pr.Repo, "pr", pr.Number, "err", err)
		stats.errored++
		return
	}
	if hasMarker && !dryRun {
		stats.skippedExisting++
		return
	}

	diff, err := fetchPRDiff(ctx, ghPath, pr.Repo, pr.Number)
	if err != nil {
		log.Warn("triage-prs: fetch diff failed",
			"repo", pr.Repo, "pr", pr.Number, "err", err)
		stats.errored++
		return
	}
	if strings.TrimSpace(diff) == "" {
		// Empty diff = nothing to triage (e.g. PR with only branch
		// rename or merge commit). Skip silently.
		return
	}

	verdict, _ := classifier.Classify(ctx, classification.PRTriageInput{Diff: diff})

	if dryRun {
		log.Info("triage-prs: dry-run verdict",
			"repo", pr.Repo, "pr", pr.Number, "title", pr.Title,
			"class", verdict.Class, "reason", verdict.Reason,
			"already_commented", hasMarker)
		stats.triaged++
		return
	}

	body := formatPRTriageComment(verdict)
	if err := postPRComment(ctx, ghPath, pr.Repo, pr.Number, body); err != nil {
		log.Warn("triage-prs: post comment failed",
			"repo", pr.Repo, "pr", pr.Number, "err", err)
		stats.errored++
		return
	}
	log.Info("triage-prs: triaged",
		"repo", pr.Repo, "pr", pr.Number, "title", pr.Title,
		"class", verdict.Class, "reason", verdict.Reason)
	stats.triaged++
}

// formatPRTriageComment renders the verdict as a plain-text PR
// comment. Marker prefix gates de-dupe.
func formatPRTriageComment(v classification.Verdict) string {
	return fmt.Sprintf("%s class=%s\nreason: %s", prTriageMarker, v.Class, v.Reason)
}

// fetchPRDiff returns the unified diff for one PR via gh CLI. gh
// handles GitHub auth via the operator's existing gh login.
func fetchPRDiff(ctx context.Context, ghPath, repo string, number int) (string, error) {
	args := []string{"pr", "diff", fmt.Sprintf("%d", number), "--repo", repo}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("gh pr diff: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("gh pr diff: %w", err)
	}
	return string(out), nil
}

// prHasMarkerComment returns true if the PR has any comment whose
// body contains marker. Same de-dupe shape as triage-tickets.
func prHasMarkerComment(ctx context.Context, ghPath, repo string, number int, marker string) (bool, error) {
	args := []string{"pr", "view", fmt.Sprintf("%d", number), "--repo", repo, "--json", "comments"}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, fmt.Errorf("gh pr view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return false, fmt.Errorf("gh pr view: %w", err)
	}
	var resp struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, fmt.Errorf("decode gh pr view output: %w", err)
	}
	for _, c := range resp.Comments {
		if strings.Contains(c.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// postPRComment shells out to `gh pr comment <num> --body ...`.
// Mirrors the read-side use of gh — same credential, same path.
func postPRComment(ctx context.Context, ghPath, repo string, number int, body string) error {
	args := []string{"pr", "comment", fmt.Sprintf("%d", number), "--repo", repo, "--body", body}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
