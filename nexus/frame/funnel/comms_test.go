package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// fakeGateway is the test double for ChatGateway. Records every call
// for assertion; configurable error returns per method.
type fakeGateway struct {
	mu sync.Mutex

	// Recorded calls
	sentMessages []sentMessage
	reactions    []reactionCall
	threadReads  []readCall
	announces    []announceCall
	shares       []shareCall

	// Configurable errors / return values
	sendErr     error
	sendNextID  int64
	reactErr    error
	readResults []ChatMessage
	readErr     error
	announceErr error
	shareErr    error
}

type sentMessage struct {
	Content string
	ReplyTo int64
	Topic   string
}

type reactionCall struct {
	MsgID int64
	Emoji string
}

type readCall struct {
	ThreadID int64
	SinceID  int64
}

type announceCall struct {
	Path        string
	Description string
}

type shareCall struct {
	Path       string
	Recipients []string
}

func (g *fakeGateway) SendChat(_ context.Context, content string, replyTo int64, topic string) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sentMessages = append(g.sentMessages, sentMessage{Content: content, ReplyTo: replyTo, Topic: topic})
	if g.sendErr != nil {
		return 0, g.sendErr
	}
	if g.sendNextID == 0 {
		g.sendNextID = 1000
	}
	g.sendNextID++
	return g.sendNextID, nil
}

func (g *fakeGateway) ReactTo(_ context.Context, msgID int64, emoji string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reactions = append(g.reactions, reactionCall{MsgID: msgID, Emoji: emoji})
	return g.reactErr
}

func (g *fakeGateway) ReadThread(_ context.Context, threadID, sinceID int64) ([]ChatMessage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.threadReads = append(g.threadReads, readCall{ThreadID: threadID, SinceID: sinceID})
	if g.readErr != nil {
		return nil, g.readErr
	}
	return g.readResults, nil
}

func (g *fakeGateway) AnnounceFile(_ context.Context, path, description string) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.announces = append(g.announces, announceCall{Path: path, Description: description})
	if g.announceErr != nil {
		return 0, g.announceErr
	}
	return 9001, nil
}

func (g *fakeGateway) ShareFile(_ context.Context, path string, recipients []string) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shares = append(g.shares, shareCall{Path: path, Recipients: recipients})
	if g.shareErr != nil {
		return 0, g.shareErr
	}
	return 7001, nil
}

func (g *fakeGateway) snapshotSent() []sentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]sentMessage, len(g.sentMessages))
	copy(out, g.sentMessages)
	return out
}

func TestCommsToolDefs_HasAllFiveTools(t *testing.T) {
	defs := CommsToolDefs()
	want := map[string]bool{
		ToolNameSendChat:     false,
		ToolNameReactTo:      false,
		ToolNameChatRead:     false,
		ToolNameAnnounceFile: false,
		ToolNameShareFile:    false,
	}
	for _, d := range defs {
		want[d.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool def %q", name)
		}
	}
	if len(defs) != 5 {
		t.Errorf("tool count: got %d, want 5", len(defs))
	}
}

func TestCommsToolDefs_SchemasAreValidJSON(t *testing.T) {
	for _, d := range CommsToolDefs() {
		var schema map[string]any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("schema for %q is invalid JSON: %v", d.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("schema for %q: top-level type should be 'object'", d.Name)
		}
	}
}

func TestCommsRunner_NoGatewayReturnsError(t *testing.T) {
	r := CommsRunner{}
	_, err := r.Run(context.Background(), bridle.ToolCall{Name: ToolNameSendChat})
	if err == nil {
		t.Error("expected error when gateway is nil")
	}
}

func TestCommsRunner_SendChatSucceeds(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	args := mustJSON(map[string]any{"content": "hello operator", "reply_to": 42})
	res, err := r.Run(context.Background(), bridle.ToolCall{Name: ToolNameSendChat, Args: args})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	sent := g.snapshotSent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(sent))
	}
	if sent[0].Content != "hello operator" {
		t.Errorf("content: got %q", sent[0].Content)
	}
	if sent[0].ReplyTo != 42 {
		t.Errorf("reply_to: got %d, want 42", sent[0].ReplyTo)
	}
	var got map[string]int64
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	if got["msg_id"] == 0 {
		t.Error("result should carry msg_id")
	}
}

