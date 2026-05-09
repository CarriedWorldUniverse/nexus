package chat

import (
	"context"
	"testing"
)

// Tests for the Crossing 5c additions to chat.Store: ListReplies,
// ListPage, GetReactions. The existing chat_test.go covers
// Insert/ListThread/ToggleReaction/etc.

func TestListReplies_DirectChildrenOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	parent, _ := s.Insert(ctx, "keel", "parent", 0, "")
	child1, _ := s.Insert(ctx, "anvil", "reply 1", parent.ID, "")
	child2, _ := s.Insert(ctx, "verity", "reply 2", parent.ID, "")
	// Grandchild — must NOT appear in ListReplies(parent).
	_, _ = s.Insert(ctx, "anvil", "grandchild", child1.ID, "")
	// Sibling thread root — must NOT appear.
	_, _ = s.Insert(ctx, "harrow", "unrelated root", 0, "")

	rep, err := s.ListReplies(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) != 2 {
		t.Fatalf("expected 2 direct replies, got %d: %+v", len(rep), rep)
	}
	if rep[0].ID != child1.ID || rep[1].ID != child2.ID {
		t.Errorf("expected oldest-first ordering: got ids %d, %d", rep[0].ID, rep[1].ID)
	}
}

func TestListReplies_RejectsZeroParent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.ListReplies(context.Background(), 0); err == nil {
		t.Error("expected error for parent_id=0")
	}
}

func TestListReplies_NoChildrenReturnsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	root, _ := s.Insert(ctx, "keel", "lonely", 0, "")
	rep, err := s.ListReplies(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) != 0 {
		t.Errorf("expected 0 replies, got %d", len(rep))
	}
}

func TestListPage_NewestPageOrderedOldestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = s.Insert(ctx, "keel", string(rune('a'+i)), 0, "")
	}
	msgs, hasMore, err := s.ListPage(ctx, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5, got %d", len(msgs))
	}
	if hasMore {
		t.Error("hasMore must be false when result < limit")
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].ID <= msgs[i-1].ID {
			t.Errorf("expected oldest-first by id: got %d after %d", msgs[i].ID, msgs[i-1].ID)
		}
	}
}

func TestListPage_HasMoreTrueWhenLimitHit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		_, _ = s.Insert(ctx, "keel", "x", 0, "")
	}
	msgs, hasMore, err := s.ListPage(ctx, 0, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("expected 3 rows respecting limit, got %d", len(msgs))
	}
	if !hasMore {
		t.Error("hasMore must be true when more rows exist past limit")
	}
}

func TestListPage_AfterID_OnlyNewer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.Insert(ctx, "keel", "1", 0, "")
	_, _ = s.Insert(ctx, "keel", "2", 0, "")
	_, _ = s.Insert(ctx, "keel", "3", 0, "")

	msgs, _, err := s.ListPage(ctx, 0, a.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 rows after id %d, got %d", a.ID, len(msgs))
	}
	for _, m := range msgs {
		if m.ID <= a.ID {
			t.Errorf("got id %d which is not > after_id %d", m.ID, a.ID)
		}
	}
}

func TestListPage_BeforeID_OnlyOlder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, _ = s.Insert(ctx, "keel", "1", 0, "")
	_, _ = s.Insert(ctx, "keel", "2", 0, "")
	c, _ := s.Insert(ctx, "keel", "3", 0, "")

	msgs, _, err := s.ListPage(ctx, c.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 rows before id %d, got %d", c.ID, len(msgs))
	}
	for _, m := range msgs {
		if m.ID >= c.ID {
			t.Errorf("got id %d which is not < before_id %d", m.ID, c.ID)
		}
	}
	// Still oldest-first.
	if len(msgs) >= 2 && msgs[0].ID >= msgs[1].ID {
		t.Errorf("expected oldest-first ordering on before-id page")
	}
}

func TestListPage_RejectsBothBoundsSet(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.ListPage(context.Background(), 5, 3, 10); err == nil {
		t.Error("expected error when both before_id and after_id are set")
	}
}

func TestListPage_EmptyStore(t *testing.T) {
	s := openTestStore(t)
	msgs, hasMore, err := s.ListPage(context.Background(), 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 || hasMore {
		t.Errorf("empty store: got %d msgs, hasMore=%v", len(msgs), hasMore)
	}
}

func TestGetReactions_GroupedByMsgID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.Insert(ctx, "keel", "msg-a", 0, "")
	b, _ := s.Insert(ctx, "keel", "msg-b", 0, "")
	// Two reactions on a, one on b.
	_, _ = s.ToggleReaction(ctx, a.ID, "anvil", "👀")
	_, _ = s.ToggleReaction(ctx, a.ID, "verity", "👍")
	_, _ = s.ToggleReaction(ctx, b.ID, "harrow", "✅")

	rows, err := s.GetReactions(ctx, []int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[a.ID]) != 2 {
		t.Errorf("expected 2 reactions on a, got %d", len(rows[a.ID]))
	}
	if len(rows[b.ID]) != 1 {
		t.Errorf("expected 1 reaction on b, got %d", len(rows[b.ID]))
	}
}

func TestGetReactions_EmptyInput(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.GetReactions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("nil input must return empty map, got %d", len(rows))
	}
}

func TestGetReactions_MissingMsgIDOmitted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.Insert(ctx, "keel", "msg", 0, "")
	_, _ = s.ToggleReaction(ctx, a.ID, "anvil", "👀")

	rows, err := s.GetReactions(ctx, []int64{a.ID, 9999})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows[9999]; ok {
		t.Error("missing msg_id must not appear in result map")
	}
	if len(rows[a.ID]) != 1 {
		t.Errorf("a should still resolve: got %d reactions", len(rows[a.ID]))
	}
}
