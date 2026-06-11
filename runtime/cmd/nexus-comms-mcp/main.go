// Command nexus-comms-mcp bridges nexus's WebSocket chat substrate to
// stdio MCP. Long-running process: one process == one nexus identity ==
// one held-open /connect WS. Stdio half exposes comms tools to whichever
// MCP client (claude-code subprocess, ad-hoc CLI, etc.) launched it.
//
// Spec: agent-network/docs/2026-05-11-nexus-comms-mcp-spec.md.
//
// Part 2 scope: stdio MCP server wired in, send_chat tool registered
// and bridged to chat.send WS frame. Verified by hand-rolled MCP client
// sending JSON-RPC over stdin and confirming the message lands in the
// nexus chat store. read_chat and friends arrive in Part 3.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func main() {
	var (
		nexusURL     = flag.String("nexus-url", "", "Override the WS URL (default: derived from keyfile envelope, with scheme rewritten http→ws/https→wss and /connect appended)")
		keyfilePath  = flag.String("keyfile", "", "Path to the aspect keyfile JSON (required for keyfile auth)")
		opToken      = flag.String("operator-token", "", "Operator JWT for non-keyfile auth (mutually exclusive with --keyfile)")
		opTokenFile  = flag.String("operator-token-file", "", "Read operator JWT from this file (alternative to --operator-token)")
		insecureSkip = flag.Bool("insecure-skip-verify", false, "Skip TLS cert verification (dev/self-signed only — do NOT use in production)")
		logLevel     = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile      = flag.String("log-file", "", "Write logs here instead of stderr; stdout is always reserved for the MCP protocol stream")
		noMCP        = flag.Bool("no-mcp", false, "Skip starting the stdio MCP server — transport-only mode for diagnostics")
		inboxCap     = flag.Int("inbox-buffer", 500, "Max chat.deliver frames buffered before oldest is dropped")
		doRegister   = flag.Bool("register", true, "Send a register frame after connecting so the broker delivers chat.deliver pushes (set false for diagnostics or when an agentfunnel for this identity already owns the WS slot)")
		startSince   = flag.Int64("since-msg-id", 0, "Initial since_msg_id for the first register frame — non-zero asks Nexus to replay addressed messages newer than this id (Lock 6). Default 0 means no replay (cold start, live frames only).")
	)
	flag.Parse()

	// MCP stdio uses stdout for protocol. Force logs off stdout: if
	// --log-file is unset, send to stderr; never to os.Stdout.
	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-comms-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Startup auth retry: if the broker is down when the MCP launches (a
	// session started during a broker restart/blip), don't exit. The runtime
	// path already reconnects indefinitely (NEX-237 + wsclient backoff), so
	// startup should be just as resilient — otherwise a momentary broker
	// outage at session start drops nexus-comms entirely and needs a manual
	// /mcp. Retry the validate handshake with exponential backoff, bounded so
	// a genuinely bad keyfile surfaces as a failure rather than hanging
	// "connecting" forever.
	var auth *authInfo
	backoff := 1 * time.Second
	for attempt := 1; ; attempt++ {
		var aerr error
		auth, aerr = resolveAuth(*keyfilePath, *opToken, *opTokenFile, *nexusURL, *insecureSkip, log)
		if aerr == nil {
			break
		}
		if attempt >= 10 {
			log.Error("auth setup failed after retries; giving up", "err", aerr, "attempts", attempt)
			os.Exit(2)
		}
		log.Warn("auth setup failed; retrying (broker may be down at startup)",
			"err", aerr, "attempt", attempt, "delay", backoff)
		select {
		case <-ctx.Done():
			log.Error("auth setup interrupted before success", "err", aerr)
			os.Exit(2)
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}

	log.Info("nexus-comms-mcp starting",
		"aspect", auth.aspect,
		"nexus_url", auth.wsURL,
		"jwt_expires_at", auth.expiresAt.Format(time.RFC3339))

	// Inbox buffer captures every chat.deliver pushed by the broker.
	// Sized for "the operator typing fast for a few minutes" with
	// generous headroom; falls behind via FIFO drop, not memory growth.
	inbox := newInboxBuffer(*inboxCap)
	if *startSince > 0 {
		inbox.seedHighest(*startSince)
	}

	// Build the WS client. The Handler routes every uncorrelated frame:
	// chat.deliver goes into the inbox; everything else gets logged at
	// debug. Request/response correlation (chat.list etc.) is handled
	// inside wsclient itself — those never reach Handler.
	wsHandler := wsclient.HandlerFunc(func(env frames.Envelope) {
		if env.Kind == frames.KindChatDeliver {
			var p frames.ChatDeliverPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				log.Warn("chat.deliver payload decode failed", "err", err)
				return
			}
			inbox.add(p)
			log.Debug("chat.deliver captured", "id", p.ID, "from", p.From, "reason", p.Reason)
			return
		}
		// Surface register-side errors prominently — these mean the
		// broker rejected our handshake and we won't get chat pushes.
		if env.Kind == "register.error" || env.Kind == "register.ack" {
			log.Info("register response", "kind", env.Kind, "payload", string(env.Payload))
			return
		}
		log.Debug("uncorrelated frame received", "kind", env.Kind, "id", env.ID, "payload", string(env.Payload))
	})

	// FailFirstConnect: false so a transient broker outage between
	// auth-success and the first WS dial doesn't tear the MCP down.
	// Tools surface "broker unavailable" via b.brokerDown() in that
	// window; wsclient keeps reconnecting in the background and the
	// MCP stays addressable to claude-code throughout.
	//
	// (Note: this only helps post-auth. If the broker is down at
	// MCP startup, resolveAuth fails earlier at the validate POST
	// and the MCP still exits. Wrapping resolveAuth in a retry loop
	// is a separate, larger change.)
	// NEX-237: JWT expires after the broker-side TTL (~12h default).
	// When wsclient reconnects (sleep/wake, network blip, broker
	// restart) it dials with the cached JWT — if that JWT has
	// expired the broker returns 401, kicking off an exponential-
	// backoff loop that never recovers without manual MCP restart.
	// TokenProvider re-validates the keyfile to mint a fresh JWT
	// before each dial when the cache is empty or near-expiry.
	//
	// Operator-token auth has no keyfile to refresh against — that
	// path falls through to using the static AuthToken, matching
	// pre-NEX-237 behaviour. Operator must rotate the token
	// out-of-band when it expires.
	tokenCache := &jwtCache{}
	tokenCache.Set(auth.jwt, auth.expiresAt)
	var tokenProvider func(ctx context.Context) (string, error)
	if auth.keyfile != nil && auth.keyfileClient != nil {
		kf := auth.keyfile
		client := auth.keyfileClient
		tokenProvider = func(ctx context.Context) (string, error) {
			jwt, expires := tokenCache.Get()
			if jwt != "" && time.Until(expires) > 1*time.Minute {
				return jwt, nil
			}
			fresh, ferr := client.Validate(ctx, kf)
			if ferr != nil {
				log.Warn("nexus-comms-mcp: TokenProvider re-validate failed, using cached token",
					"err", ferr)
				return "", ferr
			}
			tokenCache.Set(fresh.SessionJWT, fresh.SessionExpiresAt)
			log.Info("nexus-comms-mcp: TokenProvider re-validated via keyfile",
				"expires", fresh.SessionExpiresAt.Format(time.RFC3339))
			return fresh.SessionJWT, nil
		}
	}

	wsCli, err := wsclient.New(wsclient.Config{
		URL:              auth.wsURL,
		AuthToken:        auth.jwt,
		TokenProvider:    tokenProvider,
		Handler:          wsHandler,
		Logger:           log,
		FailFirstConnect: false,
	})
	if err != nil {
		log.Error("wsclient.New", "err", err)
		os.Exit(2)
	}

	// Session id is fresh per process — nexus-comms-mcp isn't a
	// continuous agent session, just a transport. Persisting it across
	// restarts would conflict with agentfunnel-owned aspect sessions.
	sessionID := uuid.NewString()

	// Connect-event handler: logs transitions and sends a register
	// frame on each fresh connect (so the broker delivers chat.deliver
	// pushes to us). For operator-token mode we skip register — the
	// operator subscription is keyed off the JWT itself by the broker.
	go func() {
		for ev := range wsCli.Events() {
			if !ev.Connected {
				log.Warn("ws disconnected", "url", auth.wsURL)
				continue
			}
			log.Info("ws connected", "url", auth.wsURL)
			if *doRegister && auth.aspect != "operator" {
				if err := sendRegister(ctx, wsCli, auth, sessionID, inbox.highest(), log); err != nil {
					log.Warn("register frame failed", "err", err)
				}
			}
		}
	}()

	// Run the WS in a goroutine so we can run the MCP server on the
	// main goroutine (mcpserver.ServeStdio blocks on stdin EOF).
	wsErrCh := make(chan error, 1)
	go func() { wsErrCh <- wsCli.Run(ctx) }()

	if *noMCP {
		// Diagnostic mode: just hold the WS open. Exit on ctx done.
		log.Info("--no-mcp: holding WS open, no stdio server")
		err := <-wsErrCh
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("ws client exited with error", "err", err)
			os.Exit(2)
		}
		log.Info("nexus-comms-mcp stopped")
		return
	}

	// Build the MCP server + register tools. The bridge gives every
	// tool handler a way to talk to the WS connection.
	bridge := &wsBridge{
		ws:           wsCli,
		inbox:        inbox,
		defaultFrom:  auth.aspect,
		log:          log,
		writeTimeout: 5 * time.Second,
	}
	srv := newMCPServer(bridge, log)

	log.Info("starting stdio MCP server", "aspect", auth.aspect)
	mcpErrCh := make(chan error, 1)
	go func() { mcpErrCh <- mcpserver.ServeStdio(srv) }()

	// Either side terminating ends the process. Stdin EOF (client
	// closed) → MCP returns nil; SIGINT → ctx done → WS Run returns
	// ctx.Err(). Cleanly join both.
	select {
	case err := <-mcpErrCh:
		stop() // trigger WS shutdown
		<-wsErrCh
		if err != nil {
			log.Error("mcp server exited with error", "err", err)
			os.Exit(2)
		}
	case err := <-wsErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("ws client exited with error", "err", err)
		}
		stop()
		// MCP server doesn't have a graceful stop hook — closing stdin
		// is the documented way, but we don't own stdin. Just exit.
	}

	log.Info("nexus-comms-mcp stopped")
}

