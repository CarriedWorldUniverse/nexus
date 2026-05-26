// Activity-summary subcommand for nexus (NEX-246 caller, lane 3 of NEX-243).
//
// `nexus activity-summary --classifier-credential <provider-cred>
//                         --nexus-url wss://broker/connect
//                         (--operator-token T | --operator-token-file F)
//                         --aspect anvil [--aspect harrow ...]
//                         [--since 1h] [--collect-window 3s]
//                         [--data-dir DIR] [--timeout-s 300]
//                         [--insecure-skip-verify]`
//
// Subscribes to the broker's observability stream for each --aspect,
// drains retained tail + collects frames for --collect-window, then
// runs ActivitySummary (NEX-246 Slice 1) and prints a markdown digest
// plus deterministic per-kind / per-aspect counts to stdout.
//
// Source choice: live broker Hub (not jsonlsink files) because the
// dashboard CLI is expected to run on a separate host from the
// broker — see operator's NEX-246 design choice. Auth model mirrors
// nexus-watch + nexus-comms-mcp: operator JWT via --operator-token
// (UNSAFE for prod — leaks via ps) or --operator-token-file (safe).

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
)

func runActivitySummarySubcommand(args []string) int {
	fs := flag.NewFlagSet("activity-summary", flag.ContinueOnError)
	classifierCred := fs.String("classifier-credential", "", "name of a kind=provider credential for the classifier (required)")
	classifierProvider := fs.String("classifier-provider", "openai", "bridle provider: claude-api | openai (default openai per NEX-298)")
	classifierModel := fs.String("classifier-model", "deepseek-chat", "default model (env NEXUS_ACTIVITY_SUMMARY_MODEL wins; per-call override wins over both)")
	classifierBaseURL := fs.String("classifier-base-url", "", "override classifier provider base URL")
	nexusURL := fs.String("nexus-url", "", "WS URL of the nexus broker (e.g. wss://host:port/connect) — required")
	opToken := fs.String("operator-token", "", "Operator JWT — UNSAFE for prod (leaks via ps); prefer --operator-token-file")
	opTokenFile := fs.String("operator-token-file", "", "Read operator JWT from this file (safe for secrets)")
	insecureSkip := fs.Bool("insecure-skip-verify", false, "Skip TLS cert verification (dev/self-signed only)")
	var aspects repeatableStringFlag
	fs.Var(&aspects, "aspect", "Aspect to subscribe to; repeat for multiple (required)")
	since := fs.Duration("since", 1*time.Hour, "drop frames older than this from the digest input")
	collectWindow := fs.Duration("collect-window", 3*time.Second, "wait this long after subscribing for tail drain to settle")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 300, "wall-clock budget for the whole run in seconds")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *classifierCred == "" || *nexusURL == "" || len(aspects) == 0 {
		fmt.Fprintln(os.Stderr, "activity-summary: --classifier-credential, --nexus-url, and at least one --aspect are required")
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutS)*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, cleanup, code := openCredentialsStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	auth, err := resolveActivitySummaryAuth(*opToken, *opTokenFile, *nexusURL, *insecureSkip)
	if err != nil {
		fmt.Fprintf(os.Stderr, "activity-summary: auth: %v\n", err)
		return 2
	}

	summariser, err := buildActivitySummariser(ctx, store, *classifierCred, *classifierProvider, *classifierModel, *classifierBaseURL, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "activity-summary: build summariser: %v\n", err)
		return 1
	}

	collected, err := collectFrames(ctx, auth, aspects, *collectWindow, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "activity-summary: collect frames: %v\n", err)
		return 1
	}

	cutoff := time.Now().Add(-*since)
	events := framesToActivityEvents(collected, cutoff)
	log.Info("activity-summary: collected",
		"raw_frames", len(collected),
		"after_cutoff", len(events),
		"aspects", []string(aspects))

	if len(events) == 0 {
		fmt.Println("(no activity in window)")
		return 0
	}

	windowStart := time.Now().Add(-*since)
	out, err := summariser.Summarise(ctx, classification.ActivitySummaryInput{
		Events:      events,
		WindowStart: windowStart,
		WindowEnd:   time.Now(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "activity-summary: summarise: %v\n", err)
		return 1
	}

	fmt.Print(renderActivityDigest(out, *since, aspects))
	return 0
}

// resolveActivitySummaryAuth mirrors resolveOperatorAuth from
// runtime/cmd/nexus-watch/main.go — duplicated rather than extracted
// because that file's comment notes "a third client would justify
// runtime/operatorws/" and we are that third client; the extraction
// is a follow-up.
type activitySummaryAuth struct {
	jwt    string
	wsURL  string
	tls    *tls.Config
}

func resolveActivitySummaryAuth(opToken, opTokenFile, urlOverride string, insecure bool) (*activitySummaryAuth, error) {
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
	return &activitySummaryAuth{
		jwt:   jwt,
		wsURL: toActivitySummaryWSURL(urlOverride),
		tls:   &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // user-opt-in
	}, nil
}

