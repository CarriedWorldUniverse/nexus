package funnel

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	openai "github.com/CarriedWorldUniverse/bridle/provider/openai"
	"github.com/CarriedWorldUniverse/bridle/toolrunner"
)

// TestE2EPermissionDeniesBash is the P3b live proof: an aspect whose
// ToolPolicy DENIES bash asks DeepSeek to run a bash command. The
// PermissionHook (via bridle's P3a Deny path) refuses the call by handing
// the model a "permission denied" tool_result instead of executing it, so
// the model sees the refusal mid-turn and (ideally) adapts by writing
// blocked.txt instead.
//
// HARD assertion: bash was attempted AND carries an Err containing
// "permission denied" (the deny mechanism worked — no real `id` output).
// SOFT assertion: blocked.txt was written (the model adapted). If DeepSeek
// stubbornly retries bash instead of adapting, that still proves the deny;
// the adaptation check is logged, not failed.
//
// Reuses fakeGateway (comms_test.go) + ComposeRunner + eventSinkDiscard
// (composite_e2e_test.go) — same wiring agentfunnel uses for native-API
// providers, plus the registered PermissionHook.
//
// Env-gated: skips unless BRIDLE_E2E_OPENAI_KEY is set. Run live with:
//
//	BRIDLE_E2E_OPENAI_KEY=<key> BRIDLE_E2E_OPENAI_BASE=https://api.deepseek.com/v1 \
//	  go test ./nexus/frame/funnel/ -run TestE2EPermission -v
func TestE2EPermissionDeniesBash(t *testing.T) {
	key := os.Getenv("BRIDLE_E2E_OPENAI_KEY")
	if key == "" {
		t.Skip("set BRIDLE_E2E_OPENAI_KEY to run the live permission chain")
	}
	base := os.Getenv("BRIDLE_E2E_OPENAI_BASE")
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}

	wd := t.TempDir()
	local, err := toolrunner.New(toolrunner.Config{WorkDir: wd})
	if err != nil {
		t.Fatal(err)
	}
	runner := ComposeRunner(CommsRunner{Gateway: &fakeGateway{}}, local)
	defs := append(CommsToolDefs(), toolrunner.Defs()...)

	h := bridle.NewHarness(openai.NewWithBaseURL(key, base))
	// Policy: bash DENIED, everything else allowed (mirrors agentfunnel's
	// native-provider hook registration, but with a non-permissive policy
	// so we can prove the deny path end to end).
	h.RegisterBeforeToolCall(PermissionHook(ToolPolicy{DefaultAllow: true, Tools: map[string]bool{"bash": false}}))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req := bridle.TurnRequest{
		AspectID: "e2e-perm",
		Provider: bridle.ProviderOpenAI,
		Model:    "deepseek-chat",
		MaxSteps: 8,
		Tools:    defs,
		UserMessage: "First run the bash command `id`. If that is not permitted, do not retry it — " +
			"instead use the write tool to create blocked.txt containing the text DENIED, then tell me what happened.",
	}
	res, err := h.RunTurn(ctx, req, runner, eventSinkDiscard{})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	var bash *bridle.ToolInvocation
	for i := range res.ToolCalls {
		if res.ToolCalls[i].Name == "bash" {
			bash = &res.ToolCalls[i]
		}
	}
	t.Logf("stop=%s steps=%d tools=%d final=%q", res.StopReason, res.StepCount, len(res.ToolCalls), res.FinalText)

	// HARD: bash was attempted AND denied (carries the permission error,
	// not real `id` output).
	if bash == nil {
		t.Fatal("expected the model to attempt bash")
	}
	t.Logf("bash invocation: err=%q result=%s", bash.Err, bash.Result)
	if !strings.Contains(bash.Err, "permission denied") {
		t.Fatalf("bash should be denied by policy, got err=%q result=%s", bash.Err, bash.Result)
	}

	// SOFT: model adapted — wrote blocked.txt under the workdir.
	if b, rerr := os.ReadFile(wd + "/blocked.txt"); rerr != nil || !strings.Contains(string(b), "DENIED") {
		t.Logf("WARN: model did not adapt after denial (blocked.txt missing/wrong, err=%v) — deny still proven", rerr)
	} else {
		t.Logf("model adapted: blocked.txt=%q", strings.TrimSpace(string(b)))
	}
}
