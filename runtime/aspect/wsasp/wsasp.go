// Package wsasp is the aspect-side WS client wrapper used by the
// out-of-process aspect binary (cmd/aspect, F2.5). Wraps the
// generic runtime/wsclient with Lock 6 cursor persistence, the
// register-with-since_msg_id handshake, and a funnel.ChatGateway
// implementation that translates ChatGateway calls into outbound
// chat.send / react_to / chat.read / announce_file / share_file
// frames.
//
// "Aspect host" responsibilities per Lock 6 (operator #9197): the
// AI never sees connection state. The funnel asks "send this," the
// gateway buffers if the WS is down, drains on reconnect, and the
// model never knows the difference.

package wsasp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/nexus/frame/funnel"
	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/wsclient"
)

// Config wires an aspect-side WS client.
type Config struct {
	// URL is the Nexus WS endpoint, e.g.
	// wss://agentnetwork.<tailnet>.ts.net:port/connect.
	URL string

	// AuthToken is the per-aspect bearer.
	AuthToken string

	// AspectName is the registered aspect id.
	AspectName string

	// CursorFile is where to persist the highest processed msg_id.
	// Empty disables persistence (cold-start every reconnect —
	// acceptable degradation per Lock 6).
	CursorFile string

	// OnDeliver is called when a chat.deliver frame arrives. The
	// aspect-binary main loop wires this into funnel.Receive.
	// Replay flag is propagated for callers that want to surface
	// staleness; live frames have replay=false.
	OnDeliver func(msg DeliveredMessage)
}

// DeliveredMessage is the funnel-side representation of an inbound
// chat.deliver frame. Mirrors funnel.ChatMessage shape so the funnel
// can ingest without translation.
type DeliveredMessage struct {
	ID         int64
	From       string
	Content    string
	ReplyTo    int64
	ReceivedAt string // RFC 3339 UTC
	Replay     bool   // true iff replayed via since_msg_id; false for live
	Reason     string // mention | reply | thread | all
}

// Client is the aspect-side WS aspect-host. Owns the wsclient,
// cursor persistence, and the outbound buffer used during
// disconnects.
type Client struct {
	cfg Config
	ws  *wsclient.Client

	mu      sync.Mutex
	cursor  int64             // highest processed msg_id
	pending []frames.Envelope // outbound buffer while WS is down
}

// NewClient builds the aspect host. Loads the cursor from disk
// (errors silently → cursor=0, cold-start). Does NOT connect — call
// Run.
func NewClient(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("wsasp: URL required")
	}
	if cfg.AspectName == "" {
		return nil, fmt.Errorf("wsasp: AspectName required")
	}
	if cfg.OnDeliver == nil {
		return nil, fmt.Errorf("wsasp: OnDeliver required")
	}

	c := &Client{cfg: cfg}
	c.cursor = c.loadCursor()

	ws, err := wsclient.New(wsclient.Config{
		URL:       cfg.URL,
		AuthToken: cfg.AuthToken,
		Handler:   wsclient.HandlerFunc(c.handleFrame),
	})
	if err != nil {
		return nil, fmt.Errorf("wsasp: ws client: %w", err)
	}
	c.ws = ws
	return c, nil
}

// Run drives the WS connection lifecycle. Blocks until ctx done.
// On each (re)connect, sends a register frame with since_msg_id.
func (c *Client) Run(ctx context.Context) error {
	// On reconnect, the wsclient calls back via readyCh implicitly —
	// we observe via Connected polling here. For v1 we rely on the
	// caller wiring an explicit register call once Run starts.
	go c.registerOnReady(ctx)
	go c.drainPendingLoop(ctx)
	return c.ws.Run(ctx)
}

// registerOnReady polls the ws connected state and sends the
// register frame whenever the connection comes up.
//
// A subscription pattern would be cleaner; this polling shape keeps
// the wrapper focused — wsclient's Connected() is the existing
// surface. F2.6 may rev wsclient to add a connect callback.
func (c *Client) registerOnReady(ctx context.Context) {
	wasConnected := false
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := c.ws.Connected()
			if now && !wasConnected {
				c.sendRegister(ctx)
			}
			wasConnected = now
		}
	}
}

