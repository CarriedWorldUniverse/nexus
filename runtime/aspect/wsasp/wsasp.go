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

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// Config wires an aspect-side WS client.
type Config struct {
	// URL is the Nexus WS endpoint, e.g.
	// wss://agentnetwork.<tailnet>.ts.net:port/connect.
	URL string

	// AuthToken is the per-aspect bearer.
	AuthToken string

	// TokenProvider, when set, is called before each WS dial to
	// obtain a fresh bearer token. Passed through to wsclient.Config.
	// Callers with JWT-based auth should wire a closure that re-validates
	// against the keyfile endpoint so that an expired token doesn't
	// cause permanent reconnect failures.
	TokenProvider func(ctx context.Context) (string, error)

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

	// Register is the full schemas.RegisterRequest the wrapper sends
	// at handshake. The aspect binary populates this before calling
	// Run; wsasp injects it into the register frame alongside the
	// Lock 6 since_msg_id cursor. SessionID, PID, StartedAt, etc.
	// are caller's responsibility — wsasp doesn't own identity.
	Register schemas.RegisterRequest
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

	// ThreadRoot is the linked-list thread identity (task #226). The
	// funnel uses it to derive a per-thread session id so each thread
	// gets its own claude-code jsonl (no SessionTail bleed across
	// threads). Zero = legacy/pre-#226 row.
	ThreadRoot int64
}

// Client is the aspect-side WS aspect-host. Owns the wsclient,
// cursor persistence, and the outbound buffer used during
// disconnects.
type Client struct {
	cfg Config
	ws  *wsclient.Client

	mu         sync.Mutex
	cursor     int64             // highest processed msg_id
	pending    []frames.Envelope // outbound buffer while WS is down
	registered chan struct{}     // closed when register has been sent on the current connection
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
		URL:           cfg.URL,
		AuthToken:     cfg.AuthToken,
		TokenProvider: cfg.TokenProvider,
		Handler:       wsclient.HandlerFunc(c.handleFrame),
	})
	if err != nil {
		return nil, fmt.Errorf("wsasp: ws client: %w", err)
	}
	c.ws = ws
	return c, nil
}

// Run drives the WS connection lifecycle. Blocks until ctx done.
// On each (re)connect a register frame is sent before any buffered
// chat sends are flushed — the broker must see register first or
// it can't attribute follow-up frames to this aspect on this conn.
func (c *Client) Run(ctx context.Context) error {
	registered := make(chan struct{})
	c.setRegisteredBarrier(registered)

	go c.handleConnectEvents(ctx)
	go c.drainPendingLoop(ctx)
	return c.ws.Run(ctx)
}

// setRegisteredBarrier replaces the per-connection register barrier
// under the client lock. Callers must not touch c.registered directly.
func (c *Client) setRegisteredBarrier(ch chan struct{}) {
	c.mu.Lock()
	c.registered = ch
	c.mu.Unlock()
}

// registeredBarrier returns the current per-connection register
// barrier. Callers wait on it (or close it) without holding the lock.
func (c *Client) registeredBarrier() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}

// handleConnectEvents subscribes to wsclient connect/disconnect
// transitions. On connect: send register, then close the barrier
// so drainPendingLoop is allowed to flush. On disconnect: install
// a fresh barrier so the next connect cycle re-gates the drain.
func (c *Client) handleConnectEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-c.ws.Events():
			if !ok {
				return
			}
			if ev.Connected {
				c.sendRegister(ctx)
				barrier := c.registeredBarrier()
				select {
				case <-barrier:
					// Already closed (duplicate Connected=true event) — leave it.
				default:
					close(barrier)
				}
			} else {
				c.setRegisteredBarrier(make(chan struct{}))
			}
		}
	}
}

// sendRegister builds and sends the register frame with the current
// cursor and the caller-supplied RegisterRequest. Failures are
// logged and retried on next ready.
func (c *Client) sendRegister(ctx context.Context) {
	c.mu.Lock()
	since := c.cursor
	c.mu.Unlock()

	env, err := frames.New(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: c.cfg.Register,
		SinceMsgID:      since,
	})
	if err != nil {
		return
	}
	_ = c.ws.Send(ctx, env)
}

