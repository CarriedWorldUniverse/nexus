// Comms-digest subcommand for nexus (NEX-245 caller, lane 2 of NEX-243).
//
// `nexus comms-digest --classifier-credential <provider-cred>
//                     [--operator-name operator]
//                     [--since 24h] [--limit 200]
//                     [--thread-hint-lines 2]
//                     [--data-dir DIR] [--timeout-s 300]`
//
// Reads recent chat messages from the broker's nexus.db, classifies
// each via CommsDigest (NEX-245 foundation) as needs-attention or
// background, prints a structured digest to stdout. Read-only —
// classification verdicts are never written back to the DB.
//
// Operator-driven: re-run for the same window is safe (no dedupe
// state needed — output is for human consumption).
//
// Operator's own messages are skipped (operator doesn't need to be
// reminded of what they themselves said before leaving).
//
// Polling-only (no webhook) per NEX-243's wait-for-interchange
// constraint, same as the three triage pollers in this directory.

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/identity"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func runCommsDigestSubcommand(args []string) int {
	fs := flag.NewFlagSet("comms-digest", flag.ContinueOnError)
	classifierCred := fs.String("classifier-credential", "", "name of a kind=provider credential for the classifier (required)")
	classifierProvider := fs.String("classifier-provider", "openai", "bridle provider: claude-api | openai (default openai per NEX-298)")
	classifierModel := fs.String("classifier-model", "deepseek-chat", "default model (env NEXUS_COMMS_DIGEST_MODEL wins; per-call override wins over both)")
	classifierBaseURL := fs.String("classifier-base-url", "", "override classifier provider base URL (empty = credential's base_url then SDK default)")
	operatorName := fs.String("operator-name", "operator", "whose attention the digest is optimised for; messages from this sender are skipped")
	since := fs.Duration("since", 24*time.Hour, "only digest messages created in the last N (e.g. 24h, 72h)")
	limit := fs.Int("limit", 200, "max messages to classify in one run (caps classifier cost)")
	threadHintLines := fs.Int("thread-hint-lines", 2, "include up to N prior messages from the same thread as context (0 disables)")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 300, "wall-clock budget for the whole run in seconds")
	previewChars := fs.Int("preview-chars", 240, "max characters of message content to show under each needs-attention entry")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *classifierCred == "" {
		fmt.Fprintln(os.Stderr, "comms-digest: --classifier-credential is required")
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutS)*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, db, cleanup, code := openStoreAndDB(ctx, *dataDir, log)
	if code != 0 {
		return code
	}
	defer cleanup()

	classifier, err := buildCommsDigestClassifier(ctx, store, *classifierCred, *classifierProvider, *classifierModel, *classifierBaseURL, *operatorName, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-digest: build classifier: %v\n", err)
		return 1
	}

	cutoff := time.Now().Add(-*since).UTC()
	msgs, err := loadRecentChatMessages(ctx, db, cutoff, *operatorName, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-digest: load chat messages: %v\n", err)
		return 1
	}

	log.Info("comms-digest: loaded messages",
		"count", len(msgs), "since", since.String(), "operator", *operatorName)

	results := make([]digestResult, 0, len(msgs))
	for _, m := range msgs {
		hint := ""
		if *threadHintLines > 0 && m.ThreadRootMsgID.Valid {
			hint, _ = loadThreadHint(ctx, db, m.ID, m.ThreadRootMsgID.Int64, *threadHintLines)
		}
		verdict, _ := classifier.Classify(ctx, classification.CommsDigestInput{
			From:       m.FromAgent,
			Text:       m.Content,
			ThreadHint: hint,
		})
		results = append(results, digestResult{msg: m, verdict: verdict})
	}

	fmt.Print(formatDigest(results, *since, *operatorName, *previewChars))
	return 0
}

// chatMessage is the projection from chat_messages this subcommand needs.
type chatMessage struct {
	ID              int64
	FromAgent       string
	Content         string
	CreatedAt       time.Time
	ThreadRootMsgID sql.NullInt64
}

type digestResult struct {
	msg     chatMessage
	verdict classification.CommsDigestVerdict
}

// openStoreAndDB mirrors openCredentialsStore but ALSO returns the
// underlying *sql.DB so the caller can query chat_messages on the
// same handle. The cleanup closure closes the DB.
func openStoreAndDB(ctx context.Context, dataDir string, log *slog.Logger) (*credentials.Store, *sql.DB, func(), int) {
	db, err := storage.Open(ctx, dataDir, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-digest: open db: %v\n", err)
		return nil, nil, nil, 1
	}
	id, err := identity.Load(ctx, db)
	if err != nil {
		db.Close()
		if errors.Is(err, identity.ErrNotInitialized) {
			fmt.Fprintln(os.Stderr, "comms-digest: nexus identity not initialised; run: nexus identity init")
			return nil, nil, nil, 1
		}
		fmt.Fprintf(os.Stderr, "comms-digest: load identity: %v\n", err)
		return nil, nil, nil, 1
	}
	store, err := credentials.NewStore(db, id.SessionSigningSecret)
	if err != nil {
		db.Close()
		fmt.Fprintf(os.Stderr, "comms-digest: init store: %v\n", err)
		return nil, nil, nil, 1
	}
	cleanup := func() { db.Close() }
	return store, db, cleanup, 0
}

