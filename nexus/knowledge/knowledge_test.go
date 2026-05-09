package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, nil)
}

func TestPutAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.Put(ctx, "keel", "restart-sequence", "stop aspects first, broker last", PutOptions{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id == 0 {
		t.Error("Put returned id=0")
	}

	e, err := s.Get(ctx, "keel", "restart-sequence")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Content != "stop aspects first, broker last" {
		t.Errorf("content mismatch: %q", e.Content)
	}
	if e.Shared {
		t.Error("Shared should default false")
	}
}

func TestPutUpsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, _ := s.Put(ctx, "keel", "topic-a", "original gingerbread content", PutOptions{})
	id2, err := s.Put(ctx, "keel", "topic-a", "replaced marzipan content", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("upsert should return same id, got %d vs %d", id1, id2)
	}

	e, _ := s.Get(ctx, "keel", "topic-a")
	if e.Content != "replaced marzipan content" {
		t.Errorf("content after upsert = %q, want replaced", e.Content)
	}

	// FTS index must be updated — a search for the ORIGINAL content
	// should miss, and a search for the NEW content should hit.
	// Confirms the AFTER UPDATE trigger fires on ON CONFLICT DO UPDATE.
	origHits, _ := s.Search(ctx, Query{
		Text:  "gingerbread",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if len(origHits) != 0 {
		t.Errorf("FTS still matches old content 'gingerbread' after upsert: %d hits", len(origHits))
	}
	newHits, _ := s.Search(ctx, Query{
		Text:  "marzipan",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if len(newHits) != 1 {
		t.Errorf("FTS did not pick up new content 'marzipan' after upsert: %d hits", len(newHits))
	}
}

// TestPutUpsertShared documents the footgun: re-Putting with default
// PutOptions clears a previously-shared flag.
func TestPutUpsertSharedFlagBehaviour(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "operator", "flag-test", "v1", PutOptions{Shared: true})
	e, _ := s.Get(ctx, "operator", "flag-test")
	if !e.Shared {
		t.Fatal("Shared should be true after initial Put with Shared: true")
	}

	// Caller-of-the-future does a content-only refresh, forgets to pass Shared:true.
	s.Put(ctx, "operator", "flag-test", "v2", PutOptions{})
	e, _ = s.Get(ctx, "operator", "flag-test")
	if e.Shared {
		t.Error("Shared flag should have been cleared by Put without Shared:true (documented behaviour)")
	}
}

func TestPutValidation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := map[string][3]string{
		"empty from_agent": {"", "topic", "content"},
		"empty topic":      {"agent", "", "content"},
		"empty content":    {"agent", "topic", ""},
		"whitespace agent": {"   ", "topic", "content"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Put(ctx, c[0], c[1], c[2], PutOptions{})
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "keel", "to-delete", "hi", PutOptions{})

	n, err := s.Delete(ctx, "keel", "to-delete")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete removed %d rows, want 1", n)
	}

	_, err = s.Get(ctx, "keel", "to-delete")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after Delete err = %v, want sql.ErrNoRows", err)
	}

	n, err = s.Delete(ctx, "keel", "never-existed")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Delete on missing entry rowsAffected = %d, want 0", n)
	}
}

func TestSearchScopeOwnAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "keel", "broker-restart", "stop aspects first", PutOptions{})
	s.Put(ctx, "harrow", "research-notes", "pi architecture overview", PutOptions{})

	hits, err := s.Search(ctx, Query{
		Text:  "aspects",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(hits))
	}
	if hits[0].FromAgent != "keel" {
		t.Errorf("hit from %q, want keel", hits[0].FromAgent)
	}
	if hits[0].Matched != "fts" {
		t.Errorf("Matched = %q, want fts", hits[0].Matched)
	}
}

