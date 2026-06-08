// Test-provider subcommand for nexus.
//
// `nexus test-provider --credential NAME --provider {claude-api|openai}
//
//	[--base-url URL] --model MODEL --prompt TEXT
//	[--tools] [--stream] [--steps N]` exercises a
//
// real bridle provider end-to-end against the real endpoint, using a
// credential from the local credentials store. Prints structured
// result: provider, endpoint, model, duration, usage, response,
// status.
//
// NEX-297 Layer 1: validate native bridle providers (claude/openai
// over HTTP, via NEX-295's NewWithBaseURL) BEFORE any aspect depends
// on them. The cheap-judge path historically went through claude CLI
// subprocess + ANTHROPIC_BASE_URL env override; this subcommand
// proves the SDK-wrapped native providers actually work against the
// same endpoints (api.anthropic.com, DeepSeek's /anthropic, OpenAI's
// /v1, DeepSeek's /v1, etc.) before NEX-294 flips any defaults.
//
// No aspect setup, no broker startup, no WS dance. Operator-runnable
// in seconds against any stored credential.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// runTestProviderSubcommand parses flags + runs one turn against a
// real provider. Returns process exit code.
func runTestProviderSubcommand(args []string) int {
	fs := flag.NewFlagSet("test-provider", flag.ContinueOnError)
	credentialName := fs.String("credential", "", "name of a provider-kind credential in the nexus credentials store (required)")
	providerName := fs.String("provider", "", "bridle provider: claude-api | openai (required)")
	baseURL := fs.String("base-url", "", "override the provider's base URL (defaults to the credential's base_url, then the SDK default)")
	model := fs.String("model", "", "model id passed to the provider (required)")
	prompt := fs.String("prompt", "", "user message to send (required)")
	useTools := fs.Bool("tools", false, "register a dummy 'echo' tool to exercise tool-call roundtrip")
	stream := fs.Bool("stream", false, "print streaming chunks as they arrive (debug visibility)")
	maxSteps := fs.Int("steps", 1, "harness MaxSteps (1 = single round, no tool loop; >1 lets tools execute)")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	timeoutS := fs.Int("timeout-s", 30, "request timeout in seconds")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *credentialName == "" || *providerName == "" || *model == "" || *prompt == "" {
		fmt.Fprintln(os.Stderr, "test-provider: --credential, --provider, --model, --prompt are all required")
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutS)*time.Second)
	defer cancel()

	store, cleanup, code := openCredentialsStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	cred, err := store.Get(ctx, *credentialName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-provider: load credential %q: %v\n", *credentialName, err)
		return 1
	}
	if cred.Kind != credentials.KindProvider {
		fmt.Fprintf(os.Stderr, "test-provider: credential %q is kind=%q; need kind=provider\n", cred.Name, cred.Kind)
		return 1
	}
	bundle, err := store.ProviderBundle(cred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-provider: unwrap credential: %v\n", err)
		return 1
	}

	// Pick endpoint: explicit --base-url > credential.base_url > SDK
	// default (empty string passes through to NewWithBaseURL which
	// falls back to the SDK's default endpoint).
	endpoint := *baseURL
	if endpoint == "" {
		endpoint = bundle.BaseURL
	}

	provider, providerID, err := buildTestProvider(*providerName, bundle.Key, endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-provider: %v\n", err)
		return 1
	}

	// Streaming sink: optionally print chunks as they arrive so the
	// operator can see the wire-level behaviour (timing, event order).
	sink := buildTestSink(*stream)

	tools := []bridle.ToolDef{}
	var runner bridle.ToolRunner = testProviderNullRunner{}
	if *useTools {
		tools = []bridle.ToolDef{{
			Name:        "echo",
			Description: "Echo back the input as a tool result (test-provider tool-call roundtrip).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		}}
		runner = testProviderEchoRunner{}
	}

	req := bridle.TurnRequest{
		Model:       *model,
		UserMessage: *prompt,
		MaxSteps:    *maxSteps,
		Tools:       tools,
		Provider:    providerID,
	}

	harness := bridle.NewHarness(provider)
	start := time.Now()
	result, err := harness.RunTurn(ctx, req, runner, sink)
	duration := time.Since(start)

	endpointForReport := endpoint
	if endpointForReport == "" {
		endpointForReport = "(SDK default)"
	}
	fmt.Printf("provider: %s\n", providerID)
	fmt.Printf("endpoint: %s\n", endpointForReport)
	fmt.Printf("model:    %s\n", *model)
	fmt.Printf("duration: %s\n", duration.Round(time.Millisecond))
	fmt.Printf("usage:    in=%d out=%d cache_read=%d cache_create=%d\n",
		result.Usage.InputTokens, result.Usage.OutputTokens,
		result.Usage.CacheReadInputTokens, result.Usage.CacheCreationInputTokens)
	if len(result.ToolCalls) > 0 {
		names := make([]string, len(result.ToolCalls))
		for i, tc := range result.ToolCalls {
			names[i] = tc.Name
		}
		sort.Strings(names)
		fmt.Printf("tools:    %s\n", strings.Join(names, ", "))
	}
	fmt.Printf("result:   %s\n", oneline(result.FinalText))
	if err != nil {
		fmt.Printf("status:   error: %v\n", err)
		return 1
	}
	fmt.Printf("status:   ok (stop=%s)\n", result.StopReason)
	return 0
}

// buildTestProvider instantiates a bridle provider by name. Honours
// the explicit endpoint when non-empty; passes it through
// NewWithBaseURL which falls back to the SDK default when empty.
// NEX-295 made this possible — pre-NEX-295 the only way to point at
// a non-default endpoint was via env vars + the claude CLI
// subprocess.
func buildTestProvider(name, key, baseURL string) (bridle.Provider, bridle.ProviderID, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-api", "claude", "anthropic":
		return claudeprovider.NewWithBaseURL(key, baseURL), bridle.ProviderClaude, nil
	case "openai":
		return openaiprovider.NewWithBaseURL(key, baseURL), bridle.ProviderOpenAI, nil
	default:
		return nil, "", fmt.Errorf("unsupported --provider %q (supported: claude-api, openai)", name)
	}
}

