package broker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeSubmitter records Submit calls for assertion in tests.
type fakeSubmitter struct {
	mu    sync.Mutex
	calls []dispatch.Brief
	errFn func(dispatch.Brief) error
}

func (f *fakeSubmitter) Submit(_ context.Context, b dispatch.Brief) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, b)
	if f.errFn != nil {
		return "", f.errFn(b)
	}
	return "run-1", nil
}

func (f *fakeSubmitter) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeChatStore counts Insert calls so tests can assert that both normal
// messages and !dispatch posts (the audit-thread root) reach the store.
type fakeChatStore struct {
	mu      sync.Mutex
	inserts []string // content of each Insert call
}

func (s *fakeChatStore) Insert(_ context.Context, from, content string, replyTo int64, topic string) (chat.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserts = append(s.inserts, content)
	return chat.Message{
		ID:        int64(len(s.inserts)),
		From:      from,
		Content:   content,
		CreatedAt: time.Now(),
	}, nil
}

func (s *fakeChatStore) insertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inserts)
}

// Stub implementations for the rest of chat.Store — not used by
// HandleChatSend so they can return zero/nil.

func (s *fakeChatStore) ListThread(_ context.Context, threadID, sinceID int64, limit int) ([]chat.Message, error) {
	return nil, nil
}
func (s *fakeChatStore) ToggleReaction(_ context.Context, msgID int64, reactor, emoji string) (bool, error) {
	return false, nil
}
func (s *fakeChatStore) AnnounceSharedFile(_ context.Context, sharedBy, path, description string) (int64, int64, error) {
	return 0, 0, nil
}
func (s *fakeChatStore) ShareFile(_ context.Context, sharedBy, path string, recipients []string) (int64, error) {
	return 0, nil
}
func (s *fakeChatStore) ListSince(_ context.Context, sinceID int64, limit int) ([]chat.Message, error) {
	return nil, nil
}
func (s *fakeChatStore) GetByID(_ context.Context, id int64) (chat.Message, error) {
	return chat.Message{}, nil
}
func (s *fakeChatStore) ThreadParticipants(_ context.Context, msgID int64) ([]string, error) {
	return nil, nil
}
func (s *fakeChatStore) ListShared(_ context.Context, limit int) ([]chat.SharedFile, error) {
	return nil, nil
}
func (s *fakeChatStore) GetShared(_ context.Context, id int64) (chat.SharedFile, error) {
	return chat.SharedFile{}, nil
}
func (s *fakeChatStore) ListReplies(_ context.Context, parentID int64) ([]chat.Message, error) {
	return nil, nil
}
func (s *fakeChatStore) ListPage(_ context.Context, beforeID, afterID int64, limit int) ([]chat.Message, bool, error) {
	return nil, false, nil
}
func (s *fakeChatStore) GetReactions(_ context.Context, msgIDs []int64) (map[int64][]chat.Reaction, error) {
	return nil, nil
}

// TestDispatchInterceptedBeforeChatStore verifies that:
//   - Normal chat messages ("hello world") are persisted via ChatStore.
//   - !dispatch posts are ALSO persisted (the audit-thread root, NEX-494) AND
//     routed to the Submitter; normal messages are not submitted.
func TestDispatchInterceptedBeforeChatStore(t *testing.T) {
	store := &fakeChatStore{}
	sub := &fakeSubmitter{}

	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          store,
		Runner:             sub,
	}, r)
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)

	ctx := context.Background()

	// 1. Normal message — stored, not submitted.
	if _, err := b.HandleChatSend(ctx, "shadow", "hello world", 0, ""); err != nil {
		t.Fatalf("HandleChatSend normal: %v", err)
	}

	// 2. Dispatch post — stored (audit-thread root) AND submitted.
	if _, err := b.HandleChatSend(ctx, "shadow", "!dispatch anvil NEX-999 build it", 0, ""); err != nil {
		t.Fatalf("HandleChatSend dispatch: %v", err)
	}

	// Both posts are stored (the !dispatch post is the thread root).
	if got := store.insertCount(); got != 2 {
		t.Errorf("ChatStore.Insert called %d times, want 2 (both posts stored)", got)
	}
	// Only the !dispatch post is routed to the Submitter.
	if got := sub.submitCount(); got != 1 {
		t.Errorf("Submitter.Submit called %d times, want 1 (only for !dispatch)", got)
	}

	// Verify the brief that was submitted looks right.
	sub.mu.Lock()
	brief := sub.calls[0]
	sub.mu.Unlock()

	if brief.Agent != "anvil" {
		t.Errorf("brief.Agent = %q, want %q", brief.Agent, "anvil")
	}
	if brief.Task != "NEX-999 build it" {
		t.Errorf("brief.Task = %q, want %q", brief.Task, "NEX-999 build it")
	}
	if brief.DispatchMsgID != 2 {
		t.Errorf("brief.DispatchMsgID = %d, want 2", brief.DispatchMsgID)
	}
}

