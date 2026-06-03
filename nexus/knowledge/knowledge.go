// Package knowledge implements the cross-session knowledge store per
// registration spec §2.8. Stores textual knowledge entries in the
// Nexus SQLite database with FTS5-backed keyword search. Embedding
// columns are reserved but unused in v1 (sqlite-vec deferred per
// operator #7695) — vector retrieval arrives later behind the same
// SearchKnowledge interface.
package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// Store is the knowledge store handle. Safe for concurrent use — all
// operations go through *sql.DB which handles its own locking.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// New wraps an already-open *sql.DB. Expects the Nexus schema to be
// in place (storage.Bootstrap has run). Pass a logger for diagnostic
// warnings (empty scope, future telemetry); nil logger is allowed
// but surpresses those.
func New(db *sql.DB, log *slog.Logger) *Store {
	return &Store{db: db, log: log}
}

// Entry is a single knowledge record as seen by callers.
type Entry struct {
	ID        int64
	FromAgent string
	Topic     string
	Content   string
	Shared    bool
	UpdatedAt string
	// EmbeddingModel / EmbeddingDim are populated only when vector
	// retrieval is enabled; empty otherwise.
	EmbeddingModel string
	EmbeddingDim   int
}

// PutOptions carries optional flags for Put. Zero-valued means default.
type PutOptions struct {
	// Shared marks the entry as operator-curated (visible under Scope.Shared).
	//
	// Note on upsert: Put uses ON CONFLICT DO UPDATE. If an entry
	// already exists with Shared=true and the caller re-Puts with
	// PutOptions{}, the shared flag will be CLEARED. Callers doing a
	// content-only refresh on a shared entry must pass Shared: true
	// explicitly. Future work: add a distinct `Update()` verb that
	// only touches content and leaves flags alone.
	Shared bool
}

// Put inserts or updates a knowledge entry. Keyed by (from_agent,
// topic) — writing the same pair twice replaces the earlier entry.
// Returns the entry's row id.
//
// Embeddings are NOT populated here. When vector retrieval is enabled,
// a separate write-path hook will embed the content and update the
// `embedding` / `embed_model` / `embed_dim` columns.
func (s *Store) Put(ctx context.Context, fromAgent, topic, content string, opts PutOptions) (int64, error) {
	if strings.TrimSpace(fromAgent) == "" {
		return 0, errors.New("knowledge.Put: empty from_agent")
	}
	if strings.TrimSpace(topic) == "" {
		return 0, errors.New("knowledge.Put: empty topic")
	}
	if content == "" {
		return 0, errors.New("knowledge.Put: empty content")
	}

	shared := 0
	if opts.Shared {
		shared = 1
	}

	// Upsert — ON CONFLICT on (from_agent, topic) unique index.
	// updated_at refreshed on both insert and conflict.
	const query = `
	INSERT INTO knowledge(from_agent, topic, content, shared)
	VALUES(?, ?, ?, ?)
	ON CONFLICT(from_agent, topic) DO UPDATE SET
	  content = excluded.content,
	  shared = excluded.shared,
	  updated_at = datetime('now')
	RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, query, fromAgent, topic, content, shared).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("knowledge.Put: %w", err)
	}
	// Telemetry (Commonplace): one event per write so we can finally answer
	// "is the store being used, by whom, and how much" — grep/aggregate the
	// `knowledge.put` events. Was a long-standing §2.8 TODO.
	if s.log != nil {
		s.log.Info("knowledge.put",
			"from_agent", fromAgent, "topic", topic,
			"shared", shared == 1, "bytes", len(content))
	}
	return id, nil
}

// Delete removes an entry by (from_agent, topic). No-op if the entry
// doesn't exist. Returns the number of rows removed (0 or 1).
func (s *Store) Delete(ctx context.Context, fromAgent, topic string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM knowledge WHERE from_agent = ? AND topic = ?`,
		fromAgent, topic,
	)
	if err != nil {
		return 0, fmt.Errorf("knowledge.Delete: %w", err)
	}
	return res.RowsAffected()
}

