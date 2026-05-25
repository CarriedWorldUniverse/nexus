package rewriter

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// recordingProvider captures the bridle.ProviderRequest the distiller
// hands to the harness so tests can assert which fields were set.
// Minimal stub — returns a canned text response and records the
// request for inspection.
type recordingProvider struct {
	calls atomic.Int32
	last  atomic.Value // bridle.ProviderRequest
	reply string
}

func (*recordingProvider) Name() bridle.ProviderID { return "recording" }
func (*recordingProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}
func (p *recordingProvider) RunTurn(_ context.Context, req bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	p.calls.Add(1)
	p.last.Store(req)
	reply := p.reply
	if reply == "" {
		reply = "compressed"
	}
	return bridle.ProviderResult{
		FinalText:  reply,
		StopReason: bridle.StopReasonModelDone,
	}, nil
}

// dummyToolRunner satisfies the harness's ToolRunner interface for
// the distiller path; distillation is MaxSteps=1 / no tools so it's
// never actually called.
type dummyToolRunner struct{}

func (dummyToolRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// NEX-300 (rewriter slice): the distiller's TurnRequest carries
// Temperature (non-zero, low) and a bounded MaxOutputTokens — using
// the standard provider knobs bridle exposed in NEX-299 Pass 2.
// Asserts via the recording-provider's captured request; no live API.
func TestHaikuDistiller_NEX300_TurnRequestCarriesSamplingFields(t *testing.T) {
	prov := &recordingProvider{reply: "tight"}
	d := &HaikuDistiller{
		Harness:  bridle.NewHarness(prov),
		Provider: "recording",
		Model:    "haiku",
	}

	out, err := d.DistillAssistantText(context.Background(), "verbose reasoning that should compress")
	if err != nil {
		t.Fatalf("DistillAssistantText: %v", err)
	}
	if out != "tight" {
		t.Errorf("distill output = %q, want %q", out, "tight")
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("expected 1 RunTurn call, got %d", prov.calls.Load())
	}

	last := prov.last.Load().(bridle.ProviderRequest)
	if last.Temperature == nil {
		t.Fatal("distiller TurnRequest should set Temperature *float64 (got nil)")
	}
	if *last.Temperature <= 0 || *last.Temperature >= 1 {
		t.Errorf("distiller Temperature = %v, want in (0, 1) — non-zero so distillation reads naturally, low so it stays mostly deterministic", *last.Temperature)
	}
	if last.MaxOutputTokens <= 0 {
		t.Errorf("distiller MaxOutputTokens = %d, want positive bound", last.MaxOutputTokens)
	}
	if last.MaxOutputTokens > 500 {
		t.Errorf("distiller MaxOutputTokens = %d, suspiciously high for byte-capped distillation", last.MaxOutputTokens)
	}
	// Distiller doesn't constrain output shape — it produces prose, not
	// structured data. ResponseFormat must stay nil so providers don't
	// reject the call on a missing required schema.
	if last.ResponseFormat != nil {
		t.Errorf("distiller ResponseFormat should be nil (prose output); got %+v", last.ResponseFormat)
	}
}

// NEX-300 (rewriter slice): same assertion for the tool-result
// distillation path. Both DistillToolResult and DistillAssistantText
// share runDistill; this test guards against accidental divergence
// (e.g. a future refactor that splits the paths).
func TestHaikuDistiller_NEX300_ToolResultPathAlsoCarriesFields(t *testing.T) {
	prov := &recordingProvider{reply: "ok exit 0"}
	d := &HaikuDistiller{
		Harness:  bridle.NewHarness(prov),
		Provider: "recording",
		Model:    "haiku",
	}
	_, err := d.DistillToolResult(context.Background(), "Bash", "long bash output...")
	if err != nil {
		t.Fatalf("DistillToolResult: %v", err)
	}
	last := prov.last.Load().(bridle.ProviderRequest)
	if last.Temperature == nil || last.MaxOutputTokens <= 0 {
		t.Errorf("tool-result distill path missing sampling fields: Temperature=%v MaxOutputTokens=%d",
			last.Temperature, last.MaxOutputTokens)
	}
}
