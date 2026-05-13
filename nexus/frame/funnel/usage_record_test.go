package funnel

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// recordingUsage is the test double for UsageRecorder. Captures
// every Record call so tests can assert msg_id propagation, the
// triggering-msg-id reset, and error tolerance.
type recordingUsage struct {
	mu      sync.Mutex
	records []recordedUsage
	err     error // returned by Record if non-nil
}

type recordedUsage struct {
	MsgID    int64
	TurnID   string
	AspectID string
	Model    string
	Usage    bridle.Usage
}

func (r *recordingUsage) Record(_ context.Context, msgID int64, turnID, aspectID, model string, u bridle.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, recordedUsage{MsgID: msgID, TurnID: turnID, AspectID: aspectID, Model: model, Usage: u})
	return r.err
}

func (r *recordingUsage) snapshot() []recordedUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedUsage, len(r.records))
	copy(out, r.records)
	return out
}

func TestUsageRecorder_DefaultIsNoop(t *testing.T) {
	// Funnel constructed with nil UsageRecorder should still run
	// turns cleanly — NoopUsageRecorder is the safe default.
	f, _ := newTestFunnel(t, bridle.ProviderResult{
		FinalText: "ok",
		Usage:     bridle.Usage{InputTokens: 1, OutputTokens: 1},
	})
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("deliberate with default recorder: %v", err)
	}
}

func TestUsageRecorder_RecordsAfterTurnEnd(t *testing.T) {
	rec := &recordingUsage{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "ok", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 20}},
	}}
	f, err := New(Config{
		AspectID:      "frame",
		Harness:       bridle.NewHarness(prov),
		Provider:      "scripted",
		Model:         "claude-opus-4",
		Runner:        noopRunner{},
		UsageRecorder: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.ReceiveWithMsgID(bridle.InboxItem{From: "operator", Content: "hi"}, 9242)
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	r := got[0]
	if r.MsgID != 9242 {
		t.Errorf("MsgID: got %d, want 9242", r.MsgID)
	}
	if r.AspectID != "frame" {
		t.Errorf("AspectID: got %q, want frame", r.AspectID)
	}
	if r.Model != "claude-opus-4" {
		t.Errorf("Model: got %q", r.Model)
	}
	if r.Usage.InputTokens != 100 || r.Usage.OutputTokens != 20 {
		t.Errorf("Usage: got %+v", r.Usage)
	}
	if r.TurnID == "" {
		t.Error("TurnID should be non-empty")
	}
}

func TestUsageRecorder_TriggeringMsgIDClearsBetweenDeliberations(t *testing.T) {
	rec := &recordingUsage{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "first", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 5}},
		{FinalText: "second", Usage: bridle.Usage{InputTokens: 20, OutputTokens: 7}},
	}}
	f, _ := New(Config{
		AspectID:      "frame",
		Harness:       bridle.NewHarness(prov),
		Provider:      "scripted",
		Model:         "m",
		Runner:        noopRunner{},
		UsageRecorder: rec,
	})

	// First deliberation triggered by msg_id=100.
	f.ReceiveWithMsgID(bridle.InboxItem{From: "operator", Content: "first"}, 100)
	f.Deliberate(context.Background(), "")

	// Second deliberation has no triggering chat msg (e.g. internal
	// op). Should record with MsgID=0.
	f.Deliberate(context.Background(), "internal")

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[0].MsgID != 100 {
		t.Errorf("first msg_id: got %d, want 100", got[0].MsgID)
	}
	if got[1].MsgID != 0 {
		t.Errorf("second msg_id (no trigger): got %d, want 0 — triggering id should clear after first deliberation", got[1].MsgID)
	}
}

func TestUsageRecorder_RecordsOnErrorPath(t *testing.T) {
	// Errored turn should still record — partial usage is real
	// billing the operator should be able to query.
	rec := &recordingUsage{}
	prov := erroringProvider{err: errors.New("rate limited")}
	f, _ := New(Config{
		AspectID:      "frame",
		Harness:       bridle.NewHarness(prov),
		Provider:      "erroring",
		Model:         "m",
		Runner:        noopRunner{},
		UsageRecorder: rec,
	})
	if _, err := f.Deliberate(context.Background(), "ping"); err == nil {
		t.Error("expected error from erroring provider")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("expected 1 record on error path, got %d", got)
	}
}

func TestUsageRecorder_RecorderErrorDoesNotFailDeliberation(t *testing.T) {
	rec := &recordingUsage{err: errors.New("usage table missing")}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "ok", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := New(Config{
		AspectID:      "frame",
		Harness:       bridle.NewHarness(prov),
		Provider:      "scripted",
		Model:         "m",
		Runner:        noopRunner{},
		UsageRecorder: rec,
	})
	res, err := f.Deliberate(context.Background(), "ping")
	if err != nil {
		t.Fatalf("recorder error should not fail deliberation: %v", err)
	}
	if res.TurnResult.FinalText != "ok" {
		t.Errorf("turn result lost: %+v", res)
	}
}

// TestUsageRecorder_FIFOOrderAttribution pins #224 attribution semantics:
// under FIFO, each Deliberate handles ONE message — the queue head.
// Usage is attributed to that one msg_id. Subsequent Deliberate calls
// attribute their own usage to the next queue head, in order.
//
// Pre-#224 behavior was "latest Receive wins" (all queued msgs
// attributed to one turn under one msg_id). FIFO is correct: each
// msg gets its own turn and its own attribution row.
func TestUsageRecorder_FIFOOrderAttribution(t *testing.T) {
	rec := &recordingUsage{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "r1", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "r2", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "r3", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := New(Config{
		AspectID:      "frame",
		Harness:       bridle.NewHarness(prov),
		Provider:      "scripted",
		Model:         "m",
		Runner:        noopRunner{},
		UsageRecorder: rec,
	})
	f.ReceiveWithMsgID(bridle.InboxItem{From: "operator", Content: "first"}, 100)
	f.ReceiveWithMsgID(bridle.InboxItem{From: "operator", Content: "second"}, 200)
	f.ReceiveWithMsgID(bridle.InboxItem{From: "operator", Content: "third"}, 300)

	// Three Deliberate calls drain the queue in FIFO order.
	for i := 0; i < 3; i++ {
		if _, err := f.Deliberate(context.Background(), ""); err != nil {
			t.Fatalf("Deliberate[%d]: %v", i, err)
		}
	}

	snap := rec.snapshot()
	if len(snap) != 3 {
		t.Fatalf("recorded %d usage rows, want 3", len(snap))
	}
	wantIDs := []int64{100, 200, 300}
	for i, want := range wantIDs {
		if snap[i].MsgID != want {
			t.Errorf("usage row[%d]: got msg_id=%d, want %d", i, snap[i].MsgID, want)
		}
	}
}
