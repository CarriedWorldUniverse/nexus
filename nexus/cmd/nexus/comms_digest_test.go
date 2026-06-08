package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

// NEX-245 Slice 2: needs-attention items render with from/time/reason
// and the message preview. Order is preserved oldest-first so context
// flows the way the operator would have read the log live.
func TestFormatDigest_NeedsAttentionOrderedAndDecorated(t *testing.T) {
	out := formatDigest([]digestResult{
		{
			msg: chatMessage{
				FromAgent: "anvil",
				Content:   "@operator should we ship NEX-244 now?",
				CreatedAt: time.Date(2026, 5, 25, 14, 23, 0, 0, time.UTC),
			},
			verdict: classification.CommsDigestVerdict{
				Class:  classification.CommsClassNeedsAttention,
				Reason: "direct question",
			},
		},
		{
			msg: chatMessage{
				FromAgent: "shadow",
				Content:   "blocked on credential refresh",
				CreatedAt: time.Date(2026, 5, 25, 15, 12, 0, 0, time.UTC),
			},
			verdict: classification.CommsDigestVerdict{
				Class:  classification.CommsClassNeedsAttention,
				Reason: "blocked",
			},
		},
	}, 24*time.Hour, "operator", 240)

	for _, want := range []string{
		"Needs attention (2)",
		"[anvil @ 2026-05-25 14:23] direct question",
		"@operator should we ship NEX-244 now?",
		"[shadow @ 2026-05-25 15:12] blocked",
		"classified: 2",
		"operator: operator",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q\n---\n%s", want, out)
		}
	}
	// anvil should appear before shadow (oldest-first).
	if strings.Index(out, "anvil") > strings.Index(out, "shadow") {
		t.Errorf("expected anvil entry before shadow; got:\n%s", out)
	}
}

// NEX-245 Slice 2: background section groups by sender with count
// summary, sorted by count desc + name asc for stability. Operator
// shouldn't have to scroll background noise to read attention items.
func TestFormatDigest_BackgroundGroupedByFrom(t *testing.T) {
	rs := []digestResult{}
	for range 4 {
		rs = append(rs, digestResult{
			msg:     chatMessage{FromAgent: "harrow", Content: "ack"},
			verdict: classification.CommsDigestVerdict{Class: classification.CommsClassBackground, Reason: "peer ack"},
		})
	}
	for range 2 {
		rs = append(rs, digestResult{
			msg:     chatMessage{FromAgent: "anvil", Content: "ack"},
			verdict: classification.CommsDigestVerdict{Class: classification.CommsClassBackground, Reason: "peer ack"},
		})
	}
	rs = append(rs, digestResult{
		msg:     chatMessage{FromAgent: "shadow", Content: "ack"},
		verdict: classification.CommsDigestVerdict{Class: classification.CommsClassBackground, Reason: "peer ack"},
	})

	out := formatDigest(rs, 24*time.Hour, "operator", 240)

	if !strings.Contains(out, "Background (7 messages)") {
		t.Errorf("expected Background header with count=7; got:\n%s", out)
	}
	if !strings.Contains(out, "- 4× harrow") {
		t.Errorf("missing harrow grouping; got:\n%s", out)
	}
	if !strings.Contains(out, "- 2× anvil") {
		t.Errorf("missing anvil grouping; got:\n%s", out)
	}
	if !strings.Contains(out, "- 1× shadow") {
		t.Errorf("missing shadow grouping; got:\n%s", out)
	}
	// 4-count harrow should appear before 2-count anvil.
	if strings.Index(out, "harrow") > strings.Index(out, "anvil") {
		t.Errorf("expected harrow (4x) before anvil (2x); got:\n%s", out)
	}
}

// NEX-245 Slice 2: both empty sections render "(none)" — an all-
// quiet digest should still be intelligible, not a blank screen.
func TestFormatDigest_EmptySectionsRenderNone(t *testing.T) {
	out := formatDigest(nil, 24*time.Hour, "operator", 240)
	if strings.Count(out, "(none)") != 2 {
		t.Errorf("expected (none) under both sections; got:\n%s", out)
	}
	if !strings.Contains(out, "classified: 0") {
		t.Errorf("expected classified: 0 footer; got:\n%s", out)
	}
}

// NEX-245 Slice 2: previews are capped + newlines flattened so each
// entry stays one terminal-screen-line-ish.
func TestTruncatePreview(t *testing.T) {
	short := truncatePreview("hello\nworld", 100)
	if short != "hello ↵ world" {
		t.Errorf("newline flatten: got %q want %q", short, "hello ↵ world")
	}
	long := truncatePreview(strings.Repeat("a", 500), 80)
	if len(long) != 80+len("…") {
		t.Errorf("len(long) = %d, want %d", len(long), 80+len("…"))
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("expected ellipsis suffix; got %q", long)
	}
}