func TestCommsRunner_SendChatRejectsEmptyContent(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSendChat,
		Args: mustJSON(map[string]any{}),
	})
	if err != nil {
		t.Fatalf("Run should not return error for bad args; got %v", err)
	}
	if !strings.Contains(string(res), "error") {
		t.Errorf("expected error result, got %s", res)
	}
	if len(g.snapshotSent()) != 0 {
		t.Error("gateway should not be called with empty content")
	}
}

func TestCommsRunner_GatewayErrorsBecomeToolResultErrors(t *testing.T) {
	g := &fakeGateway{sendErr: errors.New("broker down")}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSendChat,
		Args: mustJSON(map[string]any{"content": "hi"}),
	})
	// Errors that turn-aborting failures get returned as the second
	// value; we want gateway errors to land in the JSON result so the
	// model can recover, not abort the turn.
	if err != nil {
		t.Fatalf("gateway error should not abort the turn; got %v", err)
	}
	if !strings.Contains(string(res), "broker down") {
		t.Errorf("expected error in result body: got %s", res)
	}
}

func TestCommsRunner_ReactToValidatesArgs(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	cases := []map[string]any{
		{"msg_id": 0, "emoji": "👍"},  // missing msg_id
		{"msg_id": 5, "emoji": ""},    // missing emoji
	}
	for _, args := range cases {
		res, _ := r.Run(context.Background(), bridle.ToolCall{
			Name: ToolNameReactTo,
			Args: mustJSON(args),
		})
		if !strings.Contains(string(res), "error") {
			t.Errorf("expected error for %v, got %s", args, res)
		}
	}
	if len(g.reactions) != 0 {
		t.Errorf("gateway should not have been called; got %d", len(g.reactions))
	}
}

