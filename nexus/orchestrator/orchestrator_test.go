package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// --- fakeGraph: an in-memory workgraph.WorkGraph fake ---

type fakeItem struct {
	wi      workgraph.WorkItem
	claimed string // agent that claimed it, "" = unclaimed
	status  workgraph.Status
}

type fakeGraph struct {
	mu       sync.Mutex
	items    map[string]*fakeItem
	seq      int
	results  map[string][]workgraph.Result
	cancels  []string // ids Cancel(requeue=true) was called on, in order
	nextID   func() string
	transErr map[string]error
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		items:   map[string]*fakeItem{},
		results: map[string][]workgraph.Result{},
	}
}

func (g *fakeGraph) addReady(id string, wi workgraph.WorkItem) {
	g.mu.Lock()
	defer g.mu.Unlock()
	wi.ID = id
	wi.Status = workgraph.StatusQueued
	g.items[id] = &fakeItem{wi: wi, status: workgraph.StatusQueued}
}

func (g *fakeGraph) ListReady(_ context.Context, role, _ string) ([]workgraph.WorkItem, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []workgraph.WorkItem
	for _, it := range g.items {
		if it.wi.Role == role && (it.status == workgraph.StatusQueued || it.status == workgraph.StatusDispatched) {
			wi := it.wi
			wi.Status = it.status
			out = append(out, wi)
		}
	}
	return out, nil
}

func (g *fakeGraph) GetWorkItem(_ context.Context, id string) (workgraph.WorkItem, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	it, ok := g.items[id]
	if !ok {
		return workgraph.WorkItem{}, errors.New("fakeGraph: not found")
	}
	// it.status (kept separately by Transition/Cancel, and by tests that
	// poke it.status directly) is the current status — mirror ListReady's
	// convention (real workgraph.Client.GetWorkItem always reflects the
	// ledger's live status, so the fake must too, or ReapStale's ledger
	// recheck sees stale/wrong data).
	wi := it.wi
	wi.Status = it.status
	return wi, nil
}

func (g *fakeGraph) Transition(_ context.Context, id string, s workgraph.Status) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.transErr[id]; err != nil {
		return err
	}
	it, ok := g.items[id]
	if !ok {
		return errors.New("fakeGraph: not found")
	}
	it.status = s
	return nil
}

func (g *fakeGraph) RecordResult(_ context.Context, id string, result workgraph.Result) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.results[id] = append(g.results[id], result)
	it, ok := g.items[id]
	if !ok {
		return errors.New("fakeGraph: not found")
	}
	switch result.Verdict {
	case workgraph.VerdictDone:
		it.status = workgraph.StatusDone
	case workgraph.VerdictBlocked:
		it.status = workgraph.StatusBlocked
	}
	return nil
}

func (g *fakeGraph) Rework(_ context.Context, rejectedID string, newSpec workgraph.WorkItem) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	old, ok := g.items[rejectedID]
	if !ok {
		return "", errors.New("fakeGraph: rejected item not found")
	}
	g.seq++
	newID := "rework-" + itoa(g.seq)
	item := newSpec
	item.ID = newID
	if item.Role == "" {
		item.Role = old.wi.Role
	}
	item.Status = workgraph.StatusQueued
	g.items[newID] = &fakeItem{wi: item, status: workgraph.StatusQueued}
	return newID, nil
}

func (g *fakeGraph) Claim(_ context.Context, id, agent string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	it, ok := g.items[id]
	if !ok {
		return errors.New("fakeGraph: not found")
	}
	if it.claimed != "" {
		return workgraph.ErrAlreadyClaimed
	}
	it.claimed = agent
	return nil
}

func (g *fakeGraph) Cancel(_ context.Context, id string, requeue bool, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	it, ok := g.items[id]
	if !ok {
		return errors.New("fakeGraph: not found")
	}
	if requeue {
		it.status = workgraph.StatusQueued
		it.claimed = ""
		g.cancels = append(g.cancels, id)
	} else {
		it.status = workgraph.StatusCancelled
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// --- fakeDispatcher: records every SubmitPoolItem call ---

type fakeDispatcher struct {
	mu    sync.Mutex
	calls []dispatch.PoolItem
	err   error
}

func (d *fakeDispatcher) SubmitPoolItem(_ context.Context, item dispatch.PoolItem) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return "", d.err
	}
	d.calls = append(d.calls, item)
	return "run-" + item.WorkItemID, nil
}

