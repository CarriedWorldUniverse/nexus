// Command nexus-imap-mcp bridges an aspect's IMAP mailbox to stdio
// MCP. Credentials are fetched from the Nexus broker via the
// credential.fetch WS frame (NEX-77); the keyfile is still required
// because it provides the JWT auth path and the optional
// .imap.default_folder for tools that don't pass an explicit folder,
// but mailbox secrets (host, port, username, password) no longer live
// on the remote host's disk.
//
// Tools exposed:
//
//   imap.list_folders   — enumerate mailboxes the user can see
//   imap.fetch_recent   — recent messages from a folder, filterable
//   imap.fetch_otp      — pull a 6-digit code from a recent message
//   imap.move           — move a UID to another folder (auto-creates)
//   imap.delete         — expunge a UID
//   imap.mark           — set/clear IMAP flags (Seen, Flagged, ...)
//
// Today only shadow uses this — to drive Atlassian first-login OTP
// flows on behalf of other aspects — but every aspect's keyfile can
// resolve its own broker-side IMAP credential.

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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/brokercreds"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
)

func main() {
	var (
		keyfilePath    = flag.String("keyfile", "", "Path to the aspect keyfile JSON. Required: provides JWT auth to Nexus + optional .imap block for non-secret config (default_folder).")
		credentialName = flag.String("credential-name", "", "Specific imap credential name to fetch from the broker. Empty (default) → broker resolves the aspect's default imap credential.")
		nexusURLFlag   = flag.String("nexus-url", "", "Override the WS URL (default: derived from keyfile envelope, with scheme rewritten and /connect appended).")
		insecureSkip   = flag.Bool("insecure-skip-verify", false, "Skip TLS cert verification for the validate handshake (dev/self-signed only).")
		logLevel       = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile        = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the MCP protocol stream.")
		probe          = flag.Bool("probe", false, "Don't start MCP — connect, login, SELECT INBOX, and exit. Useful for credential smoke tests.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-imap-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	if *keyfilePath == "" {
		log.Error("missing -keyfile")
		os.Exit(2)
	}
	kf, err := loadKeyfile(*keyfilePath)
	if err != nil {
		log.Error("keyfile load failed", "err", err, "path", *keyfilePath)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// NEX-130: prefer keyfile-side IMAP credentials when present.
	// Keyfile is the operator-controlled source of truth; broker fetch
	// is the legacy / shared-credential path.
	var (
		host, user, password string
		port                 int
		credName             string
		wsCli                *wsclient.Client
		wsErrCh              chan error
		brokerFetched        bool
	)
	if kf.IMAP != nil && kf.IMAP.Host != "" && kf.IMAP.Username != "" && kf.IMAP.Password != "" {
		// Fast path: secrets in keyfile. Skip broker entirely.
		host = kf.IMAP.Host
		port = kf.IMAP.Port
		user = kf.IMAP.Username
		password = kf.IMAP.Password
		credName = "keyfile:imap"
		log.Info("imap creds from keyfile (no broker fetch)", "host", host, "user", user)
	} else {
		brokerFetched = true
		auth, err := validateKeyfile(ctx, kf, *nexusURLFlag, *insecureSkip, log)
		if err != nil {
			log.Error("keyfile validate failed", "err", err)
			os.Exit(2)
		}
		wsCli, err = wsclient.New(wsclient.Config{
			URL:              auth.wsURL,
			AuthToken:        auth.jwt,
			Handler:          wsclient.HandlerFunc(func(frames.Envelope) {}),
			Logger:           log,
			FailFirstConnect: true,
		})
		if err != nil {
			log.Error("wsclient.New", "err", err)
			os.Exit(2)
		}
		wsErrCh = make(chan error, 1)
		go func() { wsErrCh <- wsCli.Run(ctx) }()
		if err := waitConnected(ctx, wsCli, 10*time.Second); err != nil {
			log.Error("ws never connected", "err", err)
			stop()
			<-wsErrCh
			os.Exit(2)
		}
		fetchCtx, fetchCancel := context.WithTimeout(ctx, 15*time.Second)
		cn, bundle, err := brokercreds.FetchIMAP(fetchCtx, wsCli, *credentialName)
		fetchCancel()
		if err != nil {
			log.Error("credential.fetch imap failed", "err", err, "credential_name", *credentialName)
			stop()
			<-wsErrCh
			os.Exit(3)
		}
		credName = cn
		host = bundle.Host
		port = bundle.Port
		user = bundle.User
		password = bundle.Password
	}

	// Non-secret config (default_folder) still comes from the keyfile.
	defaultFolder := "INBOX"
	if kf.IMAP != nil && kf.IMAP.DefaultFolder != "" {
		defaultFolder = kf.IMAP.DefaultFolder
	}

	client := NewClient(host, port, user, password)

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := client.Probe(probeCtx); err != nil {
		cancel()
		log.Error("imap credential probe failed", "err", err, "credential_name", credName)
		stop()
		if wsErrCh != nil {
			<-wsErrCh
		}
		os.Exit(3)
	}
	cancel()
	credSource := "keyfile"
	if brokerFetched {
		credSource = "broker"
	}
	log.Info("nexus-imap-mcp ready",
		"host", host,
		"port", port,
		"username", user,
		"default_folder", defaultFolder,
		"credential_name", credName,
		"credential_source", credSource)

	if *probe {
		stop()
		if wsErrCh != nil {
			<-wsErrCh
		}
		return
	}

	srv := mcpserver.NewMCPServer("nexus-imap", "0.2.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, defaultFolder, log)

	mcpErrCh := make(chan error, 1)
	go func() { mcpErrCh <- mcpserver.ServeStdio(srv) }()

	// In keyfile-only mode there's no WS to watch; block on MCP loop only.
	if wsErrCh == nil {
		if err := <-mcpErrCh; err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
			log.Error("MCP stdio loop ended", "err", err)
		}
		stop()
		return
	}
	// The broker WS is only needed for the one-time credential.fetch above;
	// from here imap calls are direct to the mail server with cached creds.
	// So a broker WS drop must NOT take the MCP down (NEX-482, same as
	// nexus-jira-mcp). Tie the process lifetime to the MCP stdio loop alone;
	// a WS drop is logged in the background and serving continues.
	go func() {
		if err := <-wsErrCh; err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("broker ws exited — non-fatal; imap creds cached, MCP keeps serving", "err", err)
		}
	}()
	if err := <-mcpErrCh; err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
	stop()
}

// authInfo mirrors the jira-mcp shape — local type so the two MCPs
// stay independently editable.
type authInfo struct {
	aspect    string
	jwt       string
	wsURL     string
	expiresAt time.Time
}

func validateKeyfile(ctx context.Context, kf *keyfile.Keyfile, urlOverride string, insecureSkip bool, log *slog.Logger) (*authInfo, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecureSkip} //nolint:gosec // operator opt-in for dev cert
	client := keyfile.NewClient()
	client.HTTP = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}

	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := client.Validate(vctx, kf)
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
		"nexus_id", res.NexusID)

	return &authInfo{
		aspect:    res.AspectName,
		jwt:       res.SessionJWT,
		wsURL:     wsURL,
		expiresAt: res.SessionExpiresAt,
	}, nil
}

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

func waitConnected(ctx context.Context, wsCli *wsclient.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if wsCli.Connected() {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("ws did not connect within timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func loadKeyfile(path string) (*keyfile.Keyfile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var kf keyfile.Keyfile
	if err := json.Unmarshal(buf, &kf); err != nil {
		return nil, fmt.Errorf("parse keyfile: %w", err)
	}
	return &kf, nil
}

func buildLogger(levelStr, logFile string) (*slog.Logger, func(), error) {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, func() {}, fmt.Errorf("bad log level: %s", levelStr)
	}
	out := os.Stderr
	closer := func() {}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, func() {}, err
		}
		out = f
		closer = func() { _ = f.Close() }
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), closer, nil
}

var _ = mcpgo.NewTool