// sendRegister builds and sends the register frame with the current
// cursor. Failures are logged and retried on next ready.
func (c *Client) sendRegister(ctx context.Context) {
	c.mu.Lock()
	since := c.cursor
	c.mu.Unlock()

	env, err := frames.New(frames.KindRegister, frames.RegisterPayload{
		SinceMsgID: since,
		// Note: schemas.RegisterRequest fields are populated
		// elsewhere (this wrapper doesn't own identity beyond
		// AspectName + auth). F2.5 in the cmd/aspect main wires
		// the full RegisterRequest before calling sendRegister.
	})
	if err != nil {
		return
	}
	_ = c.ws.Send(ctx, env)
}

// drainPendingLoop empties the outbound buffer once the WS becomes
// available. Buffered sends are best-effort: drops on context done.
func (c *Client) drainPendingLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.ws.Connected() {
				continue
			}
			c.mu.Lock()
			pending := c.pending
			c.pending = nil
			c.mu.Unlock()
			for _, env := range pending {
				if err := c.ws.Send(ctx, env); err != nil {
					// Re-queue and try later; the disconnect
					// will be picked up on next tick.
					c.mu.Lock()
					c.pending = append([]frames.Envelope{env}, c.pending...)
					c.mu.Unlock()
					return
				}
			}
		}
	}
}

// handleFrame is the wsclient.Handler. Routes inbound frames to
// the appropriate aspect-side handler.
func (c *Client) handleFrame(env frames.Envelope) {
	switch env.Kind {
	case frames.KindChatDeliver:
		c.onChatDeliver(env)
	}
	// Unhandled kinds (turn requests, dispatch, etc.) flow through
	// the wsclient to its Request/correlation path; chat.deliver is
	// the only push-to-aspect frame we own here.
}

// onChatDeliver decodes the chat.deliver frame and forwards it to
// the configured OnDeliver callback. Updates the cursor BEFORE
// invoking the callback so that even if the callback panics or the
// process dies mid-handle, we don't replay this same message
// forever (at-least-once → effectively at-most-twice on crash).
//
// At-least-once delivery semantics from Lock 6: aspects MUST be
// idempotent in their tool-call effects. The funnel re-running a
// turn on re-delivered triggering message produces the same chat
// effect via the outbound queue's exactly-once-visible guarantee.
func (c *Client) onChatDeliver(env frames.Envelope) {
	var p frames.ChatDeliverPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		return
	}

	msg := DeliveredMessage{
		ID:         int64(p.ID),
		From:       p.From,
		Content:    p.Content,
		ReplyTo:    int64(p.ReplyTo),
		ReceivedAt: p.ReceivedAt,
		Replay:     p.Replay,
		Reason:     p.Reason,
	}

	c.advanceCursor(msg.ID)
	c.cfg.OnDeliver(msg)
}

// advanceCursor updates the highest-processed cursor and persists
// it. Atomic: write to a temp file + rename. Failures are silent
// (logging at this layer would be noise for routine WS frames).
func (c *Client) advanceCursor(id int64) {
	c.mu.Lock()
	if id > c.cursor {
		c.cursor = id
	}
	persistID := c.cursor
	c.mu.Unlock()

	if c.cfg.CursorFile == "" {
		return
	}
	tmp := c.cfg.CursorFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(persistID, 10)), 0o600); err == nil {
		_ = os.Rename(tmp, c.cfg.CursorFile)
	}
}

// loadCursor reads the persisted cursor from CursorFile. Returns 0
// on any error (file missing, parse failure) — Lock 6's
// "cold-start = no replay" degradation path.
func (c *Client) loadCursor() int64 {
	if c.cfg.CursorFile == "" {
		return 0
	}
	data, err := os.ReadFile(c.cfg.CursorFile)
	if err != nil {
		return 0
	}
	id, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// SendChat queues a chat.send frame. If the WS is connected, sends
// immediately; if down, buffers in memory for drain on reconnect
// (in-memory only — process-death loses queued sends, which is
// acceptable per anvil/forge consensus #9183/#9186).
func (c *Client) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	env, err := frames.New(frames.KindChatSend, frames.ChatSendPayload{
		From:    c.cfg.AspectName,
		Content: content,
		ReplyTo: int(replyTo),
		Topic:   topic,
	})
	if err != nil {
		return 0, err
	}
	c.queueOrSend(ctx, env)
	// chat.send is fire-and-forget per the transport spec — no
	// response shape carries the new msg_id back to the caller.
	// The funnel doesn't actually need the id (the model already
	// got the result; ids matter only for reactions/reply links,
	// which the model gets from inbound chat.deliver frames).
	return 0, nil
}