// --- fakeWorkerStatus ---

type fakeWorkerStatus struct {
	mu      sync.Mutex
	rows    []workerstatus.Status
	deleted []string // agents Delete was called on, in order
}

func (s *fakeWorkerStatus) List(_ context.Context) ([]workerstatus.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]workerstatus.Status, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *fakeWorkerStatus) Delete(_ context.Context, agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, agent)
	kept := s.rows[:0]
	for _, r := range s.rows {
		if r.Agent != agent {
			kept = append(kept, r)
		}
	}
	s.rows = kept
	return nil
}

// --- fakeAlerter ---

type fakeAlerter struct {
	mu    sync.Mutex
	calls []string // subjects, in order
}

func (a *fakeAlerter) Alert(_ context.Context, subject, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, subject)
	return nil
}

func (a *fakeAlerter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

func TestDrainOnceDispatchesReadyItems(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build the thing"})
	graph.addReady("wi-2", workgraph.WorkItem{Role: "tester", TaskSpec: "test the thing"})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder", "tester"},
	}

	report, err := o.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(report.Dispatched) != 2 {
		t.Fatalf("expected 2 dispatched, got %d (%v)", len(report.Dispatched), report.Dispatched)
	}
	if len(disp.calls) != 2 {
		t.Fatalf("expected 2 SubmitPoolItem calls, got %d", len(disp.calls))
	}
	if graph.items["wi-1"].status != workgraph.StatusDispatched {
		t.Errorf("wi-1 status = %v, want dispatched", graph.items["wi-1"].status)
	}
}

// TestDrainOnceThreadsAcceptanceCriteria covers Unit B's WorkItem ->
// PoolItem leg of the acceptance-criteria threading (NET-22/23/24):
// dispatchOne formats wi.AcceptanceCriteria as bullet text on
// PoolItem.AcceptanceCriteria so it reaches the Brief (and, via BuildJob,
// -acceptance-file) unmodified. A work item with no criteria must NOT set
// the field at all — reproducing today's Brief with no acceptance.md.
func TestDrainOnceThreadsAcceptanceCriteria(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{
		Role:               "builder",
		TaskSpec:           "build the thing",
		AcceptanceCriteria: []string{"must produce token CONVERGED-OK", "no test regressions"},
	})
	graph.addReady("wi-2", workgraph.WorkItem{Role: "builder", TaskSpec: "build the other thing"})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
	}

	if _, err := o.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(disp.calls) != 2 {
		t.Fatalf("expected 2 SubmitPoolItem calls, got %d", len(disp.calls))
	}
	var withCriteria, without dispatch.PoolItem
	for _, c := range disp.calls {
		if c.WorkItemID == "wi-1" {
			withCriteria = c
		} else {
			without = c
		}
	}
	want := "- must produce token CONVERGED-OK\n- no test regressions"
	if withCriteria.AcceptanceCriteria != want {
		t.Errorf("AcceptanceCriteria = %q, want %q", withCriteria.AcceptanceCriteria, want)
	}
	if without.AcceptanceCriteria != "" {
		t.Errorf("work item with no criteria must produce empty AcceptanceCriteria, got %q", without.AcceptanceCriteria)
	}
}

// TestDrainOnceThreadsRepo covers the Phase 4 "real REPO tickets" gap:
// dispatchOne must carry wi.Repo straight onto PoolItem.Repo so it reaches
// Brief.Repo (and, downstream, the git-credential grant / -repo arg / PR
// gate) — see nexus/workgraph/README.md's repo mapping note and
// runtime/dispatch/README.md's pool model. A work item with no repo (every
// pre-Phase-4 caller, and respond-only work today) must produce an empty
// PoolItem.Repo, reproducing today's behavior exactly.
func TestDrainOnceThreadsRepo(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{
		Role:     "builder",
		TaskSpec: "fix the bug",
		Repo:     "CarriedWorldUniverse/nexus",
	})
	graph.addReady("wi-2", workgraph.WorkItem{Role: "builder", TaskSpec: "respond to the thread"})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
	}

	if _, err := o.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(disp.calls) != 2 {
		t.Fatalf("expected 2 SubmitPoolItem calls, got %d", len(disp.calls))
	}
	var withRepo, without dispatch.PoolItem
	for _, c := range disp.calls {
		if c.WorkItemID == "wi-1" {
			withRepo = c
		} else {
			without = c
		}
	}
	if withRepo.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("Repo = %q, want CarriedWorldUniverse/nexus", withRepo.Repo)
	}
	if without.Repo != "" {
		t.Errorf("work item with no repo must produce empty PoolItem.Repo, got %q", without.Repo)
	}
}

