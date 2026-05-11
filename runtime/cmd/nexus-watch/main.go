// Command nexus-watch is a terminal observer for one aspect's
// observability stream. It subscribes via the Phase B subscribe.observe
// frame and renders ChatFrame events to a TTY in real time, with slash
// commands to switch aspect focus, post messages back, and review
// recent history.
//
// Spec: docs/2026-05-12-nexus-watch-and-observability-core.md §4.2
// (terminal renderer) and §9 Phase C. v0.1 scope: chat traffic only.
// Bridle events (rich turn detail, tool calls, artifacts) are Phase E.
//
// Auth model mirrors nexus-comms-mcp's operator path: operator JWT via
// --operator-token / --operator-token-file, no register frame. Stdout
// is reserved for rendered output; logs go to stderr or --log-file.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-watch: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File, stdin *os.File) error {
	fs := flag.NewFlagSet("nexus-watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: nexus-watch <aspect> [flags]

Subscribe to one aspect's observability stream and render chat traffic
to the terminal in real time. Type a message to send chat addressed to
the aspect under focus; slash commands switch aspect, recall history,
and exit cleanly.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Slash commands:
  /switch <aspect>     change observation focus
  /say <message>       send chat (same as typing without leading slash)
  /new                 forget last-seen msg id (next send is top-level)
  /history [N]         print last N frames from the in-memory buffer
  /help                show this list
  /quit                graceful exit

Auth: obtaining an operator JWT is currently the operator's problem;
mint via the dashboard passkey flow and supply via --operator-token
or --operator-token-file.
`)
	}

	var (
		nexusURL     = fs.String("nexus-url", "", "WS URL of the nexus broker (e.g. wss://host:port/connect). Required.")
		opToken      = fs.String("operator-token", "", "Operator JWT (mutually exclusive with --operator-token-file)")
		opTokenFile  = fs.String("operator-token-file", "", "Read operator JWT from this file (alternative to --operator-token)")
		insecureSkip = fs.Bool("insecure-skip-verify", false, "Skip TLS cert verification (dev/self-signed only — do NOT use in production)")
		logFile      = fs.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the rendered observation feed")
		logLevel     = fs.String("log-level", "info", "slog level: debug|info|warn|error")
		historyN     = fs.Int("history", 50, "How many frames /history shows by default; also the per-aspect recent-buffer cap")
		noColor      = fs.Bool("no-color", false, "Disable ANSI color (also honoured: NO_COLOR env)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return errors.New("missing required <aspect> positional argument")
	}
	initialAspect := fs.Arg(0)

	log, closeLog, err := buildLogger(*logLevel, *logFile, stderr)
	if err != nil {
		return fmt.Errorf("logger setup: %w", err)
	}
	defer closeLog()

	auth, err := resolveOperatorAuth(*opToken, *opTokenFile, *nexusURL, *insecureSkip)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	log.Info("nexus-watch starting", "aspect", initialAspect, "nexus_url", auth.wsURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	useColor := !*noColor && os.Getenv("NO_COLOR") == ""
	rd := newRenderer(stdout, useColor, *historyN)

	// Frame channel: reader goroutine pushes decoded observability
	// frames; renderer goroutine consumes. Buffered so a slow render
	// doesn't backpressure the WS read loop.
	frameCh := make(chan observability.Frame, 64)

	// Aspect-focus state is shared between input loop, reader, and
	// renderer. Guard with mutex; reads are cheap and writes rare.
	state := &watchState{currentAspect: initialAspect}

	wsHandler := wsclient.HandlerFunc(func(env frames.Envelope) {
		if env.Kind != frames.KindObserveFrame {
			log.Debug("uncorrelated frame ignored", "kind", env.Kind)
			return
		}
		var p frames.ObserveFramePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			log.Warn("observe.frame payload decode failed", "err", err)
			return
		}
		var f observability.Frame
		if err := json.Unmarshal(p.Frame, &f); err != nil {
			log.Warn("observe.frame inner decode failed", "err", err)
			return
		}
		// Trust the envelope-level Aspect if Frame.Aspect is empty
		// (defensive — broker fills it but a future codepath might not).
		if f.Aspect == "" {
			f.Aspect = p.Aspect
		}
		select {
		case frameCh <- f:
		case <-ctx.Done():
		}
	})

	wsCli, err := wsclient.New(wsclient.Config{
		URL:              auth.wsURL,
		AuthToken:        auth.jwt,
		Handler:          wsHandler,
		Logger:           log,
		FailFirstConnect: true,
	})
	if err != nil {
		return fmt.Errorf("wsclient.New: %w", err)
	}
	_ = auth.tls // reserved: wsclient builds its own TLS from URL scheme; insecureSkip wired via env-var path if needed later

	// Connect-event handler: subscribe to current aspect on every
	// fresh connect, render banner/status. Reconnect re-subscribes
	// automatically because the broker doesn't retain operator subs
	// across the dropped WS.
	go func() {
		for ev := range wsCli.Events() {
			if !ev.Connected {
				rd.statusDisconnected()
				continue
			}
			rd.statusReconnected() // first connect: same banner shape, fine
			aspect := state.aspect()
			if err := sendSubscribeObserve(ctx, wsCli, aspect, 0); err != nil {
				log.Warn("subscribe.observe failed", "err", err, "aspect", aspect)
				continue
			}
			rd.banner(aspect)
		}
	}()

	wsErrCh := make(chan error, 1)
	go func() { wsErrCh <- wsCli.Run(ctx) }()

	// Renderer goroutine: serialises stdout writes and owns the
	// per-aspect lastSeen + recent buffer state.
	rendererDone := make(chan struct{})
	go func() {
		defer close(rendererDone)
		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-frameCh:
				if !ok {
					return
				}
				rd.handleFrame(f, state.aspect())
				if f.Kind == observability.FrameChat {
					var cf observability.ChatFrame
					if err := json.Unmarshal(f.Payload, &cf); err == nil {
						state.observeChat(f.Aspect, cf)
					}
				}
			}
		}
	}()

	// Input loop on stdin runs on the main goroutine. Exits when
	// stdin closes, /quit, or ctx cancels.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := strings.TrimRight(scanner.Text(), "\r\n")
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "/") {
				if quit := handleSlash(ctx, line, wsCli, rd, state, log); quit {
					stop()
					return
				}
				continue
			}
			// Plain text → send as @aspect chat.
			if err := sendChat(ctx, wsCli, state, line, log); err != nil {
				log.Warn("send chat failed", "err", err)
			}
		}
		if err := scanner.Err(); err != nil {
			log.Warn("stdin scan error", "err", err)
		}
		// stdin closed → graceful exit
		stop()
	}()

	// Wait for any of: ctx done, ws exit, input loop exit.
	select {
	case <-ctx.Done():
	case err := <-wsErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("ws client exited with error", "err", err)
		}
	case <-inputDone:
	}

	// Best-effort unsubscribe before tearing down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sendUnsubscribeObserve(shutdownCtx, wsCli, state.aspect())

	stop()
	<-wsErrCh
	return nil
}

// watchState carries the small bit of mutable state shared between the
// renderer goroutine and the input loop: which aspect is currently in
// focus, and the per-aspect last-seen msg id (used to thread the next
// outbound message as a reply to the most recent observed chat).
type watchState struct {
	mu            sync.Mutex
	currentAspect string
	lastSeenMsgID map[string]int64
}

func (s *watchState) aspect() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentAspect
}

func (s *watchState) setAspect(a string) {
	s.mu.Lock()
	s.currentAspect = a
	s.mu.Unlock()
}

func (s *watchState) observeChat(aspect string, cf observability.ChatFrame) {
	// Track most recent msg id we saw for this aspect, so /say replies
	// to the conversation tail. Only inbound counts as a "reply target"
	// — replying to your own message would thread to yourself.
	if cf.Direction != observability.DirectionInbound {
		return
	}
	s.mu.Lock()
	if s.lastSeenMsgID == nil {
		s.lastSeenMsgID = make(map[string]int64)
	}
	if cf.MsgID > s.lastSeenMsgID[aspect] {
		s.lastSeenMsgID[aspect] = cf.MsgID
	}
	s.mu.Unlock()
}

func (s *watchState) lastSeen(aspect string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeenMsgID[aspect]
}

func (s *watchState) clearLastSeen(aspect string) {
	s.mu.Lock()
	if s.lastSeenMsgID != nil {
		delete(s.lastSeenMsgID, aspect)
	}
	s.mu.Unlock()
}

// handleSlash dispatches a /command line. Returns true if the loop
// should terminate (/quit).
func handleSlash(ctx context.Context, line string, ws *wsclient.Client, rd *renderer, state *watchState, log *slog.Logger) bool {
	cmd, rest := splitCommand(line)
	switch cmd {
	case "/quit", "/exit":
		return true
	case "/help", "/?":
		rd.help()
		return false
	case "/switch":
		newAspect := strings.TrimSpace(rest)
		if newAspect == "" {
			rd.systemf("usage: /switch <aspect>")
			return false
		}
		old := state.aspect()
		if newAspect == old {
			rd.systemf("already observing @%s", newAspect)
			return false
		}
		_ = sendUnsubscribeObserve(ctx, ws, old)
		if err := sendSubscribeObserve(ctx, ws, newAspect, 0); err != nil {
			rd.systemf("subscribe failed: %v", err)
			log.Warn("subscribe.observe failed", "err", err, "aspect", newAspect)
			return false
		}
		state.setAspect(newAspect)
		rd.banner(newAspect)
		return false
	case "/say":
		body := strings.TrimSpace(rest)
		if body == "" {
			rd.systemf("usage: /say <message>")
			return false
		}
		if err := sendChat(ctx, ws, state, body, log); err != nil {
			rd.systemf("send failed: %v", err)
		}
		return false
	case "/new":
		a := state.aspect()
		state.clearLastSeen(a)
		rd.systemf("cleared last-seen msg id for @%s — next send is top-level", a)
		return false
	case "/history":
		n := 0
		if rest != "" {
			if v, err := parsePosInt(strings.TrimSpace(rest)); err == nil {
				n = v
			} else {
				rd.systemf("usage: /history [N]")
				return false
			}
		}
		rd.history(state.aspect(), n)
		return false
	default:
		rd.systemf("unknown command %q, try /help", cmd)
		return false
	}
}

func splitCommand(line string) (cmd, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

func parsePosInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a positive integer")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// sendChat fires a chat.send addressed to the current aspect. The
// content is auto-prefixed @<aspect> if the operator didn't include
// one; reply_to is set to the most recently observed inbound msg id
// (so the default flow threads with the conversation tail).
func sendChat(ctx context.Context, ws *wsclient.Client, state *watchState, body string, log *slog.Logger) error {
	aspect := state.aspect()
	mention := "@" + aspect
	content := body
	if !strings.Contains(body, mention) {
		content = mention + " " + body
	}
	payload := frames.ChatSendPayload{
		From:    "operator",
		Content: content,
		ReplyTo: int(state.lastSeen(aspect)),
	}
	env, err := frames.New(frames.KindChatSend, payload)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Send(sendCtx, env); err != nil {
		return fmt.Errorf("ws send: %w", err)
	}
	log.Debug("chat.send posted", "aspect", aspect, "reply_to", payload.ReplyTo, "len", len(content))
	return nil
}

func sendSubscribeObserve(ctx context.Context, ws *wsclient.Client, aspect string, sinceSeq int64) error {
	env, err := frames.New(frames.KindSubscribeObserve, frames.SubscribeObservePayload{
		Aspect:   aspect,
		SinceSeq: sinceSeq,
	})
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ws.Send(sendCtx, env)
}

func sendUnsubscribeObserve(ctx context.Context, ws *wsclient.Client, aspect string) error {
	if aspect == "" {
		return nil
	}
	env, err := frames.New(frames.KindUnsubscribeObserve, frames.UnsubscribeObservePayload{Aspect: aspect})
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return ws.Send(sendCtx, env)
}

// -------------------------------------------------------------------
// Auth + setup helpers — mirrors nexus-comms-mcp/main.go's operator
// path. Deliberately not extracted into a shared package yet (one
// shared point is fine; a third client would justify runtime/operatorws/).
// -------------------------------------------------------------------

type operatorAuth struct {
	jwt   string
	wsURL string
	tls   *tls.Config
}

func resolveOperatorAuth(opToken, opTokenFile, urlOverride string, insecure bool) (*operatorAuth, error) {
	if opToken != "" && opTokenFile != "" {
		return nil, errors.New("--operator-token and --operator-token-file are mutually exclusive")
	}
	jwt := strings.TrimSpace(opToken)
	if jwt == "" && opTokenFile != "" {
		raw, err := os.ReadFile(opTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read operator token file: %w", err)
		}
		jwt = strings.TrimSpace(string(raw))
	}
	if jwt == "" {
		return nil, errors.New("must supply --operator-token or --operator-token-file")
	}
	if urlOverride == "" {
		return nil, errors.New("--nexus-url is required")
	}
	return &operatorAuth{
		jwt:   jwt,
		wsURL: toWSURL(urlOverride),
		tls:   &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // user-opt-in
	}, nil
}

// toWSURL mirrors nexus-comms-mcp's normalisation: rewrite scheme,
// ensure /connect suffix.
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

func buildLogger(level, file string, defaultOut *os.File) (*slog.Logger, func(), error) {
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

	out := defaultOut
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