func toActivitySummaryWSURL(in string) string {
	out := strings.TrimRight(in, "/")
	switch {
	case strings.HasPrefix(out, "https://"):
		out = "wss://" + strings.TrimPrefix(out, "https://")
	case strings.HasPrefix(out, "http://"):
		out = "ws://" + strings.TrimPrefix(out, "http://")
	}
	if !strings.HasSuffix(out, "/connect") {
		out += "/connect"
	}
	return out
}

// buildActivitySummariser mirrors buildPRTriageClassifier /
// buildCommsDigestClassifier.
func buildActivitySummariser(ctx context.Context, store *credentials.Store, credName, providerName, model, baseURLOverride string, log *slog.Logger) (*classification.ActivitySummary, error) {
	cred, err := store.Get(ctx, credName)
	if err != nil {
		return nil, fmt.Errorf("get classifier credential %q: %w", credName, err)
	}
	if cred.Kind != credentials.KindProvider {
		return nil, fmt.Errorf("classifier credential %q is kind=%q, want provider", credName, cred.Kind)
	}
	bundle, err := store.ProviderBundle(cred)
	if err != nil {
		return nil, fmt.Errorf("unwrap provider bundle: %w", err)
	}
	endpoint := baseURLOverride
	if endpoint == "" {
		endpoint = bundle.BaseURL
	}
	var provider bridle.Provider
	var providerID bridle.ProviderID
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "claude-api", "claude", "anthropic":
		provider = claudeprovider.NewWithBaseURL(bundle.Key, endpoint)
		providerID = bridle.ProviderClaude
	case "openai":
		provider = openaiprovider.NewWithBaseURL(bundle.Key, endpoint)
		providerID = bridle.ProviderOpenAI
	default:
		return nil, fmt.Errorf("unsupported --classifier-provider %q (claude-api | openai)", providerName)
	}
	return &classification.ActivitySummary{
		Harness:  bridle.NewHarness(provider),
		Provider: providerID,
		Model:    model,
		Logger:   log,
	}, nil
}

