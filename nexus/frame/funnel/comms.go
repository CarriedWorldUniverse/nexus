// Comms tool surface — the bridle.ToolDefs an aspect's model can
// invoke mid-turn (Lock 3 of the aspect-funnel architecture). F1.4a
// lands the contract: tool registrations, schemas, and a ToolRunner
// that dispatches to a ChatGateway. F1.4b wires a concrete in-process
// gateway against the existing chat router.
//
// Tools shipped:
//
//   send_chat(content, reply_to?, topic?)  — post to chat
//   react_to(msg_id, emoji)                — toggle reaction
//   chat.read(thread_id, since_id?)        — pull thread history
//   announce_file(path, description)       — surface a file
//   share_file(path, recipients[])         — direct share
//
// All five run as standard bridle ToolDefs invoked by the model.
// Nothing special-cased at the bridle layer — the runner sees them
// as plain tool calls and dispatches to the gateway.
//
// Per Lock 3: mid-turn calls are first-class and unfiltered. The
// post-hoc filter (F1.1) governs only the natural final reply,
// never these intentional sends.

package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/bridle"
)

// ChatGateway is the funnel's seam onto the actual chat surface. The
// in-process Frame implements this against the broker's chat router
// (F1.4b). Out-of-process aspects implement it against the WS
// outbound queue once F1.4d-ish lands.
//
// Implementations MUST be safe to call from the deliberation
// goroutine; the funnel does not fan out, but tests will exercise
// concurrent calls. Methods return an error if the call cannot be
// performed; the caller (ToolRunner) translates errors into
// tool-result JSON the model can read.
type ChatGateway interface {
	// SendChat posts a message to chat as the aspect. Returns the
	// new message id and any error. ReplyTo and Topic are optional;
	// zero/empty means top-level post in the default topic.
	SendChat(ctx context.Context, content string, replyTo int64, topic string) (msgID int64, err error)

	// ReactTo toggles a reaction on the named message. Idempotent:
	// reacting twice with the same emoji removes it.
	ReactTo(ctx context.Context, msgID int64, emoji string) error

	// ReadThread returns messages in a thread, optionally bounded
	// to those after sinceID. The funnel hands the raw messages
	// back to the model as the tool result; render details live at
	// the gateway.
	ReadThread(ctx context.Context, threadID int64, sinceID int64) ([]ChatMessage, error)

	// AnnounceFile surfaces a path to chat with a description.
	// Returns the chat msg_id of the announcement.
	AnnounceFile(ctx context.Context, path, description string) (msgID int64, err error)

	// ShareFile shares a path directly with named recipients. No
	// chat post is created (use AnnounceFile for that). Returns a
	// share id for the model to reference.
	ShareFile(ctx context.Context, path string, recipients []string) (shareID int64, err error)
}

// ChatMessage is the gateway-level representation of a chat row.
// Mirrors broker schema fields the funnel/model cares about. Kept
// terse — the gateway translates to/from broker types so the
// funnel doesn't import storage.
type ChatMessage struct {
	ID         int64  `json:"id"`
	From       string `json:"from"`
	Content    string `json:"content"`
	ReplyTo    int64  `json:"reply_to,omitempty"`
	Topic      string `json:"topic,omitempty"`
	ReceivedAt string `json:"received_at"` // RFC 3339 UTC per Lock 6
}

// CommsToolNames are the canonical strings the model uses for tool
// calls. Centralized so the runner and the ToolDef list can't drift.
const (
	ToolNameSendChat        = "send_chat"
	ToolNameReactTo         = "react_to"
	ToolNameReactToMessage  = "react_to_message" // legacy alias from Lock 3 — same handler as react_to
	ToolNameChatRead        = "chat.read"
	ToolNameAnnounceFile    = "announce_file"
	ToolNameShareFile       = "share_file"
)

