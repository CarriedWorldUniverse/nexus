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

	readMessages       []int64
	readMessageResults map[int64]ChatMessage
	readMessageErr     error

	listSharedCalls  []int
	listSharedResult []SharedFileRef
	listSharedErr    error

	getSharedCalls  []int64
	getSharedResult map[int64]SharedFileRef
	getSharedErr    error
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

func (g *fakeGateway) ReadMessage(_ context.Context, msgID int64) (ChatMessage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readMessages = append(g.readMessages, msgID)
	if g.readMessageErr != nil {
		return ChatMessage{}, g.readMessageErr
	}
	if m, ok := g.readMessageResults[msgID]; ok {
		return m, nil
	}
	return ChatMessage{ID: msgID, From: "fake", Content: "fake"}, nil
}

func (g *fakeGateway) ListShared(_ context.Context, limit int) ([]SharedFileRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listSharedCalls = append(g.listSharedCalls, limit)
	if g.listSharedErr != nil {
		return nil, g.listSharedErr
	}
	return g.listSharedResult, nil
}

func (g *fakeGateway) GetShared(_ context.Context, shareID int64) (SharedFileRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.getSharedCalls = append(g.getSharedCalls, shareID)
	if g.getSharedErr != nil {
		return SharedFileRef{}, g.getSharedErr
	}
	if f, ok := g.getSharedResult[shareID]; ok {
		return f, nil
	}
	return SharedFileRef{ID: shareID}, nil
}

func (g *fakeGateway) snapshotSent() []sentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]sentMessage, len(g.sentMessages))
	copy(out, g.sentMessages)
	return out
}

func TestCommsToolDefs_HasAllTools(t *testing.T) {
	defs := CommsToolDefs()
	want := map[string]bool{
		ToolNameSendChat:        false,
		ToolNameReactTo:         false,
		ToolNameChatRead:        false,
		ToolNameAnnounceFile:    false,
		ToolNameShareFile:       false,
		ToolNameReadChatMessage: false,
		ToolNameReadChatThread:  false,
		ToolNameListShared:      false,
		ToolNameGetShared:       false,
		ToolNameStoreKnowledge:  false,
		ToolNameSearchKnowledge: false,
	}
	for _, d := range defs {
		want[d.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool def %q", name)
		}
	}
	if len(defs) != len(want) {
		t.Errorf("tool count: got %d, want %d", len(defs), len(want))
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

func TestCommsRunner_ReadMessageRequiresID(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameReadChatMessage,
		Args: mustJSON(map[string]any{}),
	})
	if !strings.Contains(string(res), "error") {
		t.Errorf("missing id should error: got %s", res)
	}
	if len(g.readMessages) != 0 {
		t.Error("gateway should not be called when id is missing")
	}
}