// authInfo carries everything needed to dial /connect. Populated by
// resolveAuth from either keyfile or operator-token flags.
type authInfo struct {
	aspect    string    // sub claim — aspect name or "operator"
	jwt       string    // bearer for /connect
	wsURL     string    // wss://host:port/connect
	expiresAt time.Time // best-effort; zero if unknown (operator token path)
	tls       *tls.Config

	// Filled from the validate response for keyfile auth. Used to
	// populate the register frame when --register is on. Empty for
	// operator-token auth (operator never sends register).
	provider string
	model    string

	// keyfile + keyfileClient are non-nil ONLY for keyfile auth.
	// Cached on authInfo so the TokenProvider closure (NEX-237) can
	// re-validate against the broker when the cached JWT expires,
	// without round-tripping back through resolveAuth. Nil for
	// operator-token auth — there's no keyfile to re-validate against,
	// the operator must rotate the token externally.
	keyfile       *keyfile.Keyfile
	keyfileClient *keyfile.Client
}

// jwtCache holds the current JWT + expiry, mutex-protected so the
// TokenProvider callback (called from the wsclient reconnect loop)
// can read concurrently with the refresh path that writes.
//
// Inlined here rather than reusing agentfunnel's sessionState because
// nexus-comms-mcp is a transport-only MCP — it doesn't have the
// session-refresh frame plumbing agentfunnel needs, so the bigger
// state machine would be overkill.
type jwtCache struct {
	mu      sync.Mutex
	jwt     string
	expires time.Time
}

