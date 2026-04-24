// Package wsclient is the aspect-and-Outpost-side WebSocket client.
// Handles dial + auth + reconnect + framed send/receive + correlation-
// id tracking for request/response pairs. Used by the aspect runtime
// in part 3 and the Outpost in part 6.
//
// Design: one Client per logical connection. Run() blocks until ctx
// is cancelled, transparently reconnecting on drop. Callers send
// frames via Send (fire-and-forget) or Request (awaits correlated
// response). Incoming frames with no pending correlation are
// delivered via the Handler callback.
package wsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
)

// Handler processes incoming frames that aren't correlated to a
// pending Request. The client calls Handle from its read goroutine;
// handlers should not block.
type Handler interface {
	Handle(env frames.Envelope)
}

// HandlerFunc adapts a plain function to the Handler interface.
type HandlerFunc func(env frames.Envelope)

// Handle implements Handler.
func (f HandlerFunc) Handle(env frames.Envelope) { f(env) }

// Config configures a Client.
type Config struct {
	// URL is the WS endpoint to dial, e.g. wss://nexus.host:7888/connect.
	URL string

	// AuthToken is sent as Authorization: Bearer on the upgrade.
	AuthToken string

	// Handler receives uncorrelated incoming frames. Required.
	Handler Handler

	// Logger is optional; nil falls back to slog.Default().
	Logger *slog.Logger

	// FailFirstConnect, when true, causes Run to return an error if
	// the initial dial fails. Aspects with NEXUS_OUTPOST set use this
	// to fail-loudly per transport spec §3.5. Aspects doing a pure
	// reconnect (no explicit outpost override) set false.
	FailFirstConnect bool

	// MinReconnectDelay / MaxReconnectDelay bound the exponential
	// backoff on reconnect. Defaults: 1s → 60s.
	MinReconnectDelay time.Duration
	MaxReconnectDelay time.Duration
}

// Client is a persistent WS connection that handles reconnection and
// request/response correlation. Safe for concurrent use of Send and
// Request after Run has started.
type Client struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	conn     *websocket.Conn
	pending  map[string]chan frames.Envelope
	connected bool

	// connCh broadcasts when a fresh connection becomes ready.
	// Callers of Send/Request block on it while the client is
	// reconnecting.
	readyCh chan struct{}
}

// New constructs a Client.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("wsclient: URL required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("wsclient: Handler required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.MinReconnectDelay == 0 {
		cfg.MinReconnectDelay = 1 * time.Second
	}
	if cfg.MaxReconnectDelay == 0 {
		cfg.MaxReconnectDelay = 60 * time.Second
	}

	return &Client{
		cfg:     cfg,
		log:     log,
		pending: make(map[string]chan frames.Envelope),
		readyCh: make(chan struct{}),
	}, nil
}

// Run drives the dial+serve+reconnect loop. Blocks until ctx done.
// Returns the first-connect error if FailFirstConnect is true and
// the initial dial fails; otherwise always returns ctx.Err() on exit.
func (c *Client) Run(ctx context.Context) error {
	first := true
	backoff := c.cfg.MinReconnectDelay

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := c.dialAndServe(ctx)
		if err != nil && first && c.cfg.FailFirstConnect {
			return fmt.Errorf("wsclient: initial connect failed: %w", err)
		}
		first = false

		if ctx.Err() != nil {
			return ctx.Err()
		}

		c.log.Warn("wsclient reconnecting", "err", err, "delay", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		// Exponential backoff, clamped.
		backoff *= 2
		if backoff > c.cfg.MaxReconnectDelay {
			backoff = c.cfg.MaxReconnectDelay
		}
	}
}

// dialAndServe opens one connection and runs its read loop until the
// connection drops. Resets backoff on successful connection.
func (c *Client) dialAndServe(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, c.cfg.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.AuthToken}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(1 << 20) // match broker's 1 MiB cap

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	// Signal readiness to any Send/Request waiters. Replace the
	// channel so future reconnects can block new waiters separately.
	close(c.readyCh)
	c.readyCh = make(chan struct{})
	c.mu.Unlock()

	c.log.Info("wsclient connected", "url", c.cfg.URL)

	defer func() {
		c.mu.Lock()
		c.connected = false
		c.conn = nil
		// Drain pending response channels — the outstanding Request
		// callers get a closed channel and interpret that as
		// disconnect-without-response.
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "client shutdown")
	}()

	return c.readLoop(ctx, conn)
}

// readLoop reads frames off conn and dispatches them to either a
// waiting Request or the Handler.
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if msgType != websocket.MessageText {
			c.log.Warn("non-text ws frame, ignoring", "type", msgType)
			continue
		}
		env, err := frames.Decode(data)
		if err != nil {
			c.log.Warn("frame decode failed", "err", err)
			continue
		}

		// If this frame correlates to a pending Request, fulfil it;
		// otherwise hand off to the Handler.
		if env.InReplyTo != "" {
			c.mu.Lock()
			ch, ok := c.pending[env.InReplyTo]
			if ok {
				delete(c.pending, env.InReplyTo)
			}
			c.mu.Unlock()
			if ok {
				ch <- env
				close(ch)
				continue
			}
			// No pending waiter — fall through to Handler. Might be a
			// late response after a Request timed out.
		}
		c.cfg.Handler.Handle(env)
	}
}

// Send writes a frame, fire-and-forget. Waits for connection if
// currently reconnecting. Blocking is bounded by ctx.
func (c *Client) Send(ctx context.Context, env frames.Envelope) error {
	conn, err := c.awaitConn(ctx)
	if err != nil {
		return err
	}
	raw, err := frames.Encode(env)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

// Request sends a frame and waits for the correlated response. The
// frame must be a NewRequest-shaped envelope (non-empty ID).
// Returns the response envelope or an error (timeout via ctx, or
// disconnect).
func (c *Client) Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error) {
	if env.ID == "" {
		return frames.Envelope{}, errors.New("wsclient.Request: envelope has no ID — use frames.NewRequest")
	}

	respCh := make(chan frames.Envelope, 1)
	c.mu.Lock()
	c.pending[env.ID] = respCh
	c.mu.Unlock()

	// If the write fails we still own the pending entry; clean up.
	cleanup := func() {
		c.mu.Lock()
		delete(c.pending, env.ID)
		c.mu.Unlock()
	}

	if err := c.Send(ctx, env); err != nil {
		cleanup()
		return frames.Envelope{}, err
	}

	select {
	case <-ctx.Done():
		cleanup()
		return frames.Envelope{}, ctx.Err()
	case resp, ok := <-respCh:
		if !ok {
			// Channel closed without value — disconnect before
			// response arrived.
			return frames.Envelope{}, errors.New("wsclient.Request: connection dropped before response")
		}
		return resp, nil
	}
}

// awaitConn blocks until either a connection is ready or ctx cancels.
func (c *Client) awaitConn(ctx context.Context) (*websocket.Conn, error) {
	for {
		c.mu.Lock()
		if c.connected && c.conn != nil {
			conn := c.conn
			c.mu.Unlock()
			return conn, nil
		}
		ready := c.readyCh
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
			// Re-check connected state on next iteration.
		}
	}
}

// Connected reports whether the client currently has an active WS.
// Useful for tests and observability.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}