func TestCommsRunner_ReadMessageReturnsMessage(t *testing.T) {
	g := &fakeGateway{
		readMessageResults: map[int64]ChatMessage{
			42: {ID: 42, From: "anvil", Content: "hello"},
		},
	}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameReadChatMessage,
		Args: mustJSON(map[string]any{"id": 42}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out struct {
		Message ChatMessage `json:"message"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Message.ID != 42 || out.Message.From != "anvil" {
		t.Errorf("unexpected message: %+v", out.Message)
	}
}

func TestCommsRunner_ReadMessageGatewayErrorSurfacesToModel(t *testing.T) {
	g := &fakeGateway{readMessageErr: errors.New("message 99: not found")}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameReadChatMessage,
		Args: mustJSON(map[string]any{"id": 99}),
	})
	if err != nil {
		t.Fatalf("non-fatal gateway error must not bubble as Go err: %v", err)
	}
	if !strings.Contains(string(res), "not found") {
		t.Errorf("expected 'not found' in tool result, got %s", res)
	}
}

func TestCommsRunner_ListSharedClampsLimit(t *testing.T) {
	g := &fakeGateway{listSharedResult: []SharedFileRef{}}
	r := CommsRunner{Gateway: g}
	_, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameListShared,
		Args: mustJSON(map[string]any{"limit": 10000}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g.listSharedCalls) != 1 {
		t.Fatalf("expected 1 list call, got %d", len(g.listSharedCalls))
	}
	if g.listSharedCalls[0] != 200 {
		t.Errorf("limit should be clamped to 200, got %d", g.listSharedCalls[0])
	}
}

func TestCommsRunner_ListSharedAcceptsZeroAsDefault(t *testing.T) {
	g := &fakeGateway{listSharedResult: nil}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameListShared,
		Args: mustJSON(map[string]any{}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g.listSharedCalls) != 1 || g.listSharedCalls[0] != 0 {
		t.Errorf("zero limit must pass through to gateway: calls=%v", g.listSharedCalls)
	}
	// nil slice must marshal as [], never null
	if !strings.Contains(string(res), "[]") {
		t.Errorf("nil shared result must surface as []: %s", res)
	}
}

func TestCommsRunner_GetSharedRequiresID(t *testing.T) {
	g := &fakeGateway{}
	r := CommsRunner{Gateway: g}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameGetShared,
		Args: mustJSON(map[string]any{}),
	})
	if !strings.Contains(string(res), "error") {
		t.Errorf("missing id should error: got %s", res)
	}
	if len(g.getSharedCalls) != 0 {
		t.Error("gateway should not be called when id is missing")
	}
}

func TestCommsRunner_GetSharedNotFoundFlowsAsToolResult(t *testing.T) {
	g := &fakeGateway{getSharedErr: errors.New("shared 7: not found")}
	r := CommsRunner{Gateway: g}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameGetShared,
		Args: mustJSON(map[string]any{"id": 7}),
	})
	if err != nil {
		t.Fatalf("non-fatal gateway error must not bubble: %v", err)
	}
	if !strings.Contains(string(res), "not found") {
		t.Errorf("expected 'not found' in tool result, got %s", res)
	}
}

type fakeKnowledgeGateway struct {
	mu sync.Mutex

	storeCalls   []storeKnowledgeCall
	storeNextID  int64
	storeErr     error

	searchCalls   []KnowledgeQuery
	searchResults []KnowledgeHit
	searchErr     error
}

type storeKnowledgeCall struct {
	FromAgent string
	Topic     string
	Content   string
	Shared    bool
}

func (g *fakeKnowledgeGateway) StoreKnowledge(_ context.Context, fromAgent, topic, content string, shared bool) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.storeCalls = append(g.storeCalls, storeKnowledgeCall{
		FromAgent: fromAgent, Topic: topic, Content: content, Shared: shared,
	})
	if g.storeErr != nil {
		return 0, g.storeErr
	}
	if g.storeNextID == 0 {
		g.storeNextID = 100
	}
	g.storeNextID++
	return g.storeNextID, nil
}

func (g *fakeKnowledgeGateway) SearchKnowledge(_ context.Context, q KnowledgeQuery) ([]KnowledgeHit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.searchCalls = append(g.searchCalls, q)
	if g.searchErr != nil {
		return nil, g.searchErr
	}
	return g.searchResults, nil
}

func TestCommsRunner_StoreKnowledgeRequiresGateway(t *testing.T) {
	r := CommsRunner{Gateway: &fakeGateway{}, AspectID: "keel"}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameStoreKnowledge,
		Args: mustJSON(map[string]any{"topic": "t", "content": "c"}),
	})
	if !strings.Contains(string(res), "knowledge gateway not configured") {
		t.Errorf("expected gateway-not-configured error, got %s", res)
	}
}

func TestCommsRunner_StoreKnowledgeRequiresAspectID(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameStoreKnowledge,
		Args: mustJSON(map[string]any{"topic": "t", "content": "c"}),
	})
	if !strings.Contains(string(res), "aspect id not configured") {
		t.Errorf("expected aspect-id error, got %s", res)
	}
	if len(kg.storeCalls) != 0 {
		t.Error("gateway should not be called without aspect id")
	}
}

func TestCommsRunner_StoreKnowledgeRequiresFields(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "keel"}
	cases := []map[string]any{
		{"topic": "", "content": "c"},
		{"topic": "t", "content": ""},
	}
	for _, args := range cases {
		res, _ := r.Run(context.Background(), bridle.ToolCall{
			Name: ToolNameStoreKnowledge,
			Args: mustJSON(args),
		})
		if !strings.Contains(string(res), "required") {
			t.Errorf("expected required-field error, got %s for %+v", res, args)
		}
	}
	if len(kg.storeCalls) != 0 {
		t.Error("gateway should not be called when validation fails")
	}
}

func TestCommsRunner_StoreKnowledgePropagatesAspectID(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "verity"}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameStoreKnowledge,
		Args: mustJSON(map[string]any{"topic": "lore", "content": "the canon", "shared": true}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kg.storeCalls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(kg.storeCalls))
	}
	got := kg.storeCalls[0]
	if got.FromAgent != "verity" || got.Topic != "lore" || got.Content != "the canon" || !got.Shared {
		t.Errorf("unexpected store call: %+v", got)
	}
	if !strings.Contains(string(res), `"id"`) {
		t.Errorf("expected id in result, got %s", res)
	}
}

func TestCommsRunner_SearchKnowledgeDefaultsToOwnAndShared(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{{ID: 1, Topic: "t"}}}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "harrow"}
	_, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSearchKnowledge,
		Args: mustJSON(map[string]any{"text": "decoder"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kg.searchCalls) != 1 {
		t.Fatalf("expected 1 search call, got %d", len(kg.searchCalls))
	}
	q := kg.searchCalls[0]
	if q.Agent != "harrow" {
		t.Errorf("agent not propagated: got %q want %q", q.Agent, "harrow")
	}
	if !q.OwnAgent || !q.Shared {
		t.Errorf("expected default OwnAgent+Shared true, got own=%v shared=%v", q.OwnAgent, q.Shared)
	}
}

func TestCommsRunner_SearchKnowledgeRespectsExplicitFalse(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: []KnowledgeHit{}}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "harrow"}
	_, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSearchKnowledge,
		Args: mustJSON(map[string]any{"text": "x", "own_agent": false, "shared": false, "peers": []string{"verity"}}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := kg.searchCalls[0]
	if q.OwnAgent || q.Shared {
		t.Errorf("explicit false ignored: own=%v shared=%v", q.OwnAgent, q.Shared)
	}
	if len(q.Peers) != 1 || q.Peers[0] != "verity" {
		t.Errorf("peers not propagated: %+v", q.Peers)
	}
}

func TestCommsRunner_SearchKnowledgeClampsTopK(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "keel"}
	_, _ = r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSearchKnowledge,
		Args: mustJSON(map[string]any{"text": "x", "top_k": 9999}),
	})
	if kg.searchCalls[0].TopK != 50 {
		t.Errorf("top_k must clamp to 50, got %d", kg.searchCalls[0].TopK)
	}
}

func TestCommsRunner_SearchKnowledgeRequiresText(t *testing.T) {
	kg := &fakeKnowledgeGateway{}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "keel"}
	res, _ := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSearchKnowledge,
		Args: mustJSON(map[string]any{}),
	})
	if !strings.Contains(string(res), "text is required") {
		t.Errorf("expected 'text is required' error, got %s", res)
	}
	if len(kg.searchCalls) != 0 {
		t.Error("gateway should not be called when text is missing")
	}
}

func TestCommsRunner_SearchKnowledgeNilHitsBecomeEmptyArray(t *testing.T) {
	kg := &fakeKnowledgeGateway{searchResults: nil}
	r := CommsRunner{Gateway: &fakeGateway{}, Knowledge: kg, AspectID: "keel"}
	res, err := r.Run(context.Background(), bridle.ToolCall{
		Name: ToolNameSearchKnowledge,
		Args: mustJSON(map[string]any{"text": "anything"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(res), "[]") {
		t.Errorf("nil hits must surface as []: %s", res)
	}
}
