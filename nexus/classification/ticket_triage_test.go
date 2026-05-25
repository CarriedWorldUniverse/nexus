package classification

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
)

// recordingProvider captures the ProviderRequest for assertions
// about what the classifier sent on the wire — the bridle fake
// provider replays scripted responses but doesn't expose the
// request, so we inline this small recorder.
type recordingProvider struct {
	reply string
	last  atomic.Value // bridle.ProviderRequest
}

func (*recordingProvider) Name() bridle.ProviderID { return "recording" }
func (*recordingProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}
func (p *recordingProvider) RunTurn(_ context.Context, req bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	p.last.Store(req)
	return bridle.ProviderResult{
		FinalText:  p.reply,
		StopReason: bridle.StopReasonModelDone,
	}, nil
}
func (p *recordingProvider) LastRequest() bridle.ProviderRequest {
	r, _ := p.last.Load().(bridle.ProviderRequest)
	return r
}

// silence unused-import warnings on json — used by some Go build
// configurations even when only referenced from struct tags.
var _ = json.Marshal

// NEX-247: happy-path classification routes to a named aspect with
// high confidence when the description clearly maps to one domain.
func TestTicketTriage_AspectMatch(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"assignee_aspect": "keel", "assignee_team": "", "confidence": "high", "reason": "Frame harness/funnel changes"}`,
	})
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, err := c.Classify(context.Background(), TicketTriageInput{
		Summary:     "Funnel: stream intermediate assistant text blocks to chat per-event",
		Description: "today the funnel only emits the assembled FinalText at TurnDone...",
		Labels:      []string{"funnel", "streaming"},
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.AssigneeAspect != "keel" {
		t.Errorf("AssigneeAspect = %q, want keel", verdict.AssigneeAspect)
	}
	if verdict.AssigneeTeam != "" {
		t.Errorf("AssigneeTeam should be empty when aspect set; got %q", verdict.AssigneeTeam)
	}
	if verdict.Confidence != "high" {
		t.Errorf("Confidence = %q, want high", verdict.Confidence)
	}
}

// NEX-247: low-confidence ambiguous classification routes to the
// manual triage team.
func TestTicketTriage_TeamFallback(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"assignee_aspect": "", "assignee_team": "oss-nexus-dev", "confidence": "low", "reason": "no clear domain"}`,
	})
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, _ := c.Classify(context.Background(), TicketTriageInput{
		Summary: "Improve thing",
	})
	if verdict.AssigneeTeam != "oss-nexus-dev" {
		t.Errorf("AssigneeTeam = %q, want oss-nexus-dev", verdict.AssigneeTeam)
	}
	if verdict.Confidence != "low" {
		t.Errorf("Confidence = %q, want low", verdict.Confidence)
	}
}

// NEX-247: empty Summary is an obvious caller error; never reaches
// the classifier so the model bill is preserved.
func TestTicketTriage_EmptySummary(t *testing.T) {
	c := &TicketTriage{
		Harness:  bridle.NewHarness(bridlefake.NewProvider()),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, err := c.Classify(context.Background(), TicketTriageInput{Summary: ""})
	if err == nil {
		t.Error("expected error on empty summary; got nil")
	}
}

// NEX-247: harness error fails open to manual queue with confidence=low,
// reason=classifier_error. Silent drop is much worse than human re-routing.
func TestTicketTriage_HarnessErrorFailsOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Err: errors.New("upstream timeout"),
	})
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, err := c.Classify(context.Background(), TicketTriageInput{
		Summary: "Something needs triage",
	})
	if err != nil {
		t.Fatalf("Classify should swallow harness error: %v", err)
	}
	if verdict.AssigneeTeam != "oss-nexus-dev" {
		t.Errorf("AssigneeTeam = %q, want oss-nexus-dev (fail-open queue)", verdict.AssigneeTeam)
	}
	if verdict.Confidence != "low" {
		t.Errorf("Confidence = %q, want low", verdict.Confidence)
	}
	if verdict.Reason != "classifier_error" {
		t.Errorf("Reason = %q, want classifier_error", verdict.Reason)
	}
}

// NEX-247: malformed JSON from the model fails open with reason=parse_failure.
func TestTicketTriage_ParseErrorFailsOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `not valid JSON at all`,
	})
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	verdict, _ := c.Classify(context.Background(), TicketTriageInput{
		Summary: "x",
	})
	if verdict.Reason != "parse_failure" {
		t.Errorf("Reason = %q, want parse_failure", verdict.Reason)
	}
	if verdict.AssigneeTeam != "oss-nexus-dev" {
		t.Errorf("AssigneeTeam = %q, want oss-nexus-dev", verdict.AssigneeTeam)
	}
}

