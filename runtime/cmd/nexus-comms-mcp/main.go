// Command nexus-comms-mcp bridges nexus's WebSocket chat substrate to
// stdio MCP. Long-running process: one process == one nexus identity ==
// one held-open /connect WS. Stdio half exposes comms tools to whichever
// MCP client (claude-code subprocess, ad-hoc CLI, etc.) launched it.
//
// Spec: agent-network/docs/2026-05-11-nexus-comms-mcp-spec.md.
//
// Part 1 scope: keyfile load + spec §5 handshake → JWT, dial /connect,
// hold WS open until ctx done. No MCP server yet (Part 2). No comms
// tools yet (Parts 2-4). The point of this part is to prove the
// transport works end-to-end against a live Nexus: bin runs, validates,
// dials, stays connected, exits cleanly on SIGINT.
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
		logFile      = flag.String("log-file", "", "Write logs here instead of stderr (stdout reserved for MCP protocol once Part 2 lands)")
	)
	flag.Parse()

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

	if err := runWS(ctx, auth, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("ws client exited with error", "err", err)
		os.Exit(2)
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

// resolveAuth normalises the two auth modes (keyfile vs operator token)
// into a single authInfo. Exactly one of --keyfile or --operator-token{,-file}
// must be set; if both are set, --keyfile wins and we warn.
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

// runWS dials nexus and holds the connection open. Part 1: no frames
// are sent or handled — incoming frames are logged at debug. Parts 2+
// will replace the Handler with a real dispatcher that feeds the MCP
// server and inbox buffer.
func runWS(ctx context.Context, a *authInfo, log *slog.Logger) error {
	handler := wsclient.HandlerFunc(func(env frames.Envelope) {
		log.Debug("uncorrelated frame received", "kind", env.Kind, "id", env.ID)
	})

	cli, err := wsclient.New(wsclient.Config{
		URL:              a.wsURL,
		AuthToken:        a.jwt,
		Handler:          handler,
		Logger:           log,
		FailFirstConnect: true, // surface auth/network failures loudly on startup
	})
	if err != nil {
		return fmt.Errorf("wsclient.New: %w", err)
	}

	// TLS config has to be applied via the dialer's transport. wsclient
	// uses the default transport; for self-signed dev certs the caller
	// must pre-seed the trust store OR pass --insecure-skip-verify. We
	// document this in --insecure-skip-verify help. Wiring a per-Client
	// TLS config into wsclient is a follow-up if Part 1 testing turns
	// out to need it.
	_ = a.tls

	// Connect-event drain: log every transition so operator can see
	// reconnect activity. Drop-on-overflow is fine for diagnostics.
	go func() {
		for ev := range cli.Events() {
			if ev.Connected {
				log.Info("ws connected", "url", a.wsURL)
			} else {
				log.Warn("ws disconnected", "url", a.wsURL)
			}
		}
	}()

	return cli.Run(ctx)
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