// CommsToolDefs returns the set of bridle.ToolDef registrations for
// the comms surface. Pass these into Config.Tools alongside any
// aspect-specific tools. The schemas use plain JSON Schema so they
// work with every provider bridle supports.
func CommsToolDefs() []bridle.ToolDef {
	return []bridle.ToolDef{
		{
			Name:        ToolNameSendChat,
			Description: "Post a message to the group chat. Use to ask clarifying questions, share status, or reply to an addressed message. Use @<aspect> to mention a specific aspect; replies go to the parent's author plus any explicit @-mentions.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content":  map[string]any{"type": "string", "description": "Message body. Use @aspect to mention."},
					"reply_to": map[string]any{"type": "integer", "description": "Optional msg_id of the message you're replying to."},
					"topic":    map[string]any{"type": "string", "description": "Optional topic name for feature-scoped threads."},
				},
				"required": []string{"content"},
			}),
		},
		{
			Name:        ToolNameReactTo,
			Description: "Toggle a reaction on a message. Use to acknowledge receipt (👀), agree (👍), mark complete (✅), or otherwise signal without posting a reply.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"msg_id": map[string]any{"type": "integer", "description": "Message id to react to."},
					"emoji":  map[string]any{"type": "string", "description": "Single emoji."},
				},
				"required": []string{"msg_id", "emoji"},
			}),
		},
		{
			Name:        ToolNameChatRead,
			Description: "Read messages from a thread. Use when a chat refers to context you weren't pushed (Lock 2: non-recipients pull via this tool, no fresh deliberation triggered).",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thread_id": map[string]any{"type": "integer", "description": "Root message id of the thread to read."},
					"since_id":  map[string]any{"type": "integer", "description": "Optional: only return messages with id > since_id."},
				},
				"required": []string{"thread_id"},
			}),
		},
		{
			Name:        ToolNameAnnounceFile,
			Description: "Surface a file to chat with a brief description. Posts a chat message with a clickable file affordance.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":        map[string]any{"type": "string", "description": "Absolute or workspace-relative path."},
					"description": map[string]any{"type": "string", "description": "Short description for the chat post."},
				},
				"required": []string{"path", "description"},
			}),
		},
		{
			Name:        ToolNameShareFile,
			Description: "Share a file directly with named recipients without posting to chat. Returns a share id the recipients can resolve via the file system.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Absolute or workspace-relative path."},
					"recipients": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Aspect ids to share with."},
				},
				"required": []string{"path", "recipients"},
			}),
		},
	}
}

// CommsRunner is a bridle.ToolRunner that dispatches the five comms
// tools to a ChatGateway. Other tools (provider-specific or aspect-
// specific) are NOT handled here — wrap CommsRunner with a fan-out
// runner that delegates by tool name.
//
// Run returns tool-result JSON the model can interpret. Errors from
// the gateway are translated into a JSON object with an "error"
// field rather than returning Go errors directly — bridle's
// contract is that ToolRunner errors abort the turn, but a comms
// failure (e.g. broker rejected the post) shouldn't abort; the
// model should see the error and decide what to do.
type CommsRunner struct {
	Gateway ChatGateway
}

// Run dispatches a tool call by name. Unknown tool names return an
// error so a wrapping fan-out runner can take the next handler.
//
// Context cancellation propagates as a Go error so bridle aborts the
// turn rather than continuing on a doomed deadline; gateway errors
// for any other reason land in the JSON tool-result so the model can
// recover.
func (r CommsRunner) Run(ctx context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	if r.Gateway == nil {
		return nil, fmt.Errorf("CommsRunner: no gateway configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch call.Name {
	case ToolNameSendChat:
		return r.runSendChat(ctx, call.Args)
	case ToolNameReactTo, ToolNameReactToMessage:
		return r.runReactTo(ctx, call.Args)
	case ToolNameChatRead:
		return r.runChatRead(ctx, call.Args)
	case ToolNameAnnounceFile:
		return r.runAnnounceFile(ctx, call.Args)
	case ToolNameShareFile:
		return r.runShareFile(ctx, call.Args)
	default:
		return nil, fmt.Errorf("CommsRunner: unknown tool %q", call.Name)
	}
}

// gatewayErrorResult turns a gateway error into either a Go error
// (for context cancellation — bridle should abort the turn) or a
// JSON error result (for everything else — the model can recover).
func gatewayErrorResult(err error) (json.RawMessage, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return errorResult(err), nil
}

func (r CommsRunner) runSendChat(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Content string `json:"content"`
		ReplyTo int64  `json:"reply_to"`
		Topic   string `json:"topic"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return errorResult(err), nil
	}
	if args.Content == "" {
		return errorResult(fmt.Errorf("content is required")), nil
	}
	id, err := r.Gateway.SendChat(ctx, args.Content, args.ReplyTo, args.Topic)
	if err != nil {
		return gatewayErrorResult(err)
	}
	return mustJSON(map[string]any{"msg_id": id}), nil
}

func (r CommsRunner) runReactTo(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		MsgID int64  `json:"msg_id"`
		Emoji string `json:"emoji"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return errorResult(err), nil
	}
	if args.MsgID == 0 || args.Emoji == "" {
		return errorResult(fmt.Errorf("msg_id and emoji are required")), nil
	}
	if err := r.Gateway.ReactTo(ctx, args.MsgID, args.Emoji); err != nil {
		return gatewayErrorResult(err)
	}
	return mustJSON(map[string]any{"ok": true}), nil
}