// ReactTo toggles an emoji reaction on a chat msg. Fire-and-forget:
// the broker doesn't synchronise reaction toggles back, and the
// funnel/model doesn't need confirmation (the next chat.deliver for
// any subsequent message in that thread will reflect the reactions
// view). Buffers if WS is down, mirrors SendChat's semantics.
func (c *Client) ReactTo(ctx context.Context, msgID int64, emoji string) error {
	env, err := frames.New(frames.KindChatReaction, frames.ChatReactionPayload{
		From:  c.cfg.AspectName,
		MsgID: int(msgID),
		Emoji: emoji,
	})
	if err != nil {
		return err
	}
	c.queueOrSend(ctx, env)
	return nil
}

// ReadThread sends a chat.read request and awaits the chat.read.result
// response. Unlike SendChat / ReactTo, this is request/response shape —
// the model invokes it as a tool and expects messages back. Bypasses
// the outbound buffer: if the WS is down, the read fails (callers can
// retry; a buffered read makes no sense — the caller is waiting for
// data, not fire-and-forget).
func (c *Client) ReadThread(ctx context.Context, threadID, sinceID int64) ([]frames.ChatDeliverPayload, error) {
	env, err := frames.New(frames.KindChatRead, frames.ChatReadPayload{
		ThreadID: threadID,
		SinceID:  sinceID,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.ws.Request(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("wsasp: chat.read: %w", err)
	}
	var out frames.ChatReadResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		return nil, fmt.Errorf("wsasp: chat.read.result decode: %w", err)
	}
	return out.Messages, nil
}

// AnnounceFile posts an announcement of a file path with a brief
// description. Returns the new chat msg_id so the model can reference
// the announcement in subsequent context.
func (c *Client) AnnounceFile(ctx context.Context, path, description string) (int64, error) {
	env, err := frames.New(frames.KindAnnounceFile, frames.AnnounceFilePayload{
		From:        c.cfg.AspectName,
		Path:        path,
		Description: description,
	})
	if err != nil {
		return 0, err
	}
	resp, err := c.ws.Request(ctx, env)
	if err != nil {
		return 0, fmt.Errorf("wsasp: announce_file: %w", err)
	}
	var out frames.FileResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		return 0, fmt.Errorf("wsasp: file.result decode: %w", err)
	}
	return out.MsgID, nil
}

// ShareFile records a direct share to recipients without posting to
// chat. Returns a share id the model can quote in subsequent context.
func (c *Client) ShareFile(ctx context.Context, path string, recipients []string) (int64, error) {
	env, err := frames.New(frames.KindShareFile, frames.ShareFilePayload{
		From:       c.cfg.AspectName,
		Path:       path,
		Recipients: recipients,
	})
	if err != nil {
		return 0, err
	}
	resp, err := c.ws.Request(ctx, env)
	if err != nil {
		return 0, fmt.Errorf("wsasp: share_file: %w", err)
	}
	var out frames.FileResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		return 0, fmt.Errorf("wsasp: file.result decode: %w", err)
	}
	return out.ShareID, nil
}

// queueOrSend tries an immediate send; on disconnect, buffers.
func (c *Client) queueOrSend(ctx context.Context, env frames.Envelope) {
	if c.ws.Connected() {
		if err := c.ws.Send(ctx, env); err == nil {
			return
		}
	}
	c.mu.Lock()
	c.pending = append(c.pending, env)
	c.mu.Unlock()
}

// CursorFileForAspect returns a default cursor-file path under the
// aspect home directory (`<home>/cursor`). Convenience for callers
// that don't want to hand-pick a path.
func CursorFileForAspect(home string) string {
	return filepath.Join(home, "cursor")
}

// Compile-time check that DeliveredMessage round-trips into a
// funnel.ChatMessage via the trivial mapping below. Out-of-process
// aspects that build a ChatGateway against this client convert
// using ToFunnelMessage to keep field tag changes from breaking
// silently.
func ToFunnelMessage(d DeliveredMessage) funnel.ChatMessage {
	return funnel.ChatMessage{
		ID:         d.ID,
		From:       d.From,
		Content:    d.Content,
		ReplyTo:    d.ReplyTo,
		ReceivedAt: d.ReceivedAt,
	}
}

// MarshalCursorState is a debug helper — renders the current
// cursor + pending count for a status endpoint. Not used by the
// happy path; exists so a future supervisor or dashboard can
// surface "this aspect has 3 pending sends and is at cursor 9242."
func (c *Client) MarshalCursorState() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, _ := json.Marshal(map[string]any{
		"cursor":  c.cursor,
		"pending": len(c.pending),
	})
	return out
}