// buildCommsDigestClassifier mirrors buildPRTriageClassifier
// (triage_prs.go). Same credential-store + provider-dispatch +
// base-URL precedence.
func buildCommsDigestClassifier(ctx context.Context, store *credentials.Store, credName, providerName, model, baseURLOverride, operatorName string, log *slog.Logger) (*classification.CommsDigest, error) {
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
	return &classification.CommsDigest{
		Harness:      bridle.NewHarness(provider),
		Provider:     providerID,
		Model:        model,
		OperatorName: operatorName,
		Logger:       log,
	}, nil
}

// loadRecentChatMessages reads chat_messages created after cutoff,
// excluding the operator's own sends, oldest-first, capped at limit.
//
// Oldest-first ordering matches how the operator would read a chat
// log on return; the digest preserves that order so context flows.
//
// kind='chat' filter excludes 'system' / 'hand' rows that aren't part
// of the conversational stream the operator would care about.
func loadRecentChatMessages(ctx context.Context, db *sql.DB, cutoff time.Time, operatorName string, limit int) ([]chatMessage, error) {
	const q = `
		SELECT id, from_agent, content, created_at, thread_root_msg_id
		  FROM chat_messages
		 WHERE created_at > ?
		   AND from_agent  != ?
		   AND kind         = 'chat'
		 ORDER BY created_at ASC, id ASC
		 LIMIT ?`
	rows, err := db.QueryContext(ctx, q,
		cutoff.Format("2006-01-02 15:04:05"), operatorName, limit)
	if err != nil {
		return nil, fmt.Errorf("query chat_messages: %w", err)
	}
	defer rows.Close()

	var out []chatMessage
	for rows.Next() {
		var m chatMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.Content, &createdAt, &m.ThreadRootMsgID); err != nil {
			return nil, fmt.Errorf("scan chat_messages row: %w", err)
		}
		m.CreatedAt = parseChatTimestamp(createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// loadThreadHint returns up to n earlier messages from the same
// thread, joined as "from: content" lines. Earlier == smaller id in
// the same thread_root_msg_id group. Used to give the classifier
// brief context for replies that only make sense in-thread.
func loadThreadHint(ctx context.Context, db *sql.DB, msgID, threadRoot int64, n int) (string, error) {
	const q = `
		SELECT from_agent, content
		  FROM chat_messages
		 WHERE thread_root_msg_id = ?
		   AND id                 < ?
		 ORDER BY id DESC
		 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, threadRoot, msgID, n)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type hint struct{ from, content string }
	var hs []hint
	for rows.Next() {
		var h hint
		if err := rows.Scan(&h.from, &h.content); err != nil {
			return "", err
		}
		hs = append(hs, h)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	// Reverse so output reads oldest -> newest, matching how the
	// classifier (and a human) reads conversational context.
	var b strings.Builder
	for i := len(hs) - 1; i >= 0; i-- {
		if i != len(hs)-1 {
			b.WriteByte('\n')
		}
		b.WriteString(hs[i].from)
		b.WriteString(": ")
		b.WriteString(truncateForHint(hs[i].content))
	}
	return b.String(), nil
}

// truncateForHint keeps thread-hint lines short — they're context,
// not the message under classification. 200 chars matches roughly
// one screen line in a terminal.
func truncateForHint(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	const maxHintLen = 200
	if len(s) > maxHintLen {
		return s[:maxHintLen] + "…"
	}
	return s
}

// parseChatTimestamp accepts either "2006-01-02 15:04:05" (sqlite's
// CURRENT_TIMESTAMP default) or RFC3339, returning the zero time on
// parse failure (caller renders zero as empty).
func parseChatTimestamp(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// formatDigest renders the human-facing digest. Needs-attention items
// come first (oldest -> newest so context flows); background gets a
// grouped count summary so the operator doesn't scroll through
// noise.
func formatDigest(results []digestResult, since time.Duration, operatorName string, previewChars int) string {
	var attention []digestResult
	bgCount := 0
	bgByFrom := map[string]int{}
	for _, r := range results {
		switch r.verdict.Class {
		case classification.CommsClassNeedsAttention:
			attention = append(attention, r)
		case classification.CommsClassBackground:
			bgCount++
			bgByFrom[r.msg.FromAgent]++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "=== Needs attention (%d) ===\n", len(attention))
	if len(attention) == 0 {
		b.WriteString("(none)\n")
	}
	for _, r := range attention {
		fmt.Fprintf(&b, "\n[%s @ %s] %s\n", r.msg.FromAgent, formatChatTime(r.msg.CreatedAt), r.verdict.Reason)
		b.WriteString("  ")
		b.WriteString(truncatePreview(r.msg.Content, previewChars))
		b.WriteByte('\n')
	}

	fmt.Fprintf(&b, "\n=== Background (%d messages) ===\n", bgCount)
	if bgCount == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, line := range groupedBackgroundLines(bgByFrom) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	fmt.Fprintf(&b, "\nsince: last %s  classified: %d  operator: %s\n",
		since.String(), len(results), operatorName)
	return b.String()
}

// groupedBackgroundLines returns "- N× from-agent" lines sorted by
// count descending, then by name for stability.
func groupedBackgroundLines(byFrom map[string]int) []string {
	type kv struct {
		name  string
		count int
	}
	pairs := make([]kv, 0, len(byFrom))
	for k, v := range byFrom {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].name < pairs[j].name
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = fmt.Sprintf("- %d× %s", p.count, p.name)
	}
	return out
}

// truncatePreview collapses internal newlines + caps length. Length
// cap keeps each entry to roughly one terminal screen line per
// --preview-chars; full message stays in the DB if operator needs it.
func truncatePreview(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ↵ ")
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// formatChatTime renders a chat timestamp compactly; empty string on
// zero time so the digest doesn't show "[from @ 0001-01-01...]".
func formatChatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}