func (c *jwtCache) Get() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jwt, c.expires
}

func (c *jwtCache) Set(jwt string, expires time.Time) {
	c.mu.Lock()
	c.jwt = jwt
	c.expires = expires
	c.mu.Unlock()
}

// resolveAuth normalises the two auth modes into a single authInfo.
// Exactly one of --keyfile or --operator-token{,-file} must be set.
func resolveAuth(keyfilePath, opToken, opTokenFile, urlOverride string, insecure bool, log *slog.Logger) (*authInfo, error) {
	haveKeyfile := keyfilePath != ""
	haveOp := opToken != "" || opTokenFile != ""
	if !haveKeyfile && !haveOp {
		return nil, errors.New("must supply --keyfile or --operator-token{,-file}")
	}
	if haveKeyfile && haveOp {
		log.Warn("both --keyfile and --operator-token given; using --keyfile")
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // user-opt-in

	if haveKeyfile {
		return resolveKeyfileAuth(keyfilePath, urlOverride, tlsCfg, log)
	}
	return resolveOperatorAuth(opToken, opTokenFile, urlOverride, tlsCfg)
}

func resolveKeyfileAuth(path, urlOverride string, tlsCfg *tls.Config, log *slog.Logger) (*authInfo, error) {
	kf, err := keyfile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load keyfile: %w", err)
	}

	client := keyfile.NewClient()
	client.HTTP = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := client.Validate(ctx, kf)
	if err != nil {
		return nil, fmt.Errorf("validate keyfile against nexus: %w", err)
	}

	wsURL := urlOverride
	if wsURL == "" {
		wsURL = res.NexusURL
	}
	wsURL = toWSURL(wsURL)

	log.Info("keyfile validation succeeded",
		"aspect", res.AspectName,
		"nexus_id", res.NexusID,
		"provider", res.Provider,
		"model", res.Model)

	return &authInfo{
		aspect:    res.AspectName,
		jwt:       res.SessionJWT,
		wsURL:     wsURL,
		expiresAt: res.SessionExpiresAt,
		tls:       tlsCfg,
		provider:  res.Provider,
		model:     res.Model,
		// NEX-237: stash for the TokenProvider closure to re-validate
		// when the JWT expires. Operator-token auth leaves these nil
		// (no keyfile to refresh against — that's the operator's
		// responsibility out-of-band).
		keyfile:       kf,
		keyfileClient: client,
	}, nil
}