// TestDispatchWithoutRunner — when no Runner is configured, the !dispatch post
// is still stored (the thread root), and the submit error is logged (not
// returned, since HandleChatSend swallows it).
func TestDispatchWithoutRunner(t *testing.T) {
	store := &fakeChatStore{}

	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          store,
		// Runner intentionally nil
	}, r)
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)

	ctx := context.Background()

	// !dispatch with no runner — the post is still stored (thread root); the
	// submit error is logged, not returned.
	if _, err := b.HandleChatSend(ctx, "shadow", "!dispatch anvil NEX-999 build it", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: unexpected error: %v", err)
	}

	if got := store.insertCount(); got != 1 {
		t.Errorf("ChatStore.Insert called %d times, want 1 (the !dispatch post is stored as the thread root)", got)
	}
}

// S4: hands cannot dispatch. A !dispatch post from a derived identity
// is rejected before the intercept does anything: nothing reaches the
// Runner, the !dispatch post itself is not stored, and a rejection
// note is posted into the thread instead.
func TestDispatchRejectsDerivedSender(t *testing.T) {
	store := &fakeChatStore{}
	sub := &fakeSubmitter{}

	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          store,
		Runner:             sub,
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)

	if _, err := b.HandleChatSend(context.Background(), "plumb.sub-1",
		"!dispatch anvil NEX-999 build it", 0, "spawn-42"); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	if got := sub.submitCount(); got != 0 {
		t.Errorf("Submit called %d times, want 0 (hands cannot dispatch)", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var rejection bool
	for _, content := range store.inserts {
		if strings.HasPrefix(strings.TrimSpace(content), "!dispatch") {
			t.Errorf("the hand's !dispatch post must not be stored, got %q", content)
		}
		if strings.Contains(content, "hands cannot dispatch; ask your parent") {
			rejection = true
		}
	}
	if !rejection {
		t.Errorf("rejection note missing from thread; stored = %v", store.inserts)
	}
}

func TestSubmitDispatchRejectsDisabledAgent(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	astore := aspects.NewSQLStore(db)
	if err := astore.Insert(context.Background(), aspects.Aspect{
		Name: "anvil", AspectPubkey: fakePubkeyBytes(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}
	if err := astore.SetDispatchEnabled(context.Background(), "anvil", false); err != nil {
		t.Fatalf("disable dispatch: %v", err)
	}

	b := New(Config{
		AuthToken:         "testtoken",
		AllowLegacyMaster: true,
		Runner:            &fakeSubmitter{},
		KeyfileValidator: &KeyfileValidator{
			Store: astore,
		},
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)

	err = b.submitDispatch(context.Background(), "shadow",
		"!dispatch anvil repo=o/r ticket=NEX-1 do it", "dispatch-1", 1)
	if err == nil {
		t.Fatal("disabled agent should be rejected")
	}
	if !strings.Contains(err.Error(), "agent anvil is dispatch-disabled") {
		t.Fatalf("err = %v, want dispatch-disabled rejection", err)
	}
	if got := b.runner.(*fakeSubmitter).submitCount(); got != 0 {
		t.Fatalf("Submit called %d times, want 0", got)
	}
}