func TestCommsRunner_ChatReadReturnsMessages(t *testing.T) {
	g := &fakeGateway{
		readResults: []ChatMessage{
			{ID: 100, From: "operator", Content: "hello", ReceivedAt: "2026-05-02T05:30:00Z"},
			{ID: 101, From: "anvil", Content: "world", ReplyTo: 100, ReceivedAt: "2026-05-02T05:31:00Z"},
		},
	}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameChatRead,
		Args: mustJSON(map[string]any{"thread_id": 100, "since_id": 0}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []ChatMessage `json:"messages"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(got.Messages))
	}
	if got.Messages[0].ReceivedAt == "" {
		t.Error("received_at should propagate to model (Lock 6 message age)")
	}
}

func TestCommsRunner_AnnounceFileSucceeds(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameAnnounceFile,
		Args: mustJSON(map[string]any{"path": "/tmp/doc.md", "description": "spec draft"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.announces) != 1 {
		t.Fatalf("expected 1 announce, got %d", len(g.announces))
	}
	if g.announces[0].Path != "/tmp/doc.md" {
		t.Errorf("path: got %q", g.announces[0].Path)
	}
	var got map[string]int64
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	if got["msg_id"] != 9001 {
		t.Errorf("msg_id: got %d", got["msg_id"])
	}
}

func TestCommsRunner_ShareFileRequiresRecipients(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	// Empty recipients
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameShareFile,
		Args: mustJSON(map[string]any{"path": "/x", "recipients": []string{}}),
	})
	if !strings.Contains(string(res), "error") {
		t.Error("empty recipients should error")
	}
	if len(g.shares) != 0 {
		t.Error("gateway should not be called")
	}
}

func TestCommsRunner_UnknownToolErrors(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	_, err := r.Run(context.Background(), bridle.ToolCall{Name: "not_a_real_tool"})
	if err == nil {
		t.Error("expected error for unknown tool name")
	}
}

func TestComposeRunner_DispatchesCommsToolsToCommsRunner(t *testing.T) {
	g := &fakeGateway{}
	otherCalled := false
	other := stubToolRunner(func(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
		otherCalled = true
		return json.RawMessage(`{}`), nil
	})
	r := ComposeRunner(CommsRunner{Gateway: g}, other)

	if _, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSendChat,
		Args: mustJSON(map[string]any{"content": "hi"}),
	}); err != nil {
		t.Fatal(err)
	}
	if otherCalled {
		t.Error("comms tool should not fall through to other runner")
	}
	if len(g.snapshotSent()) != 1 {
		t.Error("comms runner should have handled send_chat")
	}
}

func TestComposeRunner_DelegatesUnknownToOther(t *testing.T) {
	g := &fakeGateway{}
	otherCalled := false
	var seenName string
	other := stubToolRunner(func(_ context.Context, call bridle.ToolCall) (json.RawMessage, error) {
		otherCalled = true
		seenName = call.Name
		return json.RawMessage(`{"ok":true}`), nil
	})
	r := ComposeRunner(CommsRunner{Gateway: g}, other)

	if _, err := r.Run(context.Background(), bridle.ToolCall{Name: "aspect_specific_tool"}); err != nil {
		t.Fatal(err)
	}
	if !otherCalled {
		t.Error("unknown tool should fall through to other runner")
	}
	if seenName != "aspect_specific_tool" {
		t.Errorf("delegated tool name: got %q", seenName)
	}
}

func TestComposeRunner_NoNextErrorsCleanly(t *testing.T) {
	r := ComposeRunner(CommsRunner{Gateway: &fakeGateway{}}, nil)
	res, err := r.Run(context.Background(), bridle.ToolCall{Name: "missing"})
	if err != nil {
		t.Fatalf("composed runner with no next should not abort the turn: %v", err)
	}
	if !strings.Contains(string(res), "unknown tool") {
		t.Errorf("expected unknown tool error in result: got %s", res)
	}
}

func TestCommsRunner_ReactToMessageAliasMatchesReactTo(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	if _, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameReactToMessage,
		Args: mustJSON(map[string]any{"msg_id": 42, "emoji": "👍"}),
	}); err != nil {
		t.Fatalf("legacy alias should dispatch like react_to: %v", err)
	}
	if len(g.reactions) != 1 {
		t.Errorf("expected 1 reaction, got %d", len(g.reactions))
	}
	if g.reactions[0].MsgID != 42 || g.reactions[0].Emoji != "👍" {
		t.Errorf("wrong reaction: %+v", g.reactions[0])
	}
}

func TestCommsRunner_ContextCancelPropagatesAsGoError(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call
	_, err := r.Run(ctx, bridle.ToolCall{
		Name: ToolNameSendChat,
		Args: mustJSON(map[string]any{"content": "hi"}),
	})
	if err == nil {
		t.Fatal("cancelled ctx should propagate as Go error so bridle aborts the turn")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled: got %v", err)
	}
	if len(g.snapshotSent()) != 0 {
		t.Error("gateway should not be invoked when ctx is already cancelled")
	}
}

func TestCommsRunner_GatewayContextDeadlinePropagatesAsGoError(t *testing.T) {
	g := &fakeGateway{sendErr: context.DeadlineExceeded}
	r := CommsRunner{Gateway: g}
	_, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSendChat,
		Args: mustJSON(map[string]any{"content": "hi"}),
	})
	if err == nil {
		t.Fatal("gateway returning context.DeadlineExceeded must surface as Go error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap context.DeadlineExceeded: got %v", err)
	}
}

func TestCommsRunner_AnnounceFileRequiresDescription(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameAnnounceFile,
		Args: mustJSON(map[string]any{"path": "/x", "description": ""}),
	})
	if !strings.Contains(string(res), "error") {
		t.Errorf("empty description should error: got %s", res)
	}
	if len(g.announces) != 0 {
		t.Error("gateway should not be called with empty description")
	}
}

type stubToolRunner func(ctx context.Context, call bridle.ToolCall) (json.RawMessage, error)

func (s stubToolRunner) Run(ctx context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	return s(ctx, call)
}