func resolveOperatorAuth(opToken, opTokenFile, urlOverride string, tlsCfg *tls.Config) (*authInfo, error) {
	jwt := strings.TrimSpace(opToken)
	if jwt == "" && opTokenFile != "" {
		raw, err := os.ReadFile(opTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read operator token file: %w", err)
		}
		jwt = strings.TrimSpace(string(raw))
	}
	if jwt == "" {
		return nil, errors.New("operator token resolved empty")
	}
	if urlOverride == "" {
		return nil, errors.New("--nexus-url is required when using --operator-token (no keyfile envelope to source it from)")
	}
	return &authInfo{
		aspect: "operator",
		jwt:    jwt,
		wsURL:  toWSURL(urlOverride),
		tls:    tlsCfg,
	}, nil
}

// toWSURL normalises a URL into wss://host:port/connect form. Mirrors
// agentfunnel's defensive /connect append (cutover 2026-05-11) and adds
// scheme normalisation for keyfiles whose envelopes carry https://.
func toWSURL(in string) string {
	out := strings.TrimRight(in, "/")
	switch {
	case strings.HasPrefix(out, "https://"):
		out = "wss://" + strings.TrimPrefix(out, "https://")
	case strings.HasPrefix(out, "http://"):
		out = "ws://" + strings.TrimPrefix(out, "http://")
	}
	if !strings.HasSuffix(out, "/connect") && !strings.HasSuffix(out, "/connect/") {
		out += "/connect"
	}
	return out
}

func buildLogger(level, file string) (*slog.Logger, func(), error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, func() {}, fmt.Errorf("unknown log level %q", level)
	}

	out := os.Stderr
	closer := func() {}
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open log file: %w", err)
		}
		out = f
		closer = func() { _ = f.Close() }
	}
	h := slog.NewTextHandler(out, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), closer, nil
}

// -------------------------------------------------------------------
// wsBridge — handler-side helpers for translating MCP tool calls into
// WS frames and back. Holds the long-lived wsclient.Client + the
// identity to fill into From fields when callers omit them.
// -------------------------------------------------------------------

type wsBridge struct {
	ws           *wsclient.Client
	inbox        *inboxBuffer
	defaultFrom  string // JWT subject — aspect name or "operator"
	log          *slog.Logger
	writeTimeout time.Duration
}

// brokerUnavailableMsg is what every tool returns when the WS connection
// is mid-reconnect or otherwise not ready. We surface this as a normal
// (successful) MCP tool result rather than an MCP error so the client
// (claude-code) doesn't mark the entire MCP server as dead — the model
// reads the text, understands "broker's restarting, try again," and
// retries on its own cadence. Operator chose this shape over blocking-
// through-reconnect and over optimistic buffering (no silent loss).
const brokerUnavailableMsg = "nexus broker unavailable — connection is reconnecting. The MCP transport stays up; retry shortly."

// brokerDown returns true and the structured "unavailable" tool result
// when the WS isn't connected. Tool handlers call this first and bail
// before attempting any write, so a broker restart looks like a brief
// "retry shortly" instead of a transport-level error.
func (b *wsBridge) brokerDown() (*mcpgo.CallToolResult, bool) {
	if b.ws.Connected() {
		return nil, false
	}
	return mcpgo.NewToolResultText(brokerUnavailableMsg), true
}