// NEX-245 Slice 2: parseChatTimestamp accepts SQLite default format +
// RFC3339; returns zero on garbage so renderer can blank-out the
// time column rather than crashing.
func TestParseChatTimestamp(t *testing.T) {
	cases := map[string]bool{
		"2026-05-25 14:23:00":      true,
		"2026-05-25T14:23:00Z":     true,
		"2026-05-25T14:23:00.123Z": true,
		"not a timestamp":          false,
	}
	for in, ok := range cases {
		parsed := parseChatTimestamp(in)
		if ok && parsed.IsZero() {
			t.Errorf("expected parsed time for %q", in)
		}
		if !ok && !parsed.IsZero() {
			t.Errorf("expected zero time for %q", in)
		}
	}
}

// NEX-245 Slice 2: loadRecentChatMessages filters by cutoff, skips
// the operator's own messages, ignores non-chat kinds, and orders
// oldest-first. Guards the SQL clauses against accidental changes.
func TestLoadRecentChatMessages_FiltersAndOrders(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE chat_messages (
			id                 INTEGER PRIMARY KEY,
			thread_id          TEXT,
			from_agent         TEXT NOT NULL,
			content            TEXT NOT NULL,
			reply_to           INTEGER,
			parent_msg_id      INTEGER,
			thread_root_msg_id INTEGER,
			kind               TEXT NOT NULL DEFAULT 'chat',
			created_at         TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	rows := []struct {
		from, content, kind, ts string
	}{
		{"anvil", "old — outside window", "chat", "2026-05-20 10:00:00"},
		{"anvil", "in-window message A", "chat", "2026-05-25 10:00:00"},
		{"operator", "operator's own — excluded", "chat", "2026-05-25 10:05:00"},
		{"shadow", "in-window message B", "chat", "2026-05-25 11:00:00"},
		{"harrow", "system row — excluded", "system", "2026-05-25 11:30:00"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO chat_messages (from_agent, content, kind, created_at) VALUES (?,?,?,?)`,
			r.from, r.content, r.kind, r.ts,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	cutoff := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	msgs, err := loadRecentChatMessages(ctx, db, cutoff, "operator", 100)
	if err != nil {
		t.Fatalf("loadRecentChatMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (filtered cutoff/operator/kind)", len(msgs))
	}
	if msgs[0].FromAgent != "anvil" || msgs[1].FromAgent != "shadow" {
		t.Errorf("ordering wrong: got %q,%q want anvil,shadow",
			msgs[0].FromAgent, msgs[1].FromAgent)
	}
}

// NEX-245 Slice 2: loadThreadHint returns up to N prior messages in
// the same thread, oldest-first so context reads naturally. Trims
// internal newlines + caps each line.
func TestLoadThreadHint_OldestFirstAndBounded(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE chat_messages (
			id                 INTEGER PRIMARY KEY,
			from_agent         TEXT NOT NULL,
			content            TEXT NOT NULL,
			thread_root_msg_id INTEGER
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Root + two intermediate messages + a fourth (the "current"
	// message we'll ask hint for, id=4 within the same thread).
	for _, r := range []struct {
		id   int64
		from string
		text string
	}{
		{1, "operator", "should we ship?"},
		{2, "harrow", "i think yes"},
		{3, "anvil", "agree, ready"},
		{4, "shadow", "the fourth reply"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO chat_messages (id, from_agent, content, thread_root_msg_id) VALUES (?,?,?,1)`,
			r.id, r.from, r.text,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	hint, err := loadThreadHint(ctx, db, 4, 1, 2)
	if err != nil {
		t.Fatalf("loadThreadHint: %v", err)
	}
	// Expect 2 prior (ids 2,3) oldest-first.
	wantPrefix := "harrow: i think yes\nanvil: agree, ready"
	if hint != wantPrefix {
		t.Errorf("hint = %q\nwant   %q", hint, wantPrefix)
	}
}

// NEX-245 Slice 2: truncateForHint flattens newlines + caps length
// so a 50KB log dump in one thread message doesn't blow the prompt.
func TestTruncateForHint(t *testing.T) {
	got := truncateForHint("line1\nline2  \n  line3")
	if got != "line1 line2     line3" {
		t.Errorf("truncateForHint multiline: got %q", got)
	}
	long := truncateForHint(strings.Repeat("x", 500))
	if len(long) != 200+len("…") {
		t.Errorf("len = %d, want %d", len(long), 200+len("…"))
	}
}
