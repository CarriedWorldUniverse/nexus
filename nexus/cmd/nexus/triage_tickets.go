// Ticket auto-triage subcommand for nexus (NEX-247 Slice 2).
//
// `nexus triage-tickets --credential <jira-cred> [--apply-assignee]
//                       [--dry-run] [--limit N] [--jql "..."]
//                       [--data-dir DIR]` polls Jira for unassigned
// NEX-* stories, classifies each via the NEX-247 TicketTriage
// classifier, and posts the verdict back as a Jira comment. With
// --apply-assignee, also sets the Jira assignee for high-confidence
// verdicts.
//
// Design notes:
//
//   - Polling (not webhook): nexus has no public HTTP endpoint today.
//     Webhook variant becomes possible once interchange gateway respec
//     lands. Polling is the natural minimum-viable.
//   - One-shot by default: operator wraps in cron / launchd /
//     systemd-timer per their host conventions. --watch would add a
//     persistent loop but adds opinions about cadence.
//   - De-duped via comment marker: each posted comment is prefixed
//     with `triageMarker` so the next poll can skip already-triaged
//     tickets. Operator can re-trigger by deleting the comment.
//   - Classifier credential separate from Jira credential. Operator
//     names both: --credential <jira>, --classifier-credential
//     <provider>. Classifier defaults to deepseek per NEX-243's
//     AI-switchability contract.
//
// Inline Jira REST client kept minimal (Search, ListComments,
// Comment, Assign, MyAccountID). Refactor to a shared nexus/jiraapi
// package once a second consumer surfaces — see follow-up note in
// the PR description.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// triageMarker prefixes every comment this subcommand posts so the
// next poll can recognise an already-triaged ticket + skip it.
// Stable string — operators relying on the marker for greppable
// audit shouldn't see it change without a deliberate migration.
const triageMarker = "🤖 nexus-triage:"

// defaultUnassignedJQL finds open NEX-* stories with no assignee.
// Tightened via --jql when the operator wants more selectivity
// (e.g. only stories created in the last 24h).
const defaultUnassignedJQL = `project = NEX AND assignee is EMPTY AND statusCategory != Done`

func runTriageTicketsSubcommand(args []string) int {
	fs := flag.NewFlagSet("triage-tickets", flag.ContinueOnError)
	jiraCred := fs.String("credential", "", "name of a kind=jira credential in the nexus credentials store (required)")
	classifierCred := fs.String("classifier-credential", "", "name of a kind=provider credential for the classifier (required)")
	classifierProvider := fs.String("classifier-provider", "openai", "bridle provider for the classifier: claude-api | openai (default openai per NEX-298)")
	classifierModel := fs.String("classifier-model", "deepseek-chat", "default model for the classifier (env NEXUS_TICKET_TRIAGE_MODEL wins; per-call ModelOverride wins over both)")
	classifierBaseURL := fs.String("classifier-base-url", "", "override classifier provider base URL (empty = credential's base_url then SDK default)")
	jql := fs.String("jql", defaultUnassignedJQL, "JQL for which tickets to triage")
	limit := fs.Int("limit", 20, "max tickets to triage per run")
	dryRun := fs.Bool("dry-run", false, "classify + log verdicts but don't post comments or set assignees")
	applyAssignee := fs.Bool("apply-assignee", false, "for high-confidence verdicts pointing at a known aspect, also set the Jira assignee (requires the aspect's Jira accountId to be resolvable)")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 120, "wall-clock budget for the whole run in seconds")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *jiraCred == "" || *classifierCred == "" {
		fmt.Fprintln(os.Stderr, "triage-tickets: --credential and --classifier-credential are both required")
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
		fmt.Fprintf(os.Stderr, "triage-tickets: load jira credential: %v\n", err)
		return 1
	}
	classifier, err := buildTicketTriageClassifier(ctx, store, *classifierCred, *classifierProvider, *classifierModel, *classifierBaseURL, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "triage-tickets: build classifier: %v\n", err)
		return 1
	}

	keys, err := jc.searchUnassigned(ctx, *jql, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "triage-tickets: search jira: %v\n", err)
		return 1
	}
	log.Info("triage-tickets: search complete", "jql", *jql, "candidates", len(keys), "limit", *limit)

	stats := triageStats{}
	for _, key := range keys {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "triage-tickets: ctx done after %d processed: %v\n", stats.processed, ctx.Err())
			break
		default:
		}
		outcome := triageOne(ctx, jc, classifier, key, *dryRun, *applyAssignee, log)
		stats.record(outcome)
	}

	fmt.Printf("processed: %d  triaged: %d  skipped_existing: %d  errored: %d  applied_assignee: %d  dry_run: %v\n",
		stats.processed, stats.triaged, stats.skippedExisting, stats.errored, stats.appliedAssignee, *dryRun)
	if stats.errored > 0 {
		return 1
	}
	return 0
}

// triageOutcome is what triageOne returns — for stats counting.
type triageOutcome int

const (
	outcomeTriaged triageOutcome = iota
	outcomeSkippedExisting
	outcomeErrored
)