func TestSearchScopeIsolation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "keel", "broker", "broker things", PutOptions{})
	s.Put(ctx, "harrow", "broker-article", "broker things we read about", PutOptions{})

	// Scoped to keel only — harrow's entry must not appear.
	hits, _ := s.Search(ctx, Query{
		Text:  "broker",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	for _, h := range hits {
		if h.FromAgent != "keel" {
			t.Errorf("leaked cross-agent entry: %q from %q", h.Topic, h.FromAgent)
		}
	}
}

func TestSearchScopeShared(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "operator", "canon-shared", "network protocol", PutOptions{Shared: true})
	s.Put(ctx, "operator", "private-note", "network protocol", PutOptions{Shared: false})

	hits, _ := s.Search(ctx, Query{
		Text:  "protocol",
		Scope: Scope{Agent: "keel", Shared: true},
	})
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1 (shared only)", len(hits))
	}
	if hits[0].Topic != "canon-shared" {
		t.Errorf("got topic %q, want canon-shared", hits[0].Topic)
	}
	if !hits[0].Shared {
		t.Error("hit Shared flag should be true")
	}
}

func TestSearchScopePeers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.Put(ctx, "keel", "own-note", "auth flow stuff", PutOptions{})
	s.Put(ctx, "harrow", "peer-note", "auth flow stuff", PutOptions{})
	s.Put(ctx, "wren", "excluded-note", "auth flow stuff", PutOptions{})

	hits, _ := s.Search(ctx, Query{
		Text: "auth",
		Scope: Scope{
			Agent:    "keel",
			OwnAgent: true,
			Peers:    []string{"harrow"},
		},
	})

	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.FromAgent] = true
	}
	if !seen["keel"] {
		t.Error("missing own entry")
	}
	if !seen["harrow"] {
		t.Error("missing peer entry")
	}
	if seen["wren"] {
		t.Error("wren entry leaked through — not in Peers list")
	}
}

func TestSearchEmptyScopeReturnsNoHits(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "t", "content", PutOptions{})

	hits, err := s.Search(ctx, Query{
		Text:  "content",
		Scope: Scope{}, // no OwnAgent / Shared / Peers set
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("empty scope should return no hits, got %d", len(hits))
	}
}

func TestSearchEmptyText(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Search(context.Background(), Query{
		Text:  "",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err == nil {
		t.Error("expected error for empty search text")
	}
}

func TestSearchTopKCap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s.Put(ctx, "keel", "t-" + string(rune('a'+i)), "matching content here", PutOptions{})
	}

	hits, _ := s.Search(ctx, Query{
		Text:  "matching",
		Scope: Scope{Agent: "keel", OwnAgent: true},
		TopK:  3,
	})
	if len(hits) != 3 {
		t.Errorf("len(hits) = %d, want 3", len(hits))
	}
}

func TestSearchDefaultTopK(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s.Put(ctx, "keel", "t-" + string(rune('a'+i)), "matching content here", PutOptions{})
	}

	hits, _ := s.Search(ctx, Query{
		Text:  "matching",
		Scope: Scope{Agent: "keel", OwnAgent: true},
		// TopK unset → DefaultTopK (5)
	})
	if len(hits) != DefaultTopK {
		t.Errorf("len(hits) = %d, want %d (DefaultTopK)", len(hits), DefaultTopK)
	}
}

func TestSearchMaxRankThreshold(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A closely-matching entry and a barely-matching entry.
	s.Put(ctx, "keel", "tight", "unique_marker quick fox quick fox quick fox", PutOptions{})
	s.Put(ctx, "keel", "loose", "noise text unique_marker more noise and padding", PutOptions{})

	// Very strict cutoff — both may pass or only the tighter one,
	// depending on BM25 weighting. Test the direction: rank < MaxRank
	// should reject the weaker match when we tighten the bar.
	allHits, _ := s.Search(ctx, Query{
		Text:  "unique_marker",
		Scope: Scope{Agent: "keel", OwnAgent: true},
		TopK:  10,
	})
	if len(allHits) < 1 {
		t.Fatal("expected at least one hit without threshold")
	}

	// MaxRank semantics: rank < MaxRank. A sentinel below any actual
	// rank (very negative, e.g. -1000) should reject everything;
	// above any rank (e.g. 1000) should keep everything.
	none, _ := s.Search(ctx, Query{
		Text:    "unique_marker",
		Scope:   Scope{Agent: "keel", OwnAgent: true},
		TopK:    10,
		MaxRank: -1000.0,
	})
	if len(none) != 0 {
		t.Errorf("MaxRank = -1000 should reject everything, got %d", len(none))
	}

	permissive, _ := s.Search(ctx, Query{
		Text:    "unique_marker",
		Scope:   Scope{Agent: "keel", OwnAgent: true},
		TopK:    10,
		MaxRank: 1000.0,
	})
	if len(permissive) != len(allHits) {
		t.Errorf("MaxRank = 1000 should match no-threshold result count %d, got %d",
			len(allHits), len(permissive))
	}
}