// buildTestSink returns an EventSink that optionally prints stream
// chunks. When stream=false it discards everything except the final
// TurnDone marker (which we surface via the structured report
// anyway). When stream=true it prints chunk text + tool-call events
// to stdout as they arrive so the operator can eyeball cadence.
func buildTestSink(stream bool) bridle.EventSink {
	if !stream {
		return testProviderNullSink{}
	}
	return testProviderStreamSink{}
}

type testProviderNullSink struct{}

func (testProviderNullSink) Emit(_ bridle.Event) {}

// testProviderNullRunner satisfies bridle.ToolRunner for the no-tools
// path (model emits no tool calls; harness never calls Run).
type testProviderNullRunner struct{}

func (testProviderNullRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// testProviderEchoRunner echoes the call's args as the tool result.
// Lets the model see its own input back so the roundtrip is visible
// AND args-parsing correctness is observable.
type testProviderEchoRunner struct{}

func (testProviderEchoRunner) Run(_ context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	if len(call.Args) == 0 {
		return json.RawMessage(`{"echo":""}`), nil
	}
	return call.Args, nil
}

type testProviderStreamSink struct{}

func (testProviderStreamSink) Emit(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ModelChunk:
		if e.Text != "" {
			fmt.Print(e.Text)
		}
	case bridle.ToolCallStart:
		fmt.Printf("\n[tool→ %s(%s)]", e.Name, string(e.Args))
	case bridle.ToolCallResult:
		if e.Err != "" {
			fmt.Printf(" [tool← err: %s]\n", e.Err)
		} else {
			fmt.Printf(" [tool← %s]\n", oneline(string(e.Result)))
		}
	case bridle.TurnDone:
		fmt.Println()
	}
}

// oneline collapses whitespace + caps at 200 chars so the printed
// result fits on one terminal line. The model's full output is what
// matters for "did this work"; if the operator wants full text they
// can run with --stream.
func oneline(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if len(s) > 200 {
		s = s[:197] + "…"
	}
	return s
}