// collectFrames opens an operator WS, subscribes to each aspect with
// since_seq=0 (full retained tail), waits collectWindow for frames to
// flow, unsubscribes, and returns whatever landed. The wait is the
// simplest viable signal — the broker doesn't emit "tail drained"
// markers and retained-buffer drain happens before any live frame.
//
// Concurrency: a single channel collects from the WS handler
// (one goroutine inside wsclient) into a slice the main goroutine
// reads after the window elapses; muOut guards the slice.
func collectFrames(ctx context.Context, auth *activitySummaryAuth, aspects []string, collectWindow time.Duration, log *slog.Logger) ([]observability.Frame, error) {
	var (
		muOut     sync.Mutex
		collected []observability.Frame
	)

	handler := wsclient.HandlerFunc(func(env frames.Envelope) {
		if env.Kind != frames.KindObserveFrame {
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
		if f.Aspect == "" {
			f.Aspect = p.Aspect
		}
		muOut.Lock()
		collected = append(collected, f)
		muOut.Unlock()
	})

	wsCli, err := wsclient.New(wsclient.Config{
		URL:              auth.wsURL,
		AuthToken:        auth.jwt,
		Handler:          handler,
		Logger:           log,
		FailFirstConnect: true,
	})
	if err != nil {
		return nil, fmt.Errorf("wsclient.New: %w", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	runErr := make(chan error, 1)
	go func() { runErr <- wsCli.Run(runCtx) }()

	// Wait for the first connect event before subscribing.
	connected := false
	select {
	case ev := <-wsCli.Events():
		connected = ev.Connected
	case err := <-runErr:
		return nil, fmt.Errorf("ws connect failed: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !connected {
		return nil, errors.New("ws first event was disconnected")
	}

	for _, aspect := range aspects {
		if err := sendActivitySubscribe(runCtx, wsCli, aspect, 0); err != nil {
			log.Warn("subscribe.observe failed", "aspect", aspect, "err", err)
		}
	}

	// Wait for tail to drain + a brief live window. Frames keep
	// landing into `collected` via the handler goroutine.
	select {
	case <-time.After(collectWindow):
	case <-ctx.Done():
	}

	// Best-effort unsubscribe — broker drops subs on disconnect anyway.
	for _, aspect := range aspects {
		_ = sendActivityUnsubscribe(runCtx, wsCli, aspect)
	}

	runCancel()
	<-runErr // drain Run's exit value; nil on clean cancel.

	muOut.Lock()
	out := append([]observability.Frame(nil), collected...)
	muOut.Unlock()
	return out, nil
}

func sendActivitySubscribe(ctx context.Context, ws *wsclient.Client, aspect string, sinceSeq int64) error {
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

func sendActivityUnsubscribe(ctx context.Context, ws *wsclient.Client, aspect string) error {
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

// framesToActivityEvents converts the collected observability frames
// into the source-agnostic ActivityEvent shape consumed by the
// classifier. Frames with TS older than cutoff are skipped (the
// broker's retained tail can hold older history than --since).
//
// Each frame becomes one event with a kind-specific summary line.
// Skips kinds that aren't in the prompt vocabulary yet (defensive
// against future FrameKind additions).
func framesToActivityEvents(in []observability.Frame, cutoff time.Time) []classification.ActivityEvent {
	out := make([]classification.ActivityEvent, 0, len(in))
	for _, f := range in {
		if f.TS.Before(cutoff) {
			continue
		}
		summary := summariseFrame(f)
		if summary == "" {
			continue
		}
		out = append(out, classification.ActivityEvent{
			TS:      f.TS,
			Aspect:  f.Aspect,
			Kind:    string(f.Kind),
			Summary: summary,
		})
	}
	return out
}

// summariseFrame renders a one-line description of a Frame keyed by
// Kind. Returns "" for unknown kinds — the caller filters those out.
func summariseFrame(f observability.Frame) string {
	switch f.Kind {
	case observability.FrameTurn:
		var tf observability.TurnFrame
		if err := json.Unmarshal(f.Payload, &tf); err != nil {
			return ""
		}
		return summariseTurnFrame(tf)
	case observability.FrameChat:
		var cf observability.ChatFrame
		if err := json.Unmarshal(f.Payload, &cf); err != nil {
			return ""
		}
		return summariseChatFrame(cf)
	case observability.FramePresence:
		var pf observability.PresenceFrame
		if err := json.Unmarshal(f.Payload, &pf); err != nil {
			return ""
		}
		conn := "connected"
		if !pf.Connected {
			conn = "disconnected"
		}
		reason := pf.Reason
		if reason == "" {
			reason = "(no reason)"
		}
		return fmt.Sprintf("presence: %s (%s)", conn, reason)
	case observability.FrameFilterDecision:
		var df observability.FilterDecisionFrame
		if err := json.Unmarshal(f.Payload, &df); err != nil {
			return ""
		}
		verdict := "drop"
		if df.ShouldPost {
			verdict = "post"
		}
		return fmt.Sprintf("filter: %s class=%s reason=%s", verdict, df.Class, df.Reason)
	}
	return ""
}

func summariseTurnFrame(tf observability.TurnFrame) string {
	label := tf.Label
	if label == "" {
		label = "main"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "turn label=%s status=%s", label, tf.Status)
	if tf.Model != "" {
		fmt.Fprintf(&b, " model=%s", tf.Model)
	}
	if tf.Ended != nil {
		fmt.Fprintf(&b, " duration=%s", tf.Ended.Sub(tf.Started).Round(time.Millisecond))
	}
	if tf.Usage != nil {
		fmt.Fprintf(&b, " usage_in=%d usage_out=%d", tf.Usage.InputTokens, tf.Usage.OutputTokens)
	}
	if tf.Error != "" {
		fmt.Fprintf(&b, " error=%q", truncatePreview(tf.Error, 80))
	}
	return b.String()
}

func summariseChatFrame(cf observability.ChatFrame) string {
	preview := truncatePreview(cf.Content, 100)
	dir := "→"
	if cf.Direction == observability.DirectionInbound {
		dir = "←"
	}
	return fmt.Sprintf("chat %s from=%s: %s", dir, cf.From, preview)
}

// renderActivityDigest is the operator-facing output. Markdown blurb
// from the model lands first, then the deterministic counts so the
// operator always has the quantitative breakdown even when the model
// failed (counts are populated regardless per Slice 1's fail-open).
func renderActivityDigest(out classification.ActivitySummaryOutput, since time.Duration, aspects []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Activity summary (last %s — aspects: %s)\n\n",
		since.String(), strings.Join(aspects, ", "))

	b.WriteString(strings.TrimSpace(out.Markdown))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "## Counts\n\n")
	fmt.Fprintf(&b, "- Total: %d\n", out.Counts.Total)
	fmt.Fprintf(&b, "- By kind: %s\n", joinKVMap(out.Counts.ByKind))
	fmt.Fprintf(&b, "- By aspect: %s\n", joinKVMap(out.Counts.ByAspect))
	return b.String()
}

// joinKVMap renders a {key: count} as "k=v k=v ..." sorted by count
// desc then key asc — matches the classifier's prompt-time ordering
// so the operator sees the same shape they would have classified on.
func joinKVMap(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	// Sort identical to classification.joinSortedCounts (count desc,
	// key asc) so operator-visible output and prompt input agree.
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].v > pairs[i].v ||
				(pairs[j].v == pairs[i].v && pairs[j].k < pairs[i].k) {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s=%d", p.k, p.v)
	}
	return strings.Join(parts, " ")
}