func (r CommsRunner) runChatRead(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ThreadID int64 `json:"thread_id"`
		SinceID  int64 `json:"since_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return errorResult(err), nil
	}
	if args.ThreadID == 0 {
		return errorResult(fmt.Errorf("thread_id is required")), nil
	}
	msgs, err := r.Gateway.ReadThread(ctx, args.ThreadID, args.SinceID)
	if err != nil {
		return gatewayErrorResult(err)
	}
	return mustJSON(map[string]any{"messages": msgs}), nil
}

func (r CommsRunner) runAnnounceFile(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return errorResult(err), nil
	}
	if args.Path == "" || args.Description == "" {
		return errorResult(fmt.Errorf("path and description are required")), nil
	}
	id, err := r.Gateway.AnnounceFile(ctx, args.Path, args.Description)
	if err != nil {
		return gatewayErrorResult(err)
	}
	return mustJSON(map[string]any{"msg_id": id}), nil
}

func (r CommsRunner) runShareFile(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Path       string   `json:"path"`
		Recipients []string `json:"recipients"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return errorResult(err), nil
	}
	if args.Path == "" || len(args.Recipients) == 0 {
		return errorResult(fmt.Errorf("path and recipients are required")), nil
	}
	id, err := r.Gateway.ShareFile(ctx, args.Path, args.Recipients)
	if err != nil {
		return gatewayErrorResult(err)
	}
	return mustJSON(map[string]any{"share_id": id}), nil
}

// errorResult renders an error into the standard tool-result shape
// so the model can read and respond to it. Returning errors as JSON
// rather than Go errors prevents bridle from aborting the turn —
// a chat-send failure is information for the model, not a fatal.
func errorResult(err error) json.RawMessage {
	return mustJSON(map[string]any{"error": err.Error()})
}

// mustJSON marshals or panics. Used only for static schemas and
// trivially-encodable result objects where marshaling failure
// indicates a programmer error in the funnel itself.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("funnel/comms: json.Marshal failed: " + err.Error())
	}
	return b
}

// ComposeRunner returns a bridle.ToolRunner that dispatches first to
// the comms runner (handling the five comms tools) and falls back
// to the supplied next runner for everything else. Use this when an
// aspect has its own tools alongside the comms surface.
//
// If next is nil, unknown tools surface as a tool-result error
// (the model can read it and recover) rather than aborting the turn.
func ComposeRunner(comms CommsRunner, next bridle.ToolRunner) bridle.ToolRunner {
	return composedRunner{comms: comms, next: next}
}

type composedRunner struct {
	comms CommsRunner
	next  bridle.ToolRunner
}

func (r composedRunner) Run(ctx context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	switch call.Name {
	case ToolNameSendChat, ToolNameReactTo, ToolNameChatRead, ToolNameAnnounceFile, ToolNameShareFile:
		return r.comms.Run(ctx, call)
	}
	if r.next == nil {
		return errorResult(fmt.Errorf("unknown tool %q", call.Name)), nil
	}
	return r.next.Run(ctx, call)
}