// TestDrainOnceThreadsPersonality covers per-personality routing: dispatchOne
// must carry wi.Personality straight onto PoolItem.Personality so it reaches
// Brief.RequestedPersonality (and, downstream, the lease — see
// runtime/dispatch tryLeaseWorkerSlot and docs/network/ROLE-MODEL.md). A
// work item with no requested personality (every pre-routing caller) must
// produce an empty PoolItem.Personality, reproducing today's "any free
// personality" behavior exactly.
func TestDrainOnceThreadsPersonality(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{
		Role:        "builder",
		TaskSpec:    "fix the bug",
		Personality: "keel",
	})
	graph.addReady("wi-2", workgraph.WorkItem{Role: "builder", TaskSpec: "respond to the thread"})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
	}

	if _, err := o.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(disp.calls) != 2 {
		t.Fatalf("expected 2 SubmitPoolItem calls, got %d", len(disp.calls))
	}
	var withPersonality, without dispatch.PoolItem
	for _, c := range disp.calls {
		if c.WorkItemID == "wi-1" {
			withPersonality = c
		} else {
			without = c
		}
	}
	if withPersonality.Personality != "keel" {
		t.Errorf("Personality = %q, want keel", withPersonality.Personality)
	}
	if without.Personality != "" {
		t.Errorf("work item with no requested personality must produce empty PoolItem.Personality, got %q", without.Personality)
	}
}

func TestDrainOnceSkipsAlreadyDispatchedAcrossTwoPasses(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build the thing"})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
	}

	// Idempotency is STATUS-based (confirmed against the live sovereign
	// ledger): the first pass dispatches wi-1 (queued) and transitions it to
	// dispatched (In Progress). ListReady still returns it on the second pass
	// (ledger's ready query includes In Progress for worker-resume), but
	// dispatchOne skips it because its status is no longer queued — no
	// double-dispatch, no Claim needed (assignment IS the claim in the
	// ledger, so the orchestrator can't Claim anyway).
	first, err := o.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("first DrainOnce: %v", err)
	}
	if len(first.Dispatched) != 1 {
		t.Fatalf("first pass: expected 1 dispatched, got %d", len(first.Dispatched))
	}

	second, err := o.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("second DrainOnce: %v", err)
	}
	if len(second.Dispatched) != 0 {
		t.Fatalf("second pass: expected 0 dispatched (already In Progress), got %d", len(second.Dispatched))
	}
	if len(second.Skipped) != 1 || second.Skipped[0] != "wi-1" {
		t.Fatalf("second pass: expected wi-1 skipped, got %v", second.Skipped)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("expected exactly 1 SubmitPoolItem call total (no double-dispatch), got %d", len(disp.calls))
	}
}

func TestRecordJobResultDoneTransitionsToDone(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   &fakeDispatcher{},
		WorkerStatus: &fakeWorkerStatus{},
	}

	_, err := o.RecordJobResult(context.Background(), "wi-1", workgraph.Result{Verdict: workgraph.VerdictDone})
	if err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}
	if graph.items["wi-1"].status != workgraph.StatusDone {
		t.Errorf("status = %v, want done", graph.items["wi-1"].status)
	}
}

func TestRecordJobResultRejectCreatesRework(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build", AcceptanceCriteria: []string{"must work"}})
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   &fakeDispatcher{},
		WorkerStatus: &fakeWorkerStatus{},
	}

	before := len(graph.items)
	_, err := o.RecordJobResult(context.Background(), "wi-1", workgraph.Result{
		Verdict: workgraph.VerdictReject,
		Reasons: []string{"tests fail"},
	})
	if err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}
	if len(graph.items) != before+1 {
		t.Fatalf("expected a rework item created, item count %d -> %d", before, len(graph.items))
	}
	var found bool
	for id, it := range graph.items {
		if id == "wi-1" {
			continue
		}
		found = true
		if it.wi.Role != "builder" {
			t.Errorf("rework item role = %q, want builder (inherited)", it.wi.Role)
		}
	}
	if !found {
		t.Fatal("no rework item found")
	}
}

