package dispatch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

func newPoolFixture(fk *fakeK8s) (*dispatch.Runner, *recordingPoster, *spawnRecorder) {
	r := newRunner(fk)
	p := &recordingPoster{}
	rec := &spawnRecorder{}
	r.Poster = p
	r.Recorder = rec
	r.MintHandCredential = func(_ context.Context, _, derived string) (string, error) {
		return "jwt-for-" + derived, nil
	}
	_ = r.Init(context.Background())
	return r, p, rec
}

// N concurrent pool work-items lease N distinct slots (default pool
// size 3).
func TestSubmitPoolLeasesDistinctSlots(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	var ids []string
	for i := 1; i <= 3; i++ {
		id, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-"+itoa(i), "")
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Fatalf("work item %d should launch immediately (pool not yet at cap)", i)
		}
		ids = append(ids, id)
	}
	for _, want := range []string{"pool.sub-1", "pool.sub-2", "pool.sub-3"} {
		if !r.AgentBusy(want) {
			t.Errorf("%s should be leased busy", want)
		}
	}
	if len(fk.jobs) != 3 {
		t.Fatalf("jobs = %v, want 3", fk.jobs)
	}
	_ = ids
}

// The (N+1)th pool work-item queues (empty run id, no error) rather
// than launching once all N slots are leased.
func TestSubmitPoolFourthQueuesAtDefaultCap(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _ := newPoolFixture(fk)

	for i := 1; i <= 3; i++ {
		if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-"+itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	id, err := r.SubmitPool(context.Background(), "builder", "work", "wi-4", "")
	if err != nil {
		t.Fatalf("queued submit should not error: %v", err)
	}
	if id != "" {
		t.Errorf("4th pool item should queue (empty run id), got %q", id)
	}
	if len(fk.jobs) != 3 {
		t.Errorf("jobs = %d, want 3 (4th queued, not launched)", len(fk.jobs))
	}
	found := false
	for _, p := range poster.posts {
		if strings.Contains(p, "pool dispatch queued") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a queued-notice post, got %v", poster.posts)
	}
}

// A completion frees its slot, and the queued 4th item drains onto the
// freed slot — a released slot is reusable.
func TestSubmitPoolDrainsQueuedOnCompletion(t *testing.T) {
	fk := &fakeK8s{}
	r, _, rec := newPoolFixture(fk)

	for i := 1; i <= 3; i++ {
		if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-"+itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-4", ""); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobs) != 3 {
		t.Fatalf("jobs before drain = %d, want 3", len(fk.jobs))
	}

	// Complete the first leased slot (pool.sub-1's ticket = wi-1). The
	// drain runs synchronously inside OnJobDone, so by the time it
	// returns the freed slot has already been re-leased to the queued
	// wi-4 — checked below.
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true})

	if !r.AgentBusy("pool.sub-2") || !r.AgentBusy("pool.sub-3") {
		t.Error("the other two slots should remain leased")
	}
	if len(fk.jobs) != 4 {
		t.Fatalf("jobs after drain = %d, want 4 (queued wi-4 launched)", len(fk.jobs))
	}
	// The drained item reused the freed slot — a released slot is
	// reusable, not permanently retired.
	if !r.AgentBusy("pool.sub-1") {
		t.Error("the freed pool.sub-1 slot should be re-leased by the drained wi-4")
	}
	if len(rec.starts) != 4 {
		t.Fatalf("run starts = %+v, want 4", rec.starts)
	}
}

// Named-agent dispatch (Submit) still serializes per name AND coexists
// with pool leasing running at the same time.
func TestNamedDispatchCoexistsWithPoolLeasing(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	// Fill the pool.
	for i := 1; i <= 3; i++ {
		if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-"+itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	// A named-agent dispatch runs concurrently, unaffected by pool cap.
	runID, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("named dispatch should not be blocked by a full pool")
	}
	if !r.AgentBusy("anvil") {
		t.Error("anvil should be busy")
	}
	// A second task for anvil still queues (per-name serialization
	// unchanged by pool leasing existing).
	id2, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "" {
		t.Error("second same-agent submit should still queue")
	}
	if len(fk.jobs) != 4 {
		t.Errorf("jobs = %d, want 4 (3 pool + anvil)", len(fk.jobs))
	}

	// Freeing anvil drains its own queue, not the pool's.
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})
	if !r.AgentBusy("anvil") {
		t.Error("anvil should be busy again with the drained NEX-2")
	}
	for _, name := range []string{"pool.sub-1", "pool.sub-2", "pool.sub-3"} {
		if !r.AgentBusy(name) {
			t.Errorf("%s should remain leased — pool untouched by anvil's completion", name)
		}
	}
}

// PoolSize is configurable and distinct from SpawnMaxConcurrent — a
// pool of 1 caps at 1 regardless of the (higher) default hand cap.
func TestSubmitPoolConfigurableSizeDistinctFromSpawnCap(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.PoolSize = 1
	r.SpawnMaxConcurrent = 10 // unrelated dimension — must not affect the pool cap

	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	id, err := r.SubmitPool(context.Background(), "builder", "work", "wi-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Error("second item should queue when PoolSize=1 caps at 1, regardless of SpawnMaxConcurrent")
	}
	if len(fk.jobs) != 1 {
		t.Errorf("jobs = %d, want 1", len(fk.jobs))
	}
}

// Resubmitting an active work item is idempotent (no double-spawn),
// same as Submit's ticket dedupe.
func TestSubmitPoolDedupesActiveWorkItem(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	id1, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("resubmitting the same work item should return the same run id, got %q vs %q", id1, id2)
	}
	if len(fk.jobs) != 1 {
		t.Errorf("jobs = %d, want 1 (no double-spawn)", len(fk.jobs))
	}
}

// The completion summary stamps slot identity, role, and work item for
// accountability (not the builder branch/PR block or hand lineage).
func TestPoolCompletionSummaryStampsAccountability(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _ := newPoolFixture(fk)

	if _, err := r.SubmitPool(context.Background(), "reviewer", "review the diff", "wi-77", ""); err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-77", OK: true})

	got := poster.posts[len(poster.posts)-1]
	for _, want := range []string{"slot=pool.sub-1", "role=reviewer", "work_item=wi-77"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PR:") || strings.Contains(got, "branch:") {
		t.Errorf("pool summary must not carry the builder PR block: %q", got)
	}
}

// Global MaxConc caps pool leasing too (applies on top of poolSize).
func TestSubmitPoolGlobalMaxConcApplies(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.MaxConc = 1

	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	id, err := r.SubmitPool(context.Background(), "builder", "work", "wi-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Error("global MaxConc=1 should queue the second pool item even though the pool itself has room")
	}
	if len(fk.jobs) != 1 {
		t.Errorf("jobs = %d, want 1", len(fk.jobs))
	}
}

func TestSubmitPoolRejections(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	if _, err := r.SubmitPool(context.Background(), "", "work", "wi-1", ""); err == nil {
		t.Error("empty role must be rejected")
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "  ", "wi-1", ""); err == nil {
		t.Error("empty task must be rejected")
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "work", "", ""); err == nil {
		t.Error("empty work item id must be rejected")
	}
	r.MintHandCredential = nil
	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", ""); err == nil {
		t.Error("missing credential minter must be rejected")
	}
	if len(fk.jobs) != 0 {
		t.Errorf("no jobs expected, got %v", fk.jobs)
	}
}

func itoa(n int) string {
	digits := "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}
