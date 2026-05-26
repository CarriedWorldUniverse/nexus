package classification

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
)

// NEX-245: happy-path needs-attention classification — a direct
// @mention or question routes correctly.
func TestCommsDigest_NeedsAttention(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "needs-attention", "reason": "direct question to operator"}`,
	})
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, err := c.Classify(context.Background(), CommsDigestInput{
		From: "anvil",
		Text: "@operator should we ship NEX-244 caller now or wait for the interchange respec?",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.Class != CommsClassNeedsAttention {
		t.Errorf("Class = %q, want %q", verdict.Class, CommsClassNeedsAttention)
	}
}

// NEX-245: background-class for routine peer ack.
func TestCommsDigest_Background(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "background", "reason": "peer ack"}`,
	})
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, _ := c.Classify(context.Background(), CommsDigestInput{
		From: "harrow",
		Text: "ack 👍",
	})
	if verdict.Class != CommsClassBackground {
		t.Errorf("Class = %q, want %q", verdict.Class, CommsClassBackground)
	}
}

// NEX-245: empty text is a caller error — never reaches the
// classifier so model bill is preserved.
func TestCommsDigest_EmptyText(t *testing.T) {
	c := &CommsDigest{
		Harness:  bridle.NewHarness(bridlefake.NewProvider()),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, err := c.Classify(context.Background(), CommsDigestInput{From: "anvil", Text: "  "})
	if err == nil {
		t.Error("expected error on empty text")
	}
}

// NEX-245: harness error fails open to needs-attention. Silent drop
// of a real attention item is much worse than the operator skimming
// one extra background line.
func TestCommsDigest_HarnessErrorFailsOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{Err: errors.New("upstream timeout")})
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, err := c.Classify(context.Background(), CommsDigestInput{
		From: "anvil", Text: "anything",
	})
	if err != nil {
		t.Fatalf("Classify should swallow harness error: %v", err)
	}
	if verdict.Class != CommsClassNeedsAttention {
		t.Errorf("Class = %q, want %q (fail-open)", verdict.Class, CommsClassNeedsAttention)
	}
	if verdict.Reason != "classifier_error" {
		t.Errorf("Reason = %q, want classifier_error", verdict.Reason)
	}
}

// NEX-245: parse error fails open the same way.
func TestCommsDigest_ParseErrorFailsOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{Text: "not valid JSON"})
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, _ := c.Classify(context.Background(), CommsDigestInput{From: "anvil", Text: "x"})
	if verdict.Class != CommsClassNeedsAttention {
		t.Errorf("Class = %q, want needs-attention (fail-open)", verdict.Class)
	}
	if verdict.Reason != "parse_failure" {
		t.Errorf("Reason = %q, want parse_failure", verdict.Reason)
	}
}

// NEX-245: per-call ModelOverride beats env var beats hardcoded
// default. Verified via the request the recording provider saw.
func TestCommsDigest_ModelOverride(t *testing.T) {
	t.Setenv("NEXUS_COMMS_DIGEST_MODEL", "")
	prov := &recordingProvider{reply: `{"class":"background","reason":"x"}`}
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := c.Classify(context.Background(), CommsDigestInput{
		From: "anvil", Text: "test",
		ModelOverride: "claude-haiku-4-5",
	}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	last := prov.LastRequest()
	if last.Model != "claude-haiku-4-5" {
		t.Errorf("model on wire = %q, want claude-haiku-4-5 (per-call override)", last.Model)
	}
}

// NEX-245: env-var fallback when no per-call override.
func TestCommsDigest_ModelEnvVar(t *testing.T) {
	t.Setenv("NEXUS_COMMS_DIGEST_MODEL", "deepseek-v4-flash")
	prov := &recordingProvider{reply: `{"class":"background","reason":"x"}`}
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := c.Classify(context.Background(), CommsDigestInput{From: "anvil", Text: "x"}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	last := prov.LastRequest()
	if last.Model != "deepseek-v4-flash" {
		t.Errorf("model on wire = %q, want deepseek-v4-flash", last.Model)
	}
}

// NEX-245: operator name flows into the system prompt — the
// classifier needs to know whose attention to optimise for.
func TestCommsDigest_SystemPromptIncludesOperatorName(t *testing.T) {
	prov := &recordingProvider{reply: `{"class":"background","reason":"x"}`}
	c := &CommsDigest{
		Harness:      bridle.NewHarness(prov),
		Provider:     "claude-api",
		Model:        "deepseek-chat",
		OperatorName: "jacinta",
	}
	_, _ = c.Classify(context.Background(), CommsDigestInput{From: "anvil", Text: "x"})
	last := prov.LastRequest()
	if !strings.Contains(last.AppendSystemPrompt, "jacinta") {
		t.Errorf("system prompt should mention operator name; got:\n%s", last.AppendSystemPrompt)
	}
}

// NEX-245: empty OperatorName falls back to "operator" as the
// chat-routing layer's convention.
func TestCommsDigest_DefaultOperatorName(t *testing.T) {
	prov := &recordingProvider{reply: `{"class":"background","reason":"x"}`}
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
		// OperatorName intentionally empty
	}
	_, _ = c.Classify(context.Background(), CommsDigestInput{From: "anvil", Text: "x"})
	last := prov.LastRequest()
	if !strings.Contains(last.AppendSystemPrompt, "operator") {
		t.Errorf("system prompt should default to 'operator'; got:\n%s", last.AppendSystemPrompt)
	}
}

// NEX-245: thread context appears in the user message when supplied.
func TestCommsDigest_ThreadHintIncludedInUserMessage(t *testing.T) {
	prov := &recordingProvider{reply: `{"class":"background","reason":"x"}`}
	c := &CommsDigest{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, _ = c.Classify(context.Background(), CommsDigestInput{
		From:       "anvil",
		Text:       "the third reply",
		ThreadHint: "operator: should we ship?\nharrow: i think yes",
	})
	last := prov.LastRequest()
	body := ""
	for _, m := range last.Messages {
		if m.Role == "user" {
			body = m.Content
			break
		}
	}
	if !strings.Contains(body, "THREAD CONTEXT") {
		t.Errorf("user message should include thread context section; got:\n%s", body)
	}
	if !strings.Contains(body, "i think yes") {
		t.Errorf("user message should include the hint text; got:\n%s", body)
	}
}

// NEX-245: ParseCommsDigestVerdict rejects unknown class values.
func TestParseCommsDigestVerdict_InvalidClassRejected(t *testing.T) {
	_, err := ParseCommsDigestVerdict(`{"class":"important","reason":"x"}`)
	if err == nil {
		t.Error("expected error on invalid class value")
	}
}

// NEX-245: ParseCommsDigestVerdict tolerates ```json fences + prose.
func TestParseCommsDigestVerdict_TolerantParsing(t *testing.T) {
	for _, in := range []string{
		"```json\n{\"class\":\"background\",\"reason\":\"ok\"}\n```",
		`Verdict: {"class":"background","reason":"ok"}`,
		`{"class":"background","reason":"ok"}`,
	} {
		v, err := ParseCommsDigestVerdict(in)
		if err != nil {
			t.Errorf("parse failed for %q: %v", in, err)
			continue
		}
		if v.Class != CommsClassBackground {
			t.Errorf("Class = %q for %q", v.Class, in)
		}
	}
}