func TestPreflightAuthFailureHoldsDrain(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	disp := &fakeDispatcher{}
	alerter := &fakeAlerter{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
		Alerter:      alerter,
		AuthProbe: func(context.Context) error {
			return errors.New("frontier token expired")
		},
	}

	report, err := o.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if !report.Held {
		t.Fatal("expected Held=true")
	}
	if len(disp.calls) != 0 {
		t.Fatalf("expected no dispatch on auth-hold, got %d calls", len(disp.calls))
	}
	if graph.items["wi-1"].status != workgraph.StatusQueued {
		t.Errorf("item status = %v, want unchanged (queued)", graph.items["wi-1"].status)
	}
	if alerter.count() != 1 {
		t.Fatalf("expected 1 alert, got %d", alerter.count())
	}
}

func TestReapStaleRequeuesAndSecondStrikeAlerts(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	graph.items["wi-1"].status = workgraph.StatusDispatched

	old := time.Now().Add(-1 * time.Hour)
	ws := &fakeWorkerStatus{rows: []workerstatus.Status{
		{Agent: "anvil-builder", WorkItemID: "wi-1", LastHeartbeat: old},
	}}
	alerter := &fakeAlerter{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   &fakeDispatcher{},
		WorkerStatus: ws,
		Alerter:      alerter,
		StaleAfter:   5 * time.Minute,
	}

	reaped, err := o.ReapStale(context.Background())
	if err != nil {
		t.Fatalf("ReapStale (1st): %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "wi-1" {
		t.Fatalf("expected wi-1 reaped, got %v", reaped)
	}
	if graph.items["wi-1"].status != workgraph.StatusQueued {
		t.Errorf("status = %v, want queued (requeued)", graph.items["wi-1"].status)
	}
	if alerter.count() != 0 {
		t.Fatalf("first strike should not alert, got %d alerts", alerter.count())
	}

	// A normal drain pass between reap calls would redispatch the
	// now-queued item (ListReady -> Claim -> Transition to dispatched) —
	// simulate that here so pass 2 finds a genuinely in-flight item again.
	// Still stale on that next pass (heartbeat never refreshed — the
	// re-lease's worker is ALSO not heartbeating): second strike must
	// alert.
	graph.items["wi-1"].status = workgraph.StatusDispatched
	reaped2, err := o.ReapStale(context.Background())
	if err != nil {
		t.Fatalf("ReapStale (2nd): %v", err)
	}
	if len(reaped2) != 1 {
		t.Fatalf("expected wi-1 reaped again, got %v", reaped2)
	}
	if alerter.count() != 1 {
		t.Fatalf("second strike should alert exactly once, got %d alerts", alerter.count())
	}
}

func TestReapStaleRecoveryClearsStrike(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	graph.items["wi-1"].status = workgraph.StatusDispatched

	stale := time.Now().Add(-1 * time.Hour)
	ws := &fakeWorkerStatus{rows: []workerstatus.Status{
		{Agent: "anvil-builder", WorkItemID: "wi-1", LastHeartbeat: stale},
	}}
	alerter := &fakeAlerter{}
	o := &Orchestrator{
		Graph: graph, Dispatcher: &fakeDispatcher{}, WorkerStatus: ws,
		Alerter: alerter, StaleAfter: 5 * time.Minute,
	}

	if _, err := o.ReapStale(context.Background()); err != nil {
		t.Fatalf("ReapStale (1st): %v", err)
	}

	// Recovers: heartbeat now fresh.
	ws.rows[0].LastHeartbeat = time.Now()
	if _, err := o.ReapStale(context.Background()); err != nil {
		t.Fatalf("ReapStale (recovered pass): %v", err)
	}

	// Goes stale again: this should be a FIRST strike again (cleared by
	// the recovery), not a second-strike alert.
	ws.rows[0].LastHeartbeat = stale
	if _, err := o.ReapStale(context.Background()); err != nil {
		t.Fatalf("ReapStale (stale again): %v", err)
	}
	if alerter.count() != 0 {
		t.Fatalf("strike should have reset on recovery, got %d alerts", alerter.count())
	}
}

// TestReapStaleDoesNotRequeueCancelledItem is the live-reproduced NET-30
// (2026-07-05) failure: a work item is CANCELLED in the ledger, but its
// finished worker's worker_status row is still present (never retired) and
// stale. ReapStale must NOT requeue a cancelled item — before this fix, it
// trusted the stale row alone and requeued it every pass, forever. It must
// also clean up the now-confirmed-harmless row so it stops being examined.
func TestReapStaleDoesNotRequeueCancelledItem(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("NET-30", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	graph.items["NET-30"].status = workgraph.StatusCancelled

	old := time.Now().Add(-1 * time.Hour)
	ws := &fakeWorkerStatus{rows: []workerstatus.Status{
		{Agent: "anvil-builder", WorkItemID: "NET-30", LastHeartbeat: old},
	}}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   &fakeDispatcher{},
		WorkerStatus: ws,
		StaleAfter:   5 * time.Minute,
	}

	reaped, err := o.ReapStale(context.Background())
	if err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("expected nothing reaped for a cancelled item, got %v", reaped)
	}
	if graph.items["NET-30"].status != workgraph.StatusCancelled {
		t.Errorf("status = %v, want unchanged (cancelled)", graph.items["NET-30"].status)
	}
	if len(graph.cancels) != 0 {
		t.Fatalf("expected Cancel(requeue=true) never called, got %v", graph.cancels)
	}
	if len(ws.deleted) != 1 || ws.deleted[0] != "anvil-builder" {
		t.Fatalf("expected the stale-but-harmless row cleaned up, deleted=%v", ws.deleted)
	}

	// Confirm the loop is actually broken: a SECOND pass over the (now
	// row-less) store finds nothing to reap either.
	reaped2, err := o.ReapStale(context.Background())
	if err != nil {
		t.Fatalf("ReapStale (2nd pass): %v", err)
	}
	if len(reaped2) != 0 {
		t.Fatalf("expected nothing reaped on the second pass either, got %v", reaped2)
	}
}