// newMCPServer builds the stdio MCP server with all Part 2 tools
// registered. Server name/version are surfaced to MCP clients via the
// initialize handshake.
func newMCPServer(b *wsBridge, log *slog.Logger) *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer(
		"nexus-comms-mcp",
		"0.2.0",
		mcpserver.WithToolCapabilities(false), // tools don't list-change at runtime
	)

	srv.AddTool(
		mcpgo.NewTool("read_chat",
			mcpgo.WithDescription("Read chat messages that have been delivered to this identity since the last read (or since the buffer started filling). Returns messages oldest-first. Pass since_id to only get messages newer than a known msg id. Each line looks like: #N [from time reason] content. The N is the msg id usable in send_chat reply_to / read_chat_thread / react_to."),
			mcpgo.WithNumber("since_id",
				mcpgo.Description("Only return messages with id greater than this. Default 0 (drain everything buffered)."),
			),
			mcpgo.WithNumber("limit",
				mcpgo.Description("Max messages to return (default 50; the buffer holds up to --inbox-buffer)."),
			),
		),
		b.handleReadChat,
	)

	srv.AddTool(
		mcpgo.NewTool("send_chat",
			mcpgo.WithDescription("Send a message to the nexus chat. Use @agent to mention. To reply to a message, pass its id number as reply_to. From defaults to the bound aspect identity if omitted."),
			mcpgo.WithString("content",
				mcpgo.Required(),
				mcpgo.Description("Message text. Use @agent to mention."),
			),
			mcpgo.WithString("from",
				mcpgo.Description("Your agent identity. If omitted, defaults to the keyfile/JWT-bound identity. Passing a different value will be rejected by the broker."),
			),
			mcpgo.WithNumber("reply_to",
				mcpgo.Description("ID of the message to reply to — the #N number from read_chat output."),
			),
			mcpgo.WithString("topic",
				mcpgo.Description("Topic name for feature-scoped threads (e.g. 'blender-pipeline'). Set on first message; replies inherit automatically."),
			),
		),
		b.handleSendChat,
	)

	srv.AddTool(
		mcpgo.NewTool("read_chat_message",
			mcpgo.WithDescription("Fetch a single chat message by msg_id, plus any descendants in its thread subtree. Useful when read_chat surfaces a #N reference and you want the full text. Synchronous WS round-trip via chat.read — does not affect the inbox buffer."),
			mcpgo.WithNumber("msg_id",
				mcpgo.Required(),
				mcpgo.Description("Message id to fetch."),
			),
		),
		b.handleReadChatMessage,
	)

	srv.AddTool(
		mcpgo.NewTool("read_chat_thread",
			mcpgo.WithDescription("Read every message in a thread, oldest-first. Pass any msg_id within the thread (typically the root); the server walks descendants. Use since_id to only return messages newer than a known cursor. Synchronous WS round-trip via chat.read — does not affect the inbox buffer."),
			mcpgo.WithNumber("msg_id",
				mcpgo.Required(),
				mcpgo.Description("Any message id in the thread (typically the root)."),
			),
			mcpgo.WithNumber("since_id",
				mcpgo.Description("Only include messages with id > since_id. Default 0 (return everything in the thread, bounded server-side to 200)."),
			),
		),
		b.handleReadChatThread,
	)

	srv.AddTool(
		mcpgo.NewTool("react_to",
			mcpgo.WithDescription("Toggle a reaction (emoji) on a chat message. Call again with the same emoji to remove it. Use for lightweight acknowledgements (👍 seen, ✅ done, 👀 looking) instead of full replies. Fire-and-forget — broker doesn't synchronously confirm the toggle."),
			mcpgo.WithNumber("msg_id",
				mcpgo.Required(),
				mcpgo.Description("Message id to react to — the #N from read_chat output."),
			),
			mcpgo.WithString("emoji",
				mcpgo.Required(),
				mcpgo.Description("Emoji to toggle (e.g. 👍, ✅, 👀, ❤️)."),
			),
			mcpgo.WithString("from",
				mcpgo.Description("Your agent identity. If omitted, defaults to the keyfile/JWT-bound identity."),
			),
		),
		b.handleReactTo,
	)

	srv.AddTool(
		mcpgo.NewTool("search_knowledge",
			mcpgo.WithDescription("Search the nexus knowledge store via FTS5 keyword retrieval. Returns ranked hits with id, topic, content excerpt, and score. Scope defaults to your own entries + operator-curated 'shared' entries; pass explicit own_agent / shared / peers to refine. Synchronous WS round-trip via knowledge.search."),
			mcpgo.WithString("text",
				mcpgo.Required(),
				mcpgo.Description("Search query — FTS5 syntax. Plain words work; combine with AND/OR/NOT for boolean."),
			),
			mcpgo.WithBoolean("own_agent",
				mcpgo.Description("Include your own knowledge entries (default true when no scope specified)."),
			),
			mcpgo.WithBoolean("shared",
				mcpgo.Description("Include operator-curated 'shared' entries (default true when no scope specified)."),
			),
			mcpgo.WithNumber("top_k",
				mcpgo.Description("Max hits to return (default 5, max 50)."),
			),
		),
		b.handleSearchKnowledge,
	)

	srv.AddTool(
		mcpgo.NewTool("store_knowledge",
			mcpgo.WithDescription("Upsert a knowledge entry under (your-identity, topic). Re-using the same topic updates the row in place. Use for durable cross-session notes you want to retrieve via search_knowledge later. Pass shared=true to mark as operator-curated (visible to all aspects searching with shared scope)."),
			mcpgo.WithString("topic",
				mcpgo.Required(),
				mcpgo.Description("Topic slug — short identifier (e.g. 'cairn-spec' or 'morph-bug-i3'). Re-used to update existing entries."),
			),
			mcpgo.WithString("content",
				mcpgo.Required(),
				mcpgo.Description("Entry body. Markdown OK. Searched via FTS5."),
			),
			mcpgo.WithBoolean("shared",
				mcpgo.Description("Mark as operator-curated / visible to all aspects via shared scope. Default false (your entry only)."),
			),
		),
		b.handleStoreKnowledge,
	)

	// spawn (NEX-601): only materialised for a parent aspect identity.
	// Derived hands (no-sub-of-sub) and the operator transport don't get
	// it — the broker would reject them anyway, this keeps the surface
	// honest. See spawnToolAvailable / spawn.go.
	if spawnToolAvailable(b.defaultFrom) {
		srv.AddTool(spawnTool(), b.handleSpawn)
		log.Info("spawn tool materialised", "aspect", b.defaultFrom)
	} else {
		log.Debug("spawn tool withheld (derived/operator identity)", "identity", b.defaultFrom)
	}

	return srv
}

