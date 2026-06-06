package broker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func openReplayTestStore(t *testing.T) *chat.SQLStore {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), filepath.Join(dir), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := storage.Bootstrap(context.Background(), db); err != nil {
		db.Close()
		t.Fatalf("storage.Bootstrap: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return chat.NewSQLStore(db)
}

func TestReplayer_DeliversAddressedMessages(t *testing.T) {
	s := openReplayTestStore(t)
	ctx := context.Background()

	// History: operator chats anvil, then forge, then anvil again.
	// Replayer for "anvil" should see msgs 1 and 3, not 2.
	m1, _ := s.Insert(ctx, "operator", "@anvil first", 0, "")
	_, _ = s.Insert(ctx, "operator", "@forge unrelated", 0, "")
	m3, _ := s.Insert(ctx, "operator", "@anvil third", 0, "")

	r := NewReplayer(s, RecipientPolicy{
		Aspects: func() []string { return []string{"anvil", "forge"} },
	})
	got, err := r.AddressedSince(ctx, "anvil", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 anvil messages, got %d", len(got))
	}
	if got[0].ID != m1.ID || got[1].ID != m3.ID {
		t.Errorf("wrong ids: got %d, %d; want %d, %d", got[0].ID, got[1].ID, m1.ID, m3.ID)
	}
}

func TestReplayer_RespectsSinceCursor(t *testing.T) {
	s := openReplayTestStore(t)
	ctx := context.Background()

	m1, _ := s.Insert(ctx, "operator", "@anvil first", 0, "")
	m2, _ := s.Insert(ctx, "operator", "@anvil second", 0, "")
	_ = m1

	r := NewReplayer(s, RecipientPolicy{
		Aspects: func() []string { return []string{"anvil"} },
	})
	// since=m1.ID — only m2 should come back.
	got, err := r.AddressedSince(ctx, "anvil", m1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].ID != m2.ID {
		t.Errorf("got id %d, want %d", got[0].ID, m2.ID)
	}
}

func TestReplayer_BroadcastReachesEveryAspect(t *testing.T) {
	s := openReplayTestStore(t)
	ctx := context.Background()

	_, _ = s.Insert(ctx, "operator", "@all morning standup", 0, "")

	r := NewReplayer(s, RecipientPolicy{
		Aspects: func() []string { return []string{"anvil", "forge", "wren"} },
	})

	// Each aspect should see the broadcast on replay.
	for _, who := range []string{"anvil", "forge", "wren"} {
		got, err := r.AddressedSince(ctx, who, 0)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: expected 1 broadcast, got %d", who, len(got))
		}
	}
}

func TestReplayer_NeverDeliversOwnMessages(t *testing.T) {
	s := openReplayTestStore(t)
	ctx := context.Background()

	// anvil posts to itself with self-mention. Replayer for anvil
	// should not see this — defensively excluded even past the
	// recipient policy check.
	_, _ = s.Insert(ctx, "anvil", "@anvil note to self", 0, "")
	_, _ = s.Insert(ctx, "anvil", "thinking out loud", 0, "")

	r := NewReplayer(s, RecipientPolicy{
		Aspects: func() []string { return []string{"anvil"} },
	})
	got, err := r.AddressedSince(ctx, "anvil", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 own messages, got %d", len(got))
	}
}

func TestReplayer_PaginatesAcrossManyMessages(t *testing.T) {
	s := openReplayTestStore(t)
	ctx := context.Background()

	// Generate 50 messages addressed to anvil interleaved with
	// 50 unrelated. Use a small page size to force pagination.
	for i := 0; i < 50; i++ {
		s.Insert(ctx, "operator", "@anvil message", 0, "")
		s.Insert(ctx, "operator", "@forge unrelated", 0, "")
	}

	r := &Replayer{
		Store: s,
		Policy: RecipientPolicy{
			Aspects: func() []string { return []string{"anvil", "forge"} },
		},
		PageSize: 7, // forces multiple pages
	}
	got, err := r.AddressedSince(ctx, "anvil", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Errorf("expected 50 anvil messages across pages, got %d", len(got))
	}
	// Verify ordering — replay must be id-ascending.
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Errorf("non-monotonic ids at index %d: %d <= %d", i, got[i].ID, got[i-1].ID)
			break
		}
	}
}

func TestReplayer_RejectsEmptyAspect(t *testing.T) {
	s := openReplayTestStore(t)
	r := NewReplayer(s, RecipientPolicy{})
	_, err := r.AddressedSince(context.Background(), "", 0)
	if err == nil {
		t.Error("empty aspect should error")
	}
}

func TestReplayer_NilStoreErrors(t *testing.T) {
	r := &Replayer{Policy: RecipientPolicy{}}
	_, err := r.AddressedSince(context.Background(), "anvil", 0)
	if err == nil {
		t.Error("nil store should error")
	}
}