// NEX-247: model_override on the call beats env var beats hardcoded
// default (transitively tested via ResolveModel — pin the wiring).
func TestTicketTriage_ModelOverride(t *testing.T) {
	t.Setenv("NEXUS_TICKET_TRIAGE_MODEL", "")
	prov := &recordingProvider{
		reply: `{"assignee_aspect": "shadow", "assignee_team": "", "confidence": "high", "reason": "ticketing"}`,
	}
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := c.Classify(context.Background(), TicketTriageInput{
		Summary:       "Ticket auto-triage lane",
		ModelOverride: "claude-haiku-4-5",
	}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	last := prov.LastRequest()
	if last.Model != "claude-haiku-4-5" {
		t.Errorf("model on wire = %q, want claude-haiku-4-5 (per-call override)", last.Model)
	}
}

// NEX-247: env-var fallback when no per-call override.
func TestTicketTriage_ModelEnvVar(t *testing.T) {
	t.Setenv("NEXUS_TICKET_TRIAGE_MODEL", "deepseek-v4-flash")
	prov := &recordingProvider{
		reply: `{"assignee_aspect": "shadow", "assignee_team": "", "confidence": "high", "reason": "x"}`,
	}
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	if _, err := c.Classify(context.Background(), TicketTriageInput{Summary: "x"}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	last := prov.LastRequest()
	if last.Model != "deepseek-v4-flash" {
		t.Errorf("model on wire = %q, want deepseek-v4-flash (env-var)", last.Model)
	}
}

// NEX-247: system prompt includes the aspect roster + the canonical
// names from DefaultAspectRoster.
func TestTicketTriage_SystemPromptIncludesRoster(t *testing.T) {
	prov := &recordingProvider{
		reply: `{"assignee_aspect": "keel", "assignee_team": "", "confidence": "high", "reason": "x"}`,
	}
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	_, _ = c.Classify(context.Background(), TicketTriageInput{Summary: "x"})
	last := prov.LastRequest()
	for _, want := range []string{"shadow", "keel", "anvil", "plumb", "forge", "wren", "harrow"} {
		if !strings.Contains(last.AppendSystemPrompt, want) {
			t.Errorf("system prompt missing aspect %q; roster not embedded?", want)
		}
	}
	if !strings.Contains(last.AppendSystemPrompt, "oss-nexus-dev") {
		t.Errorf("system prompt missing fallback team name oss-nexus-dev")
	}
}

// NEX-247: custom AspectRoster overrides DefaultAspectRoster.
func TestTicketTriage_CustomAspectRoster(t *testing.T) {
	prov := &recordingProvider{
		reply: `{"assignee_aspect": "custom1", "assignee_team": "", "confidence": "high", "reason": "x"}`,
	}
	c := &TicketTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
		AspectRoster: map[string]string{
			"custom1": "first custom aspect",
			"custom2": "second custom aspect",
		},
	}
	_, _ = c.Classify(context.Background(), TicketTriageInput{Summary: "x"})
	last := prov.LastRequest()
	if !strings.Contains(last.AppendSystemPrompt, "custom1") {
		t.Errorf("custom roster not used; system prompt missing custom1")
	}
	// Should NOT contain defaults — operator's roster wins.
	if strings.Contains(last.AppendSystemPrompt, "shadow") {
		t.Errorf("default roster leaked when custom set; system prompt contains shadow")
	}
}

// NEX-247: ParseTicketTriageVerdict rejects mutually-exclusive
// assignee fields set together.
func TestParseTicketTriageVerdict_BothAssigneesRejected(t *testing.T) {
	_, err := ParseTicketTriageVerdict(`{"assignee_aspect":"keel","assignee_team":"oss-nexus-dev","confidence":"low","reason":"x"}`)
	if err == nil {
		t.Error("expected error when both assignee fields set")
	}
}

// NEX-247: ParseTicketTriageVerdict requires at least one assignee.
func TestParseTicketTriageVerdict_NoAssigneeRejected(t *testing.T) {
	_, err := ParseTicketTriageVerdict(`{"assignee_aspect":"","assignee_team":"","confidence":"low","reason":"x"}`)
	if err == nil {
		t.Error("expected error when neither assignee field set")
	}
}

// NEX-247: ParseTicketTriageVerdict rejects unknown confidence values.
func TestParseTicketTriageVerdict_BadConfidenceRejected(t *testing.T) {
	_, err := ParseTicketTriageVerdict(`{"assignee_aspect":"keel","confidence":"definitely","reason":"x"}`)
	if err == nil {
		t.Error("expected error on bad confidence value")
	}
}

// NEX-247: ParseTicketTriageVerdict tolerates ```json fences +
// prose around the JSON object (mirror of ParseVerdict tolerance).
func TestParseTicketTriageVerdict_TolerantParsing(t *testing.T) {
	cases := []string{
		// fenced
		"```json\n{\"assignee_aspect\":\"keel\",\"confidence\":\"high\",\"reason\":\"ok\"}\n```",
		// prose preamble
		`Here's the verdict: {"assignee_aspect":"keel","confidence":"high","reason":"ok"}`,
		// bare
		`{"assignee_aspect":"keel","confidence":"high","reason":"ok"}`,
	}
	for _, c := range cases {
		v, err := ParseTicketTriageVerdict(c)
		if err != nil {
			t.Errorf("parse failed on %q: %v", c, err)
			continue
		}
		if v.AssigneeAspect != "keel" {
			t.Errorf("AssigneeAspect = %q, want keel for input %q", v.AssigneeAspect, c)
		}
	}
}