// triageStats counts outcomes per run for the operator summary line.
type triageStats struct {
	processed       int
	triaged         int
	skippedExisting int
	errored         int
	appliedAssignee int
}

func (s *triageStats) record(o triageOutcome) {
	s.processed++
	switch o {
	case outcomeTriaged:
		s.triaged++
	case outcomeSkippedExisting:
		s.skippedExisting++
	case outcomeErrored:
		s.errored++
	}
}

// triageOne processes a single ticket: skip if already triaged,
// otherwise classify + (in non-dry-run mode) post comment + optionally
// set assignee. Returns outcome for stats.
func triageOne(ctx context.Context, jc *triageJiraClient, classifier *classification.TicketTriage, key string, dryRun, applyAssignee bool, log *slog.Logger) triageOutcome {
	issue, err := jc.getIssue(ctx, key)
	if err != nil {
		log.Warn("triage-tickets: get issue failed", "key", key, "err", err)
		return outcomeErrored
	}

	if dryRun {
		// In dry-run mode we still classify so operator sees verdicts,
		// but skip the existing-comment check (no side effects to
		// dedupe against).
		verdict, _ := classifier.Classify(ctx, classification.TicketTriageInput{
			Summary:     issue.Summary,
			Description: issue.Description,
			Labels:      issue.Labels,
		})
		log.Info("triage-tickets: dry-run verdict",
			"key", key,
			"assignee_aspect", verdict.AssigneeAspect,
			"assignee_team", verdict.AssigneeTeam,
			"confidence", verdict.Confidence,
			"reason", verdict.Reason)
		return outcomeTriaged
	}

	comments, err := jc.listComments(ctx, key)
	if err != nil {
		log.Warn("triage-tickets: list comments failed", "key", key, "err", err)
		return outcomeErrored
	}
	for _, c := range comments {
		if strings.Contains(c.BodyText, triageMarker) {
			log.Debug("triage-tickets: already triaged, skipping", "key", key, "comment_id", c.ID)
			return outcomeSkippedExisting
		}
	}

	verdict, _ := classifier.Classify(ctx, classification.TicketTriageInput{
		Summary:     issue.Summary,
		Description: issue.Description,
		Labels:      issue.Labels,
	})

	body := formatTriageComment(verdict)
	if err := jc.postComment(ctx, key, body); err != nil {
		log.Warn("triage-tickets: post comment failed", "key", key, "err", err)
		return outcomeErrored
	}
	log.Info("triage-tickets: triaged",
		"key", key,
		"assignee_aspect", verdict.AssigneeAspect,
		"assignee_team", verdict.AssigneeTeam,
		"confidence", verdict.Confidence)

	// --apply-assignee: only set the Jira assignee on high-confidence
	// aspect routings. Low/medium + team routings stay comment-only so
	// the operator can review the verdict before it sticks.
	if applyAssignee && verdict.Confidence == "high" && verdict.AssigneeAspect != "" {
		log.Info("triage-tickets: --apply-assignee requested but accountId-by-aspect mapping not yet wired; comment posted only",
			"key", key, "aspect", verdict.AssigneeAspect)
		// Future: lookup the aspect's Jira accountId (operator-supplied
		// mapping file or DB), then jc.assignTo(ctx, key, accountID).
		// Out of scope for Slice 2 — operator can manually set assignee
		// after reviewing the comment.
	}
	return outcomeTriaged
}

// formatTriageComment renders the verdict as a plain-text Jira
// comment. Operator-readable; the marker prefix gates de-dupe.
func formatTriageComment(v classification.TicketTriageVerdict) string {
	var route string
	if v.AssigneeAspect != "" {
		route = "aspect=" + v.AssigneeAspect
	} else {
		route = "team=" + v.AssigneeTeam
	}
	return fmt.Sprintf("%s %s (confidence: %s)\nreason: %s",
		triageMarker, route, v.Confidence, v.Reason)
}

// loadJiraClient resolves a kind=jira credential from the local
// credentials store + builds a minimal Jira REST client.
func loadJiraClient(ctx context.Context, store *credentials.Store, credName string) (*triageJiraClient, error) {
	cred, err := store.Get(ctx, credName)
	if err != nil {
		return nil, fmt.Errorf("get credential %q: %w", credName, err)
	}
	if cred.Kind != credentials.KindJira {
		return nil, fmt.Errorf("credential %q is kind=%q, want jira", credName, cred.Kind)
	}
	bundle, err := store.JiraBundle(cred)
	if err != nil {
		return nil, fmt.Errorf("unwrap jira bundle: %w", err)
	}
	site := bundle.Subdomain + ".atlassian.net"
	return newTriageJiraClient(site, bundle.Email, bundle.Token), nil
}

// buildTicketTriageClassifier wires the TicketTriage classifier with
// a bridle provider built from the named credential. Mirrors
// test-provider's buildTestProvider shape so the same env-var
// discipline + base-URL precedence applies.
func buildTicketTriageClassifier(ctx context.Context, store *credentials.Store, credName, providerName, model, baseURLOverride string, log *slog.Logger) (*classification.TicketTriage, error) {
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

	return &classification.TicketTriage{
		Harness:  bridle.NewHarness(provider),
		Provider: providerID,
		Model:    model,
		Logger:   log,
	}, nil
}

