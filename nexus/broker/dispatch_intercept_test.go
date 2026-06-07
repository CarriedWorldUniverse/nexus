package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeSubmitter records Submit calls for assertion in tests.
type fakeSubmitter struct {
	mu     sync.Mutex
	calls  []dispatch.Brief
	errFn  func(dispatch.Brief) error
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

// fakeChatStore counts Insert calls so tests can assert normal messages
// still reach the store while !dispatch messages do not.
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
//   - !dispatch messages are routed to the Submitter and NOT persisted.
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

	// 1. Normal message — should reach ChatStore, not Submitter.
	if _, err := b.HandleChatSend(ctx, "shadow", "hello world", 0, ""); err != nil {
		t.Fatalf("HandleChatSend normal: %v", err)
	}

	// 2. Dispatch message — should reach Submitter, NOT ChatStore.
	if _, err := b.HandleChatSend(ctx, "shadow", "!dispatch anvil NEX-999 build it", 0, ""); err != nil {
		t.Fatalf("HandleChatSend dispatch: %v", err)
	}

	if got := store.insertCount(); got != 1 {
		t.Errorf("ChatStore.Insert called %d times, want 1 (only for 'hello world')", got)
	}
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
}

// TestDispatchWithoutRunner — when no Runner is configured, !dispatch
// messages should still not reach ChatStore, and the error is logged
// (not returned as a test-visible error since HandleChatSend swallows it).
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

	// !dispatch with no runner — returns (0, nil), no ChatStore insert.
	if _, err := b.HandleChatSend(ctx, "shadow", "!dispatch anvil NEX-999 build it", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: unexpected error: %v", err)
	}

	if got := store.insertCount(); got != 0 {
		t.Errorf("ChatStore.Insert called %d times, want 0 for !dispatch with no runner", got)
	}
}
