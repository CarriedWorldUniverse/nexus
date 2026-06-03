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

// TestE2EEscalationApprove / DenyBash are the P3c live proof. Policy
// ESCALATES bash (VerdictEscalate). A FAKE Requester stands in for the
// operator: case A approves → DeepSeek's bash actually runs and we see
// real `id` output; case B denies → bash is refused with "operator
// denied" and the model adapts.
//
// This exercises the same chain agentfunnel wires for native providers:
// ComposeRunner(CommsRunner+local toolrunner) + the registered
// PermissionHook, but with an Escalate policy and an in-process
// Requester instead of the live broker/operator (the broker relay is
// proven separately by the full-WS broker tests). Live DeepSeek drives
// the model side.
//
// Env-gated: skips unless BRIDLE_E2E_OPENAI_KEY is set. Run live with
// the start-plumb DeepSeek key:
//
//	BRIDLE_E2E_OPENAI_KEY=<key> BRIDLE_E2E_OPENAI_BASE=https://api.deepseek.com/v1 \
//	  go test ./nexus/frame/funnel/ -run TestE2EEscalation -v

func escalationE2ESetup(t *testing.T) (string, *bridle.Harness, bridle.ToolRunner, []bridle.ToolDef) {
	t.Helper()
	key := os.Getenv("BRIDLE_E2E_OPENAI_KEY")
	if key == "" {
		t.Skip("set BRIDLE_E2E_OPENAI_KEY to run the live escalation chain")
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
	return wd, h, runner, defs
}

func TestE2EEscalationApproveBash(t *testing.T) {
	_, h, runner, defs := escalationE2ESetup(t)

	// Policy: bash requires operator approval each call (everything else
	// allowed). The fake operator APPROVES.
	policy := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	fakeOp := &fakeRequester{decision: "approve"}
	h.RegisterBeforeToolCall(PermissionHook(policy, &Escalator{Requester: fakeOp, AspectID: "e2e-esc"}))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req := bridle.TurnRequest{
		AspectID:    "e2e-esc",
		Provider:    bridle.ProviderOpenAI,
		Model:       "deepseek-chat",
		MaxSteps:    8,
		Tools:       defs,
		UserMessage: "Run the bash command `echo escalation-approved-marker`. Then tell me exactly what it printed.",
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
	if bash == nil {
		t.Fatal("expected the model to attempt bash")
	}
	t.Logf("APPROVE case — bash invocation: err=%q result=%s", bash.Err, bash.Result)

	// The operator saw the escalation with the funnel-injected aspect id.
	if fakeOp.gotReq.Aspect != "e2e-esc" || fakeOp.gotReq.Tool != "bash" {
		t.Errorf("operator escalation request = %+v, want aspect=e2e-esc tool=bash", fakeOp.gotReq)
	}
	// HARD: approve means bash actually RAN — no permission/escalation error,
	// and the real echo output is in the result.
	if bash.Err != "" {
		t.Fatalf("APPROVE: bash should have run, but carries err=%q", bash.Err)
	}
	if !strings.Contains(string(bash.Result), "escalation-approved-marker") {
		t.Fatalf("APPROVE: bash result should contain the echoed marker, got %s", bash.Result)
	}
	t.Logf("APPROVE PROVEN: operator-approved bash executed and returned real output: %s", bash.Result)
}

func TestE2EEscalationDenyBash(t *testing.T) {
	wd, h, runner, defs := escalationE2ESetup(t)

	// Policy: bash requires operator approval. The fake operator DENIES.
	policy := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	fakeOp := &fakeRequester{decision: "deny", note: "not allowed in this environment"}
	h.RegisterBeforeToolCall(PermissionHook(policy, &Escalator{Requester: fakeOp, AspectID: "e2e-esc"}))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req := bridle.TurnRequest{
		AspectID: "e2e-esc",
		Provider: bridle.ProviderOpenAI,
		Model:    "deepseek-chat",
		MaxSteps: 8,
		Tools:    defs,
		UserMessage: "First run the bash command `id`. If it is not permitted, do NOT retry it — " +
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
	if bash == nil {
		t.Fatal("expected the model to attempt bash")
	}
	t.Logf("DENY case — bash invocation: err=%q result=%s", bash.Err, bash.Result)

	// HARD: deny means bash was refused with the operator-denied error,
	// not real `id` output.
	if !strings.Contains(bash.Err, "operator denied") {
		t.Fatalf("DENY: bash should be denied by the operator, got err=%q result=%s", bash.Err, bash.Result)
	}
	t.Logf("DENY PROVEN: bash refused with %q", bash.Err)

	// SOFT: model adapted — wrote blocked.txt.
	if b, rerr := os.ReadFile(wd + "/blocked.txt"); rerr != nil || !strings.Contains(string(b), "DENIED") {
		t.Logf("WARN: model did not adapt after denial (blocked.txt missing/wrong, err=%v) — deny still proven", rerr)
	} else {
		t.Logf("model adapted: blocked.txt=%q", strings.TrimSpace(string(b)))
	}
}