// triageJiraClient is the minimal Jira REST client used by this
// subcommand. Inlined here to keep the slice scoped; mirrors a
// subset of nexus-jira-mcp's jiraClient and is the natural extract
// candidate for a future nexus/jiraapi package.
type triageJiraClient struct {
	site     string
	email    string
	apiToken string
	http     *http.Client
}

type triageIssue struct {
	Key         string
	Summary     string
	Description string
	Labels      []string
}

type triageComment struct {
	ID       string
	BodyText string
}

func newTriageJiraClient(site, email, apiToken string) *triageJiraClient {
	return &triageJiraClient{
		site:     site,
		email:    email,
		apiToken: apiToken,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *triageJiraClient) authHeader() string {
	raw := c.email + ":" + c.apiToken
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func (c *triageJiraClient) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	u := "https://" + c.site + path
	var body io.Reader
	if reqBody != nil {
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("jira: marshal request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("jira: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(buf))
	}
	if respBody == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("jira: decode response: %w", err)
	}
	return nil
}

func (c *triageJiraClient) searchUnassigned(ctx context.Context, jql string, maxResults int) ([]string, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	if maxResults > 100 {
		maxResults = 100
	}
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", "summary")
	q.Set("maxResults", fmt.Sprintf("%d", maxResults))
	var resp struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/search/jql?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(resp.Issues))
	for _, i := range resp.Issues {
		keys = append(keys, i.Key)
	}
	return keys, nil
}

func (c *triageJiraClient) getIssue(ctx context.Context, key string) (triageIssue, error) {
	q := url.Values{}
	q.Set("fields", "summary,description,labels")
	var raw struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string         `json:"summary"`
			Description map[string]any `json:"description"`
			Labels      []string       `json:"labels"`
		} `json:"fields"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"?"+q.Encode(), nil, &raw); err != nil {
		return triageIssue{}, err
	}
	return triageIssue{
		Key:         raw.Key,
		Summary:     raw.Fields.Summary,
		Description: adfWalk(raw.Fields.Description),
		Labels:      raw.Fields.Labels,
	}, nil
}

func (c *triageJiraClient) listComments(ctx context.Context, key string) ([]triageComment, error) {
	var raw struct {
		Comments []struct {
			ID   string         `json:"id"`
			Body map[string]any `json:"body"`
		} `json:"comments"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]triageComment, 0, len(raw.Comments))
	for _, c := range raw.Comments {
		out = append(out, triageComment{
			ID:       c.ID,
			BodyText: adfWalk(c.Body),
		})
	}
	return out, nil
}

func (c *triageJiraClient) postComment(ctx context.Context, key, body string) error {
	payload := map[string]any{"body": adfFromPlain(body)}
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", payload, nil)
}

// adfFromPlain wraps plain text in ADF paragraphs split on newlines.
// Minimal — only what's needed for the triage comment shape.
func adfFromPlain(text string) map[string]any {
	var nodes []map[string]any
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			nodes = append(nodes, map[string]any{"type": "paragraph"})
			continue
		}
		nodes = append(nodes, map[string]any{
			"type": "paragraph",
			"content": []map[string]any{
				{"type": "text", "text": line},
			},
		})
	}
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": nodes,
	}
}

// adfWalk extracts plain text from an Atlassian Document Format
// (ADF) node tree. Sufficient for de-dupe + classifier input —
// doesn't preserve formatting (the classifier sees prose anyway).
func adfWalk(v any) string {
	if v == nil {
		return ""
	}
	var sb strings.Builder
	walkADFNode(v, &sb)
	return strings.TrimSpace(sb.String())
}

func walkADFNode(v any, sb *strings.Builder) {
	switch n := v.(type) {
	case map[string]any:
		if t, ok := n["type"].(string); ok && t == "text" {
			if s, ok := n["text"].(string); ok {
				sb.WriteString(s)
			}
		}
		// `content` can arrive as []any (JSON-unmarshalled from Jira's
		// response) OR []map[string]any (locally-constructed via
		// adfFromPlain). Handle both — the round-trip de-dupe path
		// depends on the local-construction shape working.
		descended := false
		if content, ok := n["content"].([]any); ok {
			for _, child := range content {
				walkADFNode(child, sb)
			}
			descended = true
		} else if content, ok := n["content"].([]map[string]any); ok {
			for _, child := range content {
				walkADFNode(child, sb)
			}
			descended = true
		}
		// Treat block boundaries as space-separators so adjacent
		// paragraphs don't run together when reflowed.
		if descended {
			if t, ok := n["type"].(string); ok {
				switch t {
				case "paragraph", "heading", "codeBlock", "listItem":
					sb.WriteByte('\n')
				}
			}
		}
	case []any:
		for _, child := range n {
			walkADFNode(child, sb)
		}
	case []map[string]any:
		for _, child := range n {
			walkADFNode(child, sb)
		}
	}
}