// handleSendChat translates an MCP send_chat tool call into a chat.send
// WS frame. Fire-and-forget per the chat substrate's transport spec:
// the broker doesn't carry the new msg_id back synchronously, so we
// return "ok" once the write lands on the wire.
func (b *wsBridge) handleSendChat(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	content := strings.TrimSpace(req.GetString("content", ""))
	if content == "" {
		return mcpgo.NewToolResultError("content is required and must be non-empty"), nil
	}

	// From: prefer the explicit arg; fall back to the bound identity.
	// We DON'T reject mismatches — let the broker enforce. Defence in
	// depth at one layer is enough; rejecting client-side would just
	// duplicate broker logic and create a drift surface.
	from := strings.TrimSpace(req.GetString("from", ""))
	if from == "" {
		from = b.defaultFrom
	}

	replyTo := req.GetInt("reply_to", 0)
	topic := strings.TrimSpace(req.GetString("topic", ""))

	if r, down := b.brokerDown(); down {
		return r, nil
	}

	payload := frames.ChatSendPayload{
		From:    from,
		Content: content,
		ReplyTo: replyTo,
		Topic:   topic,
	}

	// chat.send is a notification (no correlated response), so
	// frames.New (no ID) is correct here — not NewRequest.
	env, err := frames.New(frames.KindChatSend, payload)
	if err != nil {
		b.log.Warn("send_chat: encode failed", "err", err)
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, b.writeTimeout)
	defer cancel()
	if err := b.ws.Send(sendCtx, env); err != nil {
		b.log.Warn("send_chat: WS send failed", "err", err, "from", from)
		return mcpgo.NewToolResultError(fmt.Sprintf("WS send: %v", err)), nil
	}

	b.log.Debug("send_chat: posted", "from", from, "reply_to", replyTo, "topic", topic, "len", len(content))
	return mcpgo.NewToolResultText("ok"), nil
}

// handleReadChat drains the inbox of delivered chat.deliver frames.
// Returns oldest-first, formatted one per line so MCP clients can
// trivially scan for ids. Format matches the old comms-mcp surface
// closely enough that existing aspect prompts still parse:
//
//	#123 [from=anvil reason=mention 2026-05-11T20:30:00Z] message text
//
// If the buffer dropped any messages since the last read, a leading
// "[N messages dropped, buffer overflow]" note is prepended so the
// caller knows it fell behind.
func (b *wsBridge) handleReadChat(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sinceID := int64(req.GetInt("since_id", 0))
	limit := req.GetInt("limit", 50)
	if limit <= 0 {
		limit = 50
	}

	items, dropped := b.inbox.drainAfter(sinceID)
	if len(items) > limit {
		// Drain returned everything past since_id; cap to limit but
		// put the rest back so a follow-up call can fetch them. Cheap
		// for v1 where limit ≈ buffer cap; if this gets hot, swap for
		// a proper cursor-based read.
		extra := items[limit:]
		items = items[:limit]
		for _, e := range extra {
			b.inbox.add(e)
		}
	}

	if len(items) == 0 {
		msg := "no new messages"
		if dropped > 0 {
			msg = fmt.Sprintf("[%d messages dropped, buffer overflow]\nno new messages", dropped)
		}
		return mcpgo.NewToolResultText(msg), nil
	}

	var sb strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&sb, "[%d messages dropped, buffer overflow]\n", dropped)
	}
	for _, m := range items {
		reason := m.Reason
		if reason == "" {
			reason = "delivered"
		}
		replyHint := ""
		if m.ReplyTo > 0 {
			replyHint = fmt.Sprintf(" reply_to=%d", m.ReplyTo)
		}
		replayHint := ""
		if m.Replay {
			replayHint = " replay"
		}
		fmt.Fprintf(&sb, "#%d [from=%s reason=%s%s%s %s] %s\n",
			m.ID, m.From, reason, replyHint, replayHint, m.ReceivedAt, m.Content)
	}
	return mcpgo.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