// Get fetches a single entry by (from_agent, topic). Returns sql.ErrNoRows
// if the entry doesn't exist.
func (s *Store) Get(ctx context.Context, fromAgent, topic string) (Entry, error) {
	const query = `
	SELECT id, from_agent, topic, content, shared, updated_at,
	       COALESCE(embed_model, ''), COALESCE(embed_dim, 0)
	FROM knowledge
	WHERE from_agent = ? AND topic = ?`

	var e Entry
	var shared int
	err := s.db.QueryRowContext(ctx, query, fromAgent, topic).Scan(
		&e.ID, &e.FromAgent, &e.Topic, &e.Content, &shared, &e.UpdatedAt,
		&e.EmbeddingModel, &e.EmbeddingDim,
	)
	if err != nil {
		return Entry{}, err
	}
	e.Shared = shared == 1
	return e, nil
}

// Scope controls which entries a Search can return. Defaults (zero
// value) are OwnAgent=true (with Agent set), Shared=true, Peers=nil.
// See registration spec §2.8.
type Scope struct {
	// Agent identifies the caller for OwnAgent matching. Required if
	// OwnAgent is true.
	Agent string
	// OwnAgent includes entries where from_agent == Agent.
	OwnAgent bool
	// Shared includes entries with shared=1 (operator-curated).
	Shared bool
	// Peers is an explicit list of additional from_agent values to include.
	Peers []string
}

// Query wraps a search request per registration spec §2.8.
type Query struct {
	Text  string
	Scope Scope
	TopK  int // default 5 if zero

	// Keyword selects keyword (OR-of-terms) matching instead of the
	// default whole-text phrase match. Phrase match is right for a focused
	// query (the search_knowledge tool: "deploy runbook"); keyword match is
	// right when the query is a whole message (auto-recall passes the
	// incoming turn text) — a phrase of an entire sentence matches almost
	// nothing. With Keyword set, the text is tokenized into an OR of quoted
	// terms (stopwords + sub-3-char tokens dropped); no usable terms → no
	// hits. Each term is still quote-escaped, so this stays injection-safe.
	Keyword bool

	// MaxRank is the FTS5 BM25 rank cutoff. Hits with rank >= MaxRank
	// are rejected. BM25 ranks are negative for matches; closer to
	// zero means a weaker match. Example values: -10 keeps strong
	// matches, -1 keeps almost anything that matched at all. Zero
	// means "no threshold" (the sentinel, safe because no match ever
	// has rank == 0 — unmatched rows are simply absent).
	MaxRank float64
}

// Hit is a single search result.
type Hit struct {
	Entry
	// Score is the backend-specific relevance signal. For FTS5 it's
	// BM25 rank (lower = more relevant). Callers that want a "higher
	// = better" signal should invert.
	Score float64
	// Matched records which retrieval backend produced the hit —
	// "fts" in v1; "vector" and "hybrid" arrive later.
	Matched string
}

// DefaultTopK is used when Query.TopK is zero.
const DefaultTopK = 5

// DefaultListLimit is used when List is called with limit <= 0.
const DefaultListLimit = 50

