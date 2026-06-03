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

// TestE2ECompositeLocalPlusComms is the P2 deliverable: a LIVE model uses
// BOTH lanes in one turn — a LOCAL coding tool (write, via
// toolrunner.LocalToolRunner) AND a HOST comms tool (send_chat, via
// CommsRunner) — composed through ComposeRunner exactly as agentfunnel
// wires it for native-API providers.
//
// Identity safety: the chat sender is NOT supplied by the model. The
// model-facing send_chat schema only carries content/reply_to/topic, and
// ChatGateway.SendChat's signature is
//
//	SendChat(ctx, content string, replyTo int64, topic string) (int64, error)
//
// — there is NO "from"/sender argument the model can set. The sender is
// bound by the funnel/gateway, never the model. This test relies on that
// (it asserts the gateway recorded the send, with no way for the model to
// have spoofed identity).
//
// Env-gated: skips unless BRIDLE_E2E_OPENAI_KEY is set. Run live with:
//
//	BRIDLE_E2E_OPENAI_KEY=<key> BRIDLE_E2E_OPENAI_BASE=https://api.deepseek.com/v1 \
//	  go test ./nexus/frame/funnel/ -run TestE2EComposite -v
func TestE2ECompositeLocalPlusComms(t *testing.T) {
	key := os.Getenv("BRIDLE_E2E_OPENAI_KEY")
	if key == "" {
		t.Skip("set BRIDLE_E2E_OPENAI_KEY to run the live composite chain")
	}
	base := os.Getenv("BRIDLE_E2E_OPENAI_BASE")
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}

	// The existing comms_test.go fake gateway. Records every SendChat in
	// its sentMessages slice; snapshotSent() returns a copy under lock.
	// send_chat only touches CommsRunner.Gateway — Knowledge/Triage stay
	// nil (read runSendChat: it calls only r.Gateway.SendChat).
	fg := &fakeGateway{}
	comms := CommsRunner{Gateway: fg}

	wd := t.TempDir()
	local, err := toolrunner.New(toolrunner.Config{WorkDir: wd})
	if err != nil {
		t.Fatal(err)
	}
	runner := ComposeRunner(comms, local)
	defs := append(CommsToolDefs(), toolrunner.Defs()...)

	h := bridle.NewHarness(openai.NewWithBaseURL(key, base))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := bridle.TurnRequest{
		AspectID: "e2e-composite",
		Provider: bridle.ProviderOpenAI,
		Model:    "deepseek-chat",
		MaxSteps: 8,
		Tools:    defs,
		UserMessage: "Do two things using the tools provided: " +
			"(1) use the write tool to create a file named report.txt containing exactly the text DONE. " +
			"(2) use the send_chat tool to post the message 'report ready'. " +
			"Then reply confirming both are complete.",
	}

	res, err := h.RunTurn(ctx, req, runner, eventSinkDiscard{})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	var names []string
	for _, tc := range res.ToolCalls {
		names = append(names, tc.Name)
	}
	t.Logf("stop=%s steps=%d tools=%v final=%q", res.StopReason, res.StepCount, names, res.FinalText)

	// LOCAL lane fired: report.txt written under the toolrunner WorkDir.
	if b, rerr := os.ReadFile(wd + "/report.txt"); rerr != nil || !strings.Contains(string(b), "DONE") {
		t.Errorf("local write lane did not produce report.txt with DONE (err=%v, content=%q)", rerr, string(b))
	}

	// HOST comms lane fired: the gateway recorded a send. Identity is the
	// gateway's responsibility — the model had no "from" to supply.
	sent := fg.snapshotSent()
	if len(sent) == 0 {
		t.Fatalf("host comms lane did not fire; tools=%v", names)
	}
	foundReportReady := false
	for _, m := range sent {
		if strings.Contains(m.Content, "report ready") {
			foundReportReady = true
			break
		}
	}
	if !foundReportReady {
		t.Errorf("no recorded send contained 'report ready'; sends=%+v", sent)
	}
}

type eventSinkDiscard struct{}

func (eventSinkDiscard) Emit(bridle.Event) {}
