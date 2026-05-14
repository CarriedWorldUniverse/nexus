// Command nexus-jira-mcp bridges Atlassian Jira REST to stdio MCP.
// One process == one aspect's Atlassian identity. Credentials are
// fetched from the Nexus broker via the credential.fetch WS frame
// (NEX-77); the keyfile is still required because it provides the JWT
// auth path and the default_project_key for jira.create, but the
// secret material (email + token + subdomain) no longer needs to live
// on the remote host's disk.
//
// Tools exposed:
//
//   jira.search          — generic JQL search
//   jira.get             — fetch a single issue
//   jira.list_my_issues  — what's assigned to me, optionally filtered by status
//   jira.list_ready      — Ready/To Do work this aspect could claim
//   jira.claim           — set self as assignee + move to In Progress
//   jira.comment         — post a plain-text comment
//   jira.update_status   — transition by status name
//   jira.create          — file an Epic / Story / Task / Subtask / Bug
//   jira.complete        — transition to Done (or In Review when awaitReview=true)
//
// Identity is whoever the broker credential authenticates as. For
// shadow's keyfile the broker resolves the aspect's default jira
// credential (--credential-name overrides). The MCP host gets exactly
// the surface the resolved Atlassian account can do — no privilege
// escalation, no service-account impersonation.
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

const aspectMCPName = "nexus-jira"

func main() {
	var (
		keyfilePath    = flag.String("keyfile", "", "Path to the aspect keyfile JSON. Required: provides JWT auth to Nexus + optional .jira block for non-secret config (project_key).")
		credentialName = flag.String("credential-name", "", "Specific jira credential name to fetch from the broker. Empty (default) → broker resolves the aspect's default jira credential.")
		nexusURLFlag   = flag.String("nexus-url", "", "Override the WS URL (default: derived from keyfile envelope, with scheme rewritten and /connect appended).")
		insecureSkip   = flag.Bool("insecure-skip-verify", false, "Skip TLS cert verification for the validate handshake (dev/self-signed only).")
		logLevel       = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile        = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the MCP protocol stream.")
		probe          = flag.Bool("probe", false, "Don't start MCP — call /rest/api/3/myself and exit. Useful for credential smoke tests.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-jira-mcp: logger setup: %v\n", err)
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

	// Validate the keyfile against Nexus to get the WS JWT + URL.
	auth, err := validateKeyfile(ctx, kf, *nexusURLFlag, *insecureSkip, log)
	if err != nil {
		log.Error("keyfile validate failed", "err", err)
		os.Exit(2)
	}

	// Dial the broker WS and hold it open for the credential fetch.
	// We don't need any push frames (no register), just the
	// request/response correlation for credential.fetch.
	wsCli, err := wsclient.New(wsclient.Config{
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
	wsErrCh := make(chan error, 1)
	go func() { wsErrCh <- wsCli.Run(ctx) }()

	// Wait for the initial WS connect before fetching. FailFirstConnect
	// guarantees Run returns fast if the dial fails; otherwise we poll
	// briefly until Connected() — wsclient does not expose a synchronous
	// "wait for first connect" hook today and adding one is out of scope.
	if err := waitConnected(ctx, wsCli, 10*time.Second); err != nil {
		log.Error("ws never connected", "err", err)
		stop()
		<-wsErrCh
		os.Exit(2)
	}

	// Fetch jira credentials from the broker.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 15*time.Second)
	credName, bundle, err := brokercreds.FetchJira(fetchCtx, wsCli, *credentialName)
	fetchCancel()
	if err != nil {
		log.Error("credential.fetch jira failed", "err", err, "credential_name", *credentialName)
		stop()
		<-wsErrCh
		os.Exit(3)
	}

	// Compose the full Atlassian site hostname from the subdomain.
	// JiraBundle stores subdomain only ("carriedworlduniverse"); the
	// existing jiraClient expects the full hostname
	// ("carriedworlduniverse.atlassian.net").
	site := bundle.Subdomain + ".atlassian.net"

	// Non-secret config (project_key) still comes from the keyfile.
	// Per work-routing v1.2 + NEX-88 follow-up: the broker bundle
	// deliberately does not carry per-aspect config; the keyfile keeps
	// that role for now.
	var projectKey string
	if kf.Jira != nil {
		projectKey = kf.Jira.ProjectKey
	}

	client := newJiraClient(site, bundle.Email, bundle.Token, projectKey, nil)

	// Credential smoke test — confirms we can reach Atlassian + auth
	// works before starting the MCP loop.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	accountID, err := client.MyAccountID(probeCtx)
	cancel()
	if err != nil {
		log.Error("jira credential probe failed", "err", err, "credential_name", credName)
		stop()
		<-wsErrCh
		os.Exit(3)
	}
	log.Info("nexus-jira-mcp ready (fetched 1 credential from broker for "+auth.aspect+")",
		"site", site,
		"email", bundle.Email,
		"account_id", accountID,
		"project_key", projectKey,
		"credential_name", credName,
		"aspect", auth.aspect)

	if *probe {
		stop()
		<-wsErrCh
		return
	}

	srv := mcpserver.NewMCPServer("nexus-jira", "0.2.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, log)

	mcpErrCh := make(chan error, 1)
	go func() { mcpErrCh <- mcpserver.ServeStdio(srv) }()

	select {
	case err := <-mcpErrCh:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
			log.Error("MCP stdio loop ended", "err", err)
		}
	case err := <-wsErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("ws client exited", "err", err)
		}
	}
	stop()
	// Best-effort drain of the goroutine we didn't read from; ignore err.
	select {
	case <-wsErrCh:
	default:
	}
}

// authInfo carries everything we need from the keyfile validate handshake
// to dial /connect.
type authInfo struct {
	aspect    string
	jwt       string
	wsURL     string
	expiresAt time.Time
}

// validateKeyfile runs the spec §5 startup handshake and returns the
// dial info. Mirrors comms-mcp's resolveKeyfileAuth but trimmed to just
// the keyfile path (no operator-token mode — MCPs always run with a
// keyfile).
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

// toWSURL normalises a URL into wss://host:port/connect form. Same shape
// as comms-mcp's helper.
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

// waitConnected blocks until wsCli reports Connected(), the context
// fires, or the timeout expires. Polled because wsclient doesn't expose
// a "wait for first connect" channel separate from its Events stream,
// and adding one is out of scope for this migration.
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

// loadKeyfile reads the keyfile JSON and resolves the path relative to
// the working directory so callers can pass plain "shadow.keyfile.json"
// the same way nexus-comms-mcp does.
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

// buildLogger returns a slog.Logger pointed at logFile (when set) or
// stderr (default). NEVER stdout — MCP claims that channel.
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

// Compile-time guards so unused-but-imported helpers stay referenced.
var _ = mcpgo.NewTool
var _ = aspectMCPName
