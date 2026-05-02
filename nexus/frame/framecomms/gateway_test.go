package framecomms

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexus-cw/nexus/nexus/chat"
	"github.com/nexus-cw/nexus/nexus/frame/funnel"
	"github.com/nexus-cw/nexus/nexus/storage"
)

func openTestGateway(t *testing.T) *Gateway {
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
	return NewGateway(chat.NewSQLStore(db), "frame")
}

func TestGateway_SendChatPersists(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	id, err := g.SendChat(ctx, "hello operator", 0, "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero msg id")
	}

	msgs, err := g.ReadThread(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg in thread, got %d", len(msgs))
	}
	if msgs[0].From != "frame" {
		t.Errorf("from: got %q, want frame", msgs[0].From)
	}
	if msgs[0].Content != "hello operator" {
		t.Errorf("content: got %q", msgs[0].Content)
	}
	if msgs[0].ReceivedAt == "" {
		t.Error("ReceivedAt should be RFC 3339 (Lock 6)")
	}
	if !strings.HasSuffix(msgs[0].ReceivedAt, "Z") {
		t.Errorf("ReceivedAt should end with Z (UTC): got %q", msgs[0].ReceivedAt)
	}
}

func TestGateway_SendChatNoStoreErrors(t *testing.T) {
	g := &Gateway{AspectID: "frame"} // store nil
	_, err := g.SendChat(context.Background(), "x", 0, "")
	if err == nil {
		t.Error("nil store should error at call site")
	}
}

func TestGateway_SendChatNoAspectIDErrors(t *testing.T) {
	dir := t.TempDir()
	db, _ := storage.Open(context.Background(), filepath.Join(dir), nil)
	defer db.Close()
	storage.Bootstrap(context.Background(), db)
	g := &Gateway{Store: chat.NewSQLStore(db)} // aspect id empty
	_, err := g.SendChat(context.Background(), "x", 0, "")
	if err == nil {
		t.Error("empty AspectID should error at call site")
	}
}

func TestGateway_ReadThreadReturnsRepliesInOrder(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	rootID, _ := g.SendChat(ctx, "root", 0, "")
	r1, _ := g.SendChat(ctx, "reply 1", rootID, "")
	r2, _ := g.SendChat(ctx, "reply 2", rootID, "")

	msgs, err := g.ReadThread(ctx, rootID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected root + 2 replies, got %d", len(msgs))
	}
	if msgs[1].ID != r1 || msgs[2].ID != r2 {
		t.Errorf("expected root r1 r2 in order: got %d %d %d", msgs[0].ID, msgs[1].ID, msgs[2].ID)
	}
}

func TestGateway_ReadThreadSinceFilter(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	rootID, _ := g.SendChat(ctx, "root", 0, "")
	r1, _ := g.SendChat(ctx, "r1", rootID, "")
	r2, _ := g.SendChat(ctx, "r2", rootID, "")

	msgs, err := g.ReadThread(ctx, rootID, r1)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg after sinceID=r1, got %d", len(msgs))
	}
	if msgs[0].ID != r2 {
		t.Errorf("expected r2, got id %d", msgs[0].ID)
	}
}

// Compile-time check: Gateway must satisfy funnel.ChatGateway. If
// the interface gains a method, this fails the build before any
// caller drifts.
var _ funnelChatGatewayInterface = (*Gateway)(nil)

// We import funnel only for this check; aliasing keeps the test
// file otherwise free of funnel references.
type funnelChatGatewayInterface = funnel.ChatGateway

func TestGateway_ReactToToggles(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	msgID, _ := g.SendChat(ctx, "ping", 0, "")

	// First call: adds. Second call: removes. Both must succeed.
	if err := g.ReactTo(ctx, msgID, "👀"); err != nil {
		t.Errorf("first react_to: %v", err)
	}
	if err := g.ReactTo(ctx, msgID, "👀"); err != nil {
		t.Errorf("second react_to (toggle off): %v", err)
	}
}

func TestGateway_AnnounceFilePostsToChat(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	msgID, err := g.AnnounceFile(ctx, "/tmp/spec.md", "draft v1")
	if err != nil {
		t.Fatal(err)
	}
	if msgID == 0 {
		t.Error("expected non-zero msg_id")
	}
	// The announce should be visible as a chat message.
	msgs, err := g.ReadThread(ctx, msgID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "draft v1" {
		t.Errorf("announce message not retrievable: %+v", msgs)
	}
}

func TestGateway_ShareFileReturnsID(t *testing.T) {
	g := openTestGateway(t)
	ctx := context.Background()
	id, err := g.ShareFile(ctx, "/tmp/private.md", []string{"anvil"})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Error("expected non-zero share id")
	}
}