// drainPendingLoop flushes the outbound buffer once register has
// completed on the current connection. On every (re)connect cycle
// it re-reads the barrier and waits for it to close before draining.
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
			barrier := c.registeredBarrier()
			select {
			case <-ctx.Done():
				return
			case <-barrier:
				// Register has been sent on this connection.
			}

			// Disconnect could have happened while we waited; a fresh barrier is
			// installed by handleConnectEvents on the disconnect event, but our
			// snapshot is stale. Re-check connectedness — on false, next tick
			// picks up the new barrier and waits for the next register.
			if !c.ws.Connected() {
				continue
			}

			c.mu.Lock()
			pending := c.pending
			c.pending = nil
			c.mu.Unlock()
			for _, env := range pending {
				if err := c.ws.Send(ctx, env); err != nil {
					c.mu.Lock()
					c.pending = append([]frames.Envelope{env}, c.pending...)
					c.mu.Unlock()
					break // back to the ticker; next reconnect will re-arm the barrier and retry drain
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
		ThreadRoot: int64(p.ThreadRoot),
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

// Request sends a request frame and awaits the correlated response.
// Used for fire-and-await flows like session.refresh that need a typed
// reply. Bypasses the outbound buffer: callers must accept that a
// request fails fast if the WS is down (no buffering — a stale reply
// arriving long after the caller has timed out is worse than a clean
// failure).
func (c *Client) Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error) {
	return c.ws.Request(ctx, env)
}

// errNotConnected is returned by SendBestEffort when the underlying
// wsclient isn't connected. Sentinel so callers can distinguish
// "wire down" from a real wire-level write error.
var errNotConnected = fmt.Errorf("wsasp: not connected")

// queueOrSend tries an immediate send; on disconnect or pre-register,
// buffers. Live sends must wait behind register on a fresh connection
// for the same reason drainPendingLoop does — the broker can't
// attribute a chat.send to this aspect until register lands on the
// new connection. Non-blocking barrier check: if not yet registered,
// buffer and let drainPendingLoop flush after the barrier closes.
func (c *Client) queueOrSend(ctx context.Context, env frames.Envelope) {
	if c.ws.Connected() {
		barrier := c.registeredBarrier()
		select {
		case <-barrier:
			if err := c.ws.Send(ctx, env); err == nil {
				return
			}
		default:
			// Connection is up but register hasn't been sent yet — fall
			// through to buffer so drain delivers in the right order.
		}
	}
	c.mu.Lock()
	c.pending = append(c.pending, env)
	c.mu.Unlock()
}

// SendBestEffort attempts an immediate send and drops on disconnect.
// Use for fire-and-forget streams (observability) where stale frames
// surfacing minutes after their turn-of-origin are worse than missing
// frames. The pending-buffer replay semantics are wrong for these —
// renderers would see "TurnFrames from 5 minutes ago appearing as
// live" after a reconnect.
//
// Returns the underlying Send error (if any) so the caller can log
// at the right severity for its use case. Returns nil when the
// connection was up and the write succeeded.
func (c *Client) SendBestEffort(ctx context.Context, env frames.Envelope) error {
	if !c.ws.Connected() {
		return errNotConnected
	}
	return c.ws.Send(ctx, env)
}

// CursorFileForAspect returns a default cursor-file path under the
// aspect home directory (`<home>/cursor`). Convenience for callers
// that don't want to hand-pick a path.
func CursorFileForAspect(home string) string {
	return filepath.Join(home, "cursor")
}

// Connected reports whether the underlying WS is currently open.
// Surfaces wsclient.Connected for callers that want to render a
// connection state (agora's status line, observability counters,
// etc.). Cheap; no locks beyond what wsclient itself takes.
func (c *Client) Connected() bool {
	return c.ws.Connected()
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
