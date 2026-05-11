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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
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
		noMCP        = flag.Bool("no-mcp", false, "Skip starting the stdio MCP server — Part 1 transport-only mode for diagnostics")
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

	auth, err := resolveAuth(*keyfilePath, *opToken, *opTokenFile, *nexusURL, *insecureSkip, log)
	if err != nil {
		log.Error("auth setup failed", "err", err)
		os.Exit(2)
	}

	log.Info("nexus-comms-mcp starting",
		"aspect", auth.aspect,
		"nexus_url", auth.wsURL,
		"jwt_expires_at", auth.expiresAt.Format(time.RFC3339))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build the WS client. The Handler is a no-op for Part 2 (uncorrelated
	// inbox frames are logged at debug). Part 3 swaps in the inbox buffer.
	wsHandler := wsclient.HandlerFunc(func(env frames.Envelope) {
		log.Debug("uncorrelated frame received", "kind", env.Kind, "id", env.ID)
	})

	wsCli, err := wsclient.New(wsclient.Config{
		URL:              auth.wsURL,
		AuthToken:        auth.jwt,
		Handler:          wsHandler,
		Logger:           log,
		FailFirstConnect: true,
	})
	if err != nil {
		log.Error("wsclient.New", "err", err)
		os.Exit(2)
	}

	// Connect-event drain: surfaces connect/disconnect transitions.
	go func() {
		for ev := range wsCli.Events() {
			if ev.Connected {
				log.Info("ws connected", "url", auth.wsURL)
			} else {
				log.Warn("ws disconnected", "url", auth.wsURL)
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
	defaultFrom  string // JWT subject — aspect name or "operator"
	log          *slog.Logger
	writeTimeout time.Duration
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