// TestReapStaleDedupesTwoStaleRowsForSameItem covers the other live-observed
// symptom: two finished runs of the SAME work item each left a stale row
// (`reaped="[NET-30 NET-30]"` in the live log) — at most one
// Cancel(requeue=true) per work item per pass.
func TestReapStaleDedupesTwoStaleRowsForSameItem(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	graph.items["wi-1"].status = workgraph.StatusDispatched

	old := time.Now().Add(-1 * time.Hour)
	ws := &fakeWorkerStatus{rows: []workerstatus.Status{
		{Agent: "anvil-builder", WorkItemID: "wi-1", LastHeartbeat: old},
		{Agent: "birch-builder", WorkItemID: "wi-1", LastHeartbeat: old},
	}}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   &fakeDispatcher{},
		WorkerStatus: ws,
		StaleAfter:   5 * time.Minute,
	}

	reaped, err := o.ReapStale(context.Background())
	if err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "wi-1" {
		t.Fatalf("expected exactly one reap entry for wi-1, got %v", reaped)
	}
	if len(graph.cancels) != 1 {
		t.Fatalf("expected exactly one Cancel(requeue=true) call, got %v", graph.cancels)
	}
}

func TestOnJobDoneHookCallsIntakeAndDrain(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build"})
	graph.items["wi-1"].status = workgraph.StatusDispatched
	graph.addReady("wi-2", workgraph.WorkItem{Role: "builder", TaskSpec: "next", DependsOn: nil})
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
	}

	hook := o.OnJobDoneHook()
	hook(dispatch.JobDone{Ticket: "wi-1", OK: true})

	if graph.items["wi-1"].status != workgraph.StatusDone {
		t.Errorf("wi-1 status = %v, want done (RecordJobResult ran)", graph.items["wi-1"].status)
	}
	if len(disp.calls) != 1 || disp.calls[0].WorkItemID != "wi-2" {
		t.Fatalf("expected the re-drain to dispatch wi-2, calls=%v", disp.calls)
	}
}