// handleReadChatMessage fetches one message + its descendants via the
// chat.read pull path. Aspect-available (Lock 2) — does not require
// roster registration. Synchronous Request/Response over WS.
//
// The broker's chat.read implementation walks descendants from the
// given node, so leaves return just themselves and roots return the
// whole subtree. The caller-facing distinction (single message vs
// whole thread) is purely cosmetic.
func (b *wsBridge) handleReadChatMessage(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	msgID := req.GetInt("msg_id", 0)
	if msgID <= 0 {
		return mcpgo.NewToolResultError("msg_id is required and must be positive"), nil
	}
	return b.chatRead(ctx, msgID, 0)
}

// handleReadChatThread is read_chat_message with explicit thread-walk
// semantics + a since_id cursor for pagination.
func (b *wsBridge) handleReadChatThread(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	msgID := req.GetInt("msg_id", 0)
	if msgID <= 0 {
		return mcpgo.NewToolResultError("msg_id is required and must be positive"), nil
	}
	sinceID := int64(req.GetInt("since_id", 0))
	return b.chatRead(ctx, msgID, sinceID)
}

// chatRead issues a chat.read Request, awaits the correlated result,
// and formats it per the read_chat line convention. Shared by the
// single-message and thread-walk tools — they differ only in what the
// model is being told they do.
func (b *wsBridge) chatRead(ctx context.Context, msgID int, sinceID int64) (*mcpgo.CallToolResult, error) {
	if r, down := b.brokerDown(); down {
		return r, nil
	}
	payload := frames.ChatReadPayload{
		MsgID:   msgID,
		SinceID: sinceID,
	}
	env, err := frames.NewRequest(frames.KindChatRead, payload)
	if err != nil {
		b.log.Warn("chat.read: encode failed", "err", err)
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	// 10s timeout: a chat.read against a 200-row thread should be sub-
	// second; 10s leaves slack for a slow broker without blocking the
	// MCP client indefinitely.
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := b.ws.Request(reqCtx, env)
	if err != nil {
		b.log.Warn("chat.read: request failed", "err", err, "msg_id", msgID)
		return mcpgo.NewToolResultError(fmt.Sprintf("chat.read: %v", err)), nil
	}

	var result frames.ChatReadResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("decode result: %v", err)), nil
	}

	if len(result.Messages) == 0 {
		return mcpgo.NewToolResultText("(no messages)"), nil
	}

	var sb strings.Builder
	for _, m := range result.Messages {
		replyHint := ""
		if m.ReplyTo > 0 {
			replyHint = fmt.Sprintf(" reply_to=%d", m.ReplyTo)
		}
		fmt.Fprintf(&sb, "#%d [from=%s%s %s] %s\n",
			m.ID, m.From, replyHint, m.ReceivedAt, m.Content)
	}
	return mcpgo.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

// handleReactTo translates an MCP react_to tool call into a
// chat.reaction WS frame. Fire-and-forget per the chat substrate
// (reactions toggle on the broker; no synchronous confirmation).
func (b *wsBridge) handleReactTo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	msgID := req.GetInt("msg_id", 0)
	if msgID <= 0 {
		return mcpgo.NewToolResultError("msg_id is required and must be positive"), nil
	}
	emoji := strings.TrimSpace(req.GetString("emoji", ""))
	if emoji == "" {
		return mcpgo.NewToolResultError("emoji is required and must be non-empty"), nil
	}
	from := strings.TrimSpace(req.GetString("from", ""))
	if from == "" {
		from = b.defaultFrom
	}

	if r, down := b.brokerDown(); down {
		return r, nil
	}

	env, err := frames.New(frames.KindChatReaction, frames.ChatReactionPayload{
		From:  from,
		MsgID: msgID,
		Emoji: emoji,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, b.writeTimeout)
	defer cancel()
	if err := b.ws.Send(sendCtx, env); err != nil {
		b.log.Warn("react_to: WS send failed", "err", err, "from", from)
		return mcpgo.NewToolResultError(fmt.Sprintf("WS send: %v", err)), nil
	}

	b.log.Debug("react_to: posted", "from", from, "msg_id", msgID, "emoji", emoji)
	return mcpgo.NewToolResultText("ok"), nil
}