// Search runs keyword retrieval against FTS5 and returns scoped hits.
// Scope must select at least one category (OwnAgent with Agent set,
// Shared, or a non-empty Peers list) or Search returns an empty
// result slice — no hits is the correct answer for an empty scope.
func (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("knowledge.Search: empty query text")
	}
	topK := q.TopK
	if topK <= 0 {
		topK = DefaultTopK
	}

	whereScope, scopeArgs, hasScope := buildScope(q.Scope)
	if !hasScope {
		// Easy to hit via a typo'd scope struct. Log so the
		// misconfiguration isn't silent — returning nil hits is
		// indistinguishable from "no matches" otherwise.
		if s.log != nil {
			s.log.Warn("knowledge.Search: empty scope, returning no hits",
				"text", q.Text, "agent", q.Scope.Agent)
		}
		return nil, nil
	}

	// TODO(telemetry §2.8): counter per-search (agent, hit count, top
	// score) so we can finally answer "is the KB being used." Same
	// TODO on Put for per-write counters.

	// FTS5 `rank` column: lower is better. Threshold is "reject hits
	// with rank >= MinRelevance"; zero means no threshold (all hits
	// within TopK are kept).
	var query strings.Builder
	query.WriteString(`
	SELECT k.id, k.from_agent, k.topic, k.content, k.shared, k.updated_at,
	       COALESCE(k.embed_model, ''), COALESCE(k.embed_dim, 0),
	       fts.rank AS score
	FROM knowledge_fts fts
	JOIN knowledge k ON k.id = fts.rowid
	WHERE knowledge_fts MATCH ?
	  AND (`)
	query.WriteString(whereScope)
	query.WriteString(`)`)

	// Sanitize user text into a single FTS5 phrase. Raw user text gets
	// parsed by FTS5 for operators (AND/OR/NEAR/* /-/^/quoted phrases),
	// which is both an operator-injection vector (a low-trust caller
	// can craft scope-bypass / wildcard-explosion queries) and a
	// syntax-error footgun (stray `"` or `-` returns SQLITE_ERROR
	// rather than zero hits). Quoting the whole input as a phrase
	// neutralizes operators and makes syntax errors impossible.
	ftsExpr := sanitizeFTSQuery(q.Text)
	if q.Keyword {
		or := sanitizeFTSQueryOR(q.Text)
		if or == "" {
			// All stopwords / too-short tokens — nothing usable to match.
			// Empty result is the right answer (and fail-open for auto-recall).
			return nil, nil
		}
		ftsExpr = or
	}
	args := []any{ftsExpr}
	args = append(args, scopeArgs...)

	if q.MaxRank != 0 {
		// Keep hits with rank < MaxRank — BM25 ranks are negative for
		// matches, so "rank < threshold" means "more negative, i.e.
		// stronger match than the cutoff."
		query.WriteString(" AND fts.rank < ?")
		args = append(args, q.MaxRank)
	}
	query.WriteString(" ORDER BY fts.rank LIMIT ?")
	args = append(args, topK)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("knowledge.Search: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var h Hit
		var shared int
		if err := rows.Scan(
			&h.ID, &h.FromAgent, &h.Topic, &h.Content, &shared, &h.UpdatedAt,
			&h.EmbeddingModel, &h.EmbeddingDim,
			&h.Score,
		); err != nil {
			return nil, fmt.Errorf("knowledge.Search: scan: %w", err)
		}
		h.Shared = shared == 1
		h.Matched = "fts"
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("knowledge.Search: rows: %w", err)
	}
	// Telemetry (Commonplace): one event per search — answers "is recall
	// happening, and is it finding anything." topScore is the best (lowest)
	// BM25 rank; 0 when there were no hits. Was a long-standing §2.8 TODO.
	if s.log != nil {
		var topScore float64
		if len(hits) > 0 {
			topScore = hits[0].Score // ORDER BY rank → first is strongest
		}
		s.log.Info("knowledge.search",
			"agent", q.Scope.Agent, "query_len", len(q.Text),
			"own", q.Scope.OwnAgent, "shared", q.Scope.Shared, "peers", len(q.Scope.Peers),
			"hits", len(hits), "top_score", topScore)
	}
	return hits, nil
}

// sanitizeFTSQuery wraps user-supplied text as a single FTS5 phrase
// query, escaping internal double-quotes by doubling them (FTS5's
// quoting rule). Output is always a valid FTS5 query string; callers
// can pass any byte sequence and get a phrase-only match.
//
// Examples:
//
//	foo bar              ->  "foo bar"
//	say "hi"             ->  "say ""hi"""
//	-keyword OR *        ->  "-keyword OR *"   (operators neutralized)
//
// Side effects: caller loses the ability to use FTS operators
// deliberately. That's the trade — knowledge.Search's contract is
// "match this text", not "execute this query". A future structured-
// query API can expose operators behind an allowlisted parser.
func sanitizeFTSQuery(text string) string {
	// Doubling " is FTS5's escape for a literal double-quote inside a
	// phrase. Wrap the whole thing in quotes to force phrase parsing.
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

// ftsStopwords are dropped from keyword (OR) queries — common words that
// add query cost + noise without narrowing. Small + English; BM25 already
// downweights frequent terms, so this just trims the worst offenders so a
// whole-message recall query doesn't OR together "how/the/do/I/...".
var ftsStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "for": true, "is": true, "are": true,
	"was": true, "be": true, "do": true, "does": true, "did": true, "how": true,
	"what": true, "why": true, "when": true, "where": true, "who": true,
	"can": true, "could": true, "should": true, "would": true, "with": true,
	"my": true, "me": true, "i": true, "you": true, "it": true, "this": true,
	"that": true, "we": true, "at": true, "by": true, "as": true, "from": true,
	"about": true, "into": true, "out": true, "up": true, "if": true, "so": true,
}