// Regression for issue #37: user query text must not be parsed as
// FTS5 operators. A single-word search still hits via phrase match.
func TestSearchPhraseQuoteHappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "t1", "the quick brown fox", PutOptions{})

	hits, err := s.Search(ctx, Query{
		Text:  "quick",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("len(hits) = %d, want 1", len(hits))
	}
}

// Multi-token phrase semantics: post-fix, "quick brown" matches a row
// containing those tokens adjacent and in order, but "brown quick"
// does not. This pins the AND-to-phrase behavior change documented in
// sanitizeFTSQuery's comment.
func TestSearchMultiTokenPhraseAdjacencyAndOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "t1", "the quick brown fox", PutOptions{})

	hits, err := s.Search(ctx, Query{
		Text:  "quick brown",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err != nil {
		t.Fatalf("Search adjacent: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("adjacent in-order phrase: len(hits) = %d, want 1", len(hits))
	}

	hits, err = s.Search(ctx, Query{
		Text:  "brown quick",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err != nil {
		t.Fatalf("Search reversed: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("reversed-order phrase: len(hits) = %d, want 0 (phrase, not AND)", len(hits))
	}
}

// FTS5 operators in user text must be neutralized (treated as
// literal phrase tokens). A query of "OR" against content that
// doesn't actually contain "OR" must miss, not return all rows.
func TestSearchOperatorsNeutralized(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "t1", "alpha content one", PutOptions{})
	s.Put(ctx, "keel", "t2", "beta content two", PutOptions{})

	// Pre-fix: "alpha OR beta" parses as FTS operator and matches both.
	// Post-fix: it's a phrase query for the literal string "alpha OR
	// beta", which appears in neither row.
	hits, err := s.Search(ctx, Query{
		Text:  "alpha OR beta",
		Scope: Scope{Agent: "keel", OwnAgent: true},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("operator query returned %d hits, want 0 (operators must be neutralized)", len(hits))
	}
}

// Stray FTS5-syntax characters used to surface as SQLITE_ERROR.
// Post-fix: any byte sequence is a valid phrase query.
func TestSearchSyntaxFootgunsDoNotCrash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "t1", "harmless content", PutOptions{})

	footguns := []string{
		`unbalanced "`,
		`stray - dash`,
		`wildcard *`,
		`column:filter`,
		`NEAR(a b)`,
		`^prefix`,
		`""`,
	}
	for _, q := range footguns {
		t.Run(q, func(t *testing.T) {
			_, err := s.Search(ctx, Query{
				Text:  q,
				Scope: Scope{Agent: "keel", OwnAgent: true},
			})
			if err != nil {
				t.Errorf("Search(%q) errored: %v — sanitization should make any input a valid phrase", q, err)
			}
		})
	}
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := map[string]string{
		"foo bar":      `"foo bar"`,
		`say "hi"`:     `"say ""hi"""`,
		`-keyword OR *`: `"-keyword OR *"`,
		``:             `""`,
		`pre"mid"post`: `"pre""mid""post"`,
	}
	for in, want := range cases {
		if got := sanitizeFTSQuery(in); got != want {
			t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.Put(ctx, "keel", "a", "first", PutOptions{})
	s.Put(ctx, "keel", "b", "second", PutOptions{})
	s.Put(ctx, "harrow", "c", "third", PutOptions{})

	entries, err := s.List(ctx, "keel", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("len(entries) = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.FromAgent != "keel" {
			t.Errorf("leaked non-keel entry: %v", e)
		}
	}
}