// handleSearchKnowledge issues a knowledge.search Request over WS and
// formats the result for the model. Aspect-side path; broker scopes
// the search to the bound aspect identity. Operator-curated entries
// surface when shared=true.
func (b *wsBridge) handleSearchKnowledge(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	text := strings.TrimSpace(req.GetString("text", ""))
	if text == "" {
		return mcpgo.NewToolResultError("text is required and must be non-empty"), nil
	}
	ownAgent := req.GetBool("own_agent", false)
	shared := req.GetBool("shared", false)
	topK := req.GetInt("top_k", 5)
	if topK <= 0 {
		topK = 5
	}

	if r, down := b.brokerDown(); down {
		return r, nil
	}
	payload := frames.KnowledgeSearchPayload{
		Text:     text,
		OwnAgent: ownAgent,
		Shared:   shared,
		TopK:     topK,
	}
	env, err := frames.NewRequest(frames.KindKnowledgeSearch, payload)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := b.ws.Request(reqCtx, env)
	if err != nil {
		b.log.Warn("search_knowledge: request failed", "err", err, "text", text)
		return mcpgo.NewToolResultError(fmt.Sprintf("knowledge.search: %v", err)), nil
	}

	// knowledge.search.error returns {"error": "..."} — surface to the
	// model as a tool error so it can adjust the query or scope.
	if string(resp.Kind) == string(frames.KindKnowledgeSearch)+".error" {
		var errPayload map[string]string
		_ = json.Unmarshal(resp.Payload, &errPayload)
		return mcpgo.NewToolResultError(fmt.Sprintf("knowledge.search: %s", errPayload["error"])), nil
	}

	var result frames.KnowledgeSearchResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("decode result: %v", err)), nil
	}
	if len(result.Hits) == 0 {
		return mcpgo.NewToolResultText("(no matches)"), nil
	}
	var sb strings.Builder
	for _, h := range result.Hits {
		fmt.Fprintf(&sb, "#%d [from=%s topic=%q score=%.3f%s]\n%s\n\n",
			h.ID, h.FromAgent, h.Topic, h.Score,
			func() string {
				if h.Shared {
					return " shared"
				}
				return ""
			}(),
			h.Content)
	}
	return mcpgo.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

// handleStoreKnowledge issues a knowledge.store Request over WS. The
// broker stamps from_agent with the bound aspect identity; aspects
// can't impersonate via this path.
func (b *wsBridge) handleStoreKnowledge(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	topic := strings.TrimSpace(req.GetString("topic", ""))
	if topic == "" {
		return mcpgo.NewToolResultError("topic is required and must be non-empty"), nil
	}
	content := req.GetString("content", "")
	if content == "" {
		return mcpgo.NewToolResultError("content is required and must be non-empty"), nil
	}
	shared := req.GetBool("shared", false)

	if r, down := b.brokerDown(); down {
		return r, nil
	}
	payload := frames.KnowledgeStorePayload{
		Topic:   topic,
		Content: content,
		Shared:  shared,
	}
	env, err := frames.NewRequest(frames.KindKnowledgeStore, payload)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := b.ws.Request(reqCtx, env)
	if err != nil {
		b.log.Warn("store_knowledge: request failed", "err", err, "topic", topic)
		return mcpgo.NewToolResultError(fmt.Sprintf("knowledge.store: %v", err)), nil
	}

	if string(resp.Kind) == string(frames.KindKnowledgeStore)+".error" {
		var errPayload map[string]string
		_ = json.Unmarshal(resp.Payload, &errPayload)
		return mcpgo.NewToolResultError(fmt.Sprintf("knowledge.store: %s", errPayload["error"])), nil
	}

	var result frames.KnowledgeStoreResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("decode result: %v", err)), nil
	}
	b.log.Debug("store_knowledge: ok", "topic", topic, "id", result.ID, "shared", shared)
	return mcpgo.NewToolResultText(fmt.Sprintf("ok id=%d", result.ID)), nil
}

// sendRegister emits the post-connect register frame so the broker
// puts this identity onto the subscribe.chat fan-out + (if since_msg_id
// is non-zero) replays addressed messages we missed while disconnected.
//
// Best-effort: failures are logged by the caller. The next reconnect
// will retry the register. Errors here don't tear down the WS — the
// transport might still be useful for send-only flows.
func sendRegister(ctx context.Context, ws *wsclient.Client, auth *authInfo, sessionID string, sinceMsgID int64, log *slog.Logger) error {
	provider := auth.provider
	if provider == "" {
		// Operator path or no validate response — register the
		// transport identity descriptively so the dashboard can show
		// it without inventing a fake provider.
		provider = "nexus-comms-mcp"
	}

	payload := frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        auth.aspect,
			ContextMode: schemas.ContextThread, // arbitrary; we don't run a context loop
			Provider:    provider,
			PID:         os.Getpid(),
			StartedAt:   time.Now().UTC(),
			Model:       auth.model,
			SessionID:   sessionID,
		},
		SinceMsgID: sinceMsgID,
	}

	env, err := frames.New(frames.KindRegister, payload)
	if err != nil {
		return fmt.Errorf("encode register: %w", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Send(sendCtx, env); err != nil {
		return fmt.Errorf("send register: %w", err)
	}
	log.Info("register frame sent", "aspect", auth.aspect, "since_msg_id", sinceMsgID, "session", sessionID)
	return nil
}