// maxKeywordTerms bounds the OR query size so a huge pasted message can't
// build an enormous FTS expression.
const maxKeywordTerms = 12

// sanitizeFTSQueryOR tokenizes free text into an OR of quoted single-term
// phrases for keyword retrieval. Used by auto-recall, which passes a whole
// turn message rather than a focused phrase — ORing the salient terms lets
// BM25 surface entries that share vocabulary with the message. Tokens are
// lowercased, split on non-alphanumeric runes, de-duplicated, with
// stopwords + sub-3-char tokens dropped and the count capped. Each term is
// quote-escaped, so the result is always a valid, injection-safe FTS5
// query. Returns "" when nothing usable remains.
func sanitizeFTSQueryOR(text string) string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(fields))
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 || ftsStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		terms = append(terms, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
		if len(terms) >= maxKeywordTerms {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// List returns entries from a single agent (convenience for dashboard
// or per-agent audit). Not part of the retrieval interface — returns
// the full content of each entry without FTS matching.
func (s *Store) List(ctx context.Context, fromAgent string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT id, from_agent, topic, content, shared, updated_at,
	       COALESCE(embed_model, ''), COALESCE(embed_dim, 0)
	FROM knowledge
	WHERE from_agent = ?
	ORDER BY updated_at DESC
	LIMIT ?`,
		fromAgent, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("knowledge.List: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var shared int
		if err := rows.Scan(
			&e.ID, &e.FromAgent, &e.Topic, &e.Content, &shared, &e.UpdatedAt,
			&e.EmbeddingModel, &e.EmbeddingDim,
		); err != nil {
			return nil, fmt.Errorf("knowledge.List: scan: %w", err)
		}
		e.Shared = shared == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAll returns entries across every from_agent, newest first.
// Mirrors List but without the agent filter — used by the operator
// dashboard's default knowledge view so migrated rows (canon, research,
// per-aspect notes) are visible without the operator having to specify
// each agent.
func (s *Store) ListAll(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT id, from_agent, topic, content, shared, updated_at,
	       COALESCE(embed_model, ''), COALESCE(embed_dim, 0)
	FROM knowledge
	ORDER BY updated_at DESC
	LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("knowledge.ListAll: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var shared int
		if err := rows.Scan(
			&e.ID, &e.FromAgent, &e.Topic, &e.Content, &shared, &e.UpdatedAt,
			&e.EmbeddingModel, &e.EmbeddingDim,
		); err != nil {
			return nil, fmt.Errorf("knowledge.ListAll: scan: %w", err)
		}
		e.Shared = shared == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// buildScope constructs the SQL predicate (and arg list) that selects
// which entries a Scope allows. Returns hasScope=false when the scope
// admits nothing — caller returns empty results in that case without
// issuing a query.
func buildScope(s Scope) (predicate string, args []any, hasScope bool) {
	var clauses []string

	if s.OwnAgent && s.Agent != "" {
		clauses = append(clauses, "k.from_agent = ?")
		args = append(args, s.Agent)
	}
	if s.Shared {
		clauses = append(clauses, "k.shared = 1")
	}
	if len(s.Peers) > 0 {
		placeholders := make([]string, len(s.Peers))
		for i, p := range s.Peers {
			placeholders[i] = "?"
			args = append(args, p)
		}
		clauses = append(clauses, "k.from_agent IN ("+strings.Join(placeholders, ",")+")")
	}

	if len(clauses) == 0 {
		return "", nil, false
	}
	return strings.Join(clauses, " OR "), args, true
}
