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
	// A fixed 3-personality roster keeps the cap tests deterministic
	// (roster size IS the pool cap — one job per personality).
	r.Personalities = []string{"anvil", "plumb", "keel"}
	r.MintHandCredential = func(_ context.Context, _, derived string) (string, error) {
		return "jwt-for-" + derived, nil
	}
	_ = r.Init(context.Background())
	return r, p, rec
}

// N concurrent pool work-items lease N distinct personalities as
// `<personality>-<role>`, in roster order.
func TestSubmitPoolLeasesDistinctPersonalities(t *testing.T) {
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
	for _, want := range []string{"anvil-builder", "plumb-builder", "keel-builder"} {
		if !r.AgentBusy(want) {
			t.Errorf("%s should be leased busy", want)
		}
	}
	if len(fk.jobs) != 3 {
		t.Fatalf("jobs = %v, want 3", fk.jobs)
	}
	_ = ids
}

// One job per personality: a personality already running a job is never
// re-leased, even for a different role — the next free personality is used.
func TestSubmitPoolOneJobPerPersonality(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	if _, err := r.SubmitPool(context.Background(), "builder", "build", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("anvil-builder") {
		t.Fatal("first lease should be anvil-builder")
	}
	// A second work item, different role: anvil is busy, so the next free
	// personality (plumb) is leased — NOT anvil-tester.
	if _, err := r.SubmitPool(context.Background(), "tester", "test", "wi-2", ""); err != nil {
		t.Fatal(err)
	}
	if r.AgentBusy("anvil-tester") {
		t.Error("anvil is already running a job — it must not take a second role")
	}
	if !r.AgentBusy("plumb-tester") {
		t.Error("the tester should lease the next free personality (plumb)")
	}
}

// The (roster+1)th pool work-item queues (empty run id, no error) once
// every personality is busy.
func TestSubmitPoolQueuesAtRosterCap(t *testing.T) {
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

// A completion frees its personality, and the queued item drains onto the
// freed personality — a released personality is reusable.
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

	// Complete the first leased worker (anvil-builder's ticket = wi-1). The
	// drain runs synchronously inside OnJobDone, so by the time it returns
	// the freed personality has already been re-leased to the queued wi-4.
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true})

	if !r.AgentBusy("plumb-builder") || !r.AgentBusy("keel-builder") {
		t.Error("the other two workers should remain leased")
	}
	if len(fk.jobs) != 4 {
		t.Fatalf("jobs after drain = %d, want 4 (queued wi-4 launched)", len(fk.jobs))
	}
	// The drained item reused the freed personality (anvil, first in roster
	// order) — a released personality is reusable, not permanently retired.
	if !r.AgentBusy("anvil-builder") {
		t.Error("the freed anvil personality should be re-leased by the drained wi-4")
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
	runID, err := r.Submit(context.Background(), dispatch.Brief{Agent: "forge", Ticket: "NEX-1", Thread: "t", Task: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("named dispatch should not be blocked by a full pool")
	}
	if !r.AgentBusy("forge") {
		t.Error("forge should be busy")
	}
	// A second task for forge still queues (per-name serialization
	// unchanged by pool leasing existing).
	id2, err := r.Submit(context.Background(), dispatch.Brief{Agent: "forge", Ticket: "NEX-2", Thread: "t", Task: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "" {
		t.Error("second same-agent submit should still queue")
	}
	if len(fk.jobs) != 4 {
		t.Errorf("jobs = %d, want 4 (3 pool + forge)", len(fk.jobs))
	}

	// Freeing forge drains its own queue, not the pool's.
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})
	if !r.AgentBusy("forge") {
		t.Error("forge should be busy again with the drained NEX-2")
	}
	for _, name := range []string{"anvil-builder", "plumb-builder", "keel-builder"} {
		if !r.AgentBusy(name) {
			t.Errorf("%s should remain leased — pool untouched by forge's completion", name)
		}
	}
}

// The roster size is the pool cap, distinct from SpawnMaxConcurrent — a
// single-personality roster caps at 1 regardless of the (higher) hand cap.
func TestSubmitPoolRosterSizeCaps(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.Personalities = []string{"anvil"}
	r.SpawnMaxConcurrent = 10 // unrelated dimension — must not affect the pool cap

	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	id, err := r.SubmitPool(context.Background(), "builder", "work", "wi-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Error("second item should queue when the roster is a single personality, regardless of SpawnMaxConcurrent")
	}
	if len(fk.jobs) != 1 {
		t.Errorf("jobs = %d, want 1", len(fk.jobs))
	}
}

// The pool cap and the kindred-hand fan-out cap are independent dimensions:
// a pool lease of a personality must NOT eat into that personality's own
// SubmitSpawn hand headroom (reviewer-reproduced regression: with
// SpawnMaxConcurrent=1, a pool lease of anvil wrongly queued anvil's first
// kindred hand because liveHands counted the worker against the hand cap).
func TestPoolLeaseDoesNotConsumeKindredHandCap(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.SpawnMaxConcurrent = 1

	// Lease anvil for a pool job.
	if _, err := r.SubmitPool(context.Background(), "builder", "work", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("anvil-builder") {
		t.Fatal("pool lease should be anvil-builder")
	}
	// anvil (a separate live session) fans out ONE kindred hand of its own.
	// Zero kindred hands are live, so it must launch — not queue behind the
	// pool lease.
	handles, err := r.SubmitSpawn(context.Background(), "anvil", "background sweep", 1, "spawn-t")
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].RunID == "" {
		t.Fatalf("anvil's kindred hand should launch immediately (pool lease is a separate cap dimension), got %+v", handles)
	}
	if len(fk.jobs) != 2 {
		t.Errorf("jobs = %d, want 2 (pool worker + kindred hand)", len(fk.jobs))
	}

	// And the reverse: anvil's live kindred hand does not block the pool
	// from leasing anvil? It SHOULD NOT block per-dimension independence —
	// the pool one-job-per-personality check counts only pool workers.
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true}) // free the pool lease
	if _, err := r.SubmitPool(context.Background(), "tester", "test", "wi-2", ""); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("anvil-tester") {
		t.Error("anvil should be pool-leasable while only its kindred hand is live (independent dimensions)")
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

// The completion summary stamps worker identity, role, and work item for
// accountability (not the builder branch/PR block or hand lineage).
func TestPoolCompletionSummaryStampsAccountability(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _ := newPoolFixture(fk)

	if _, err := r.SubmitPool(context.Background(), "reviewer", "review the diff", "wi-77", ""); err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-77", OK: true})

	got := poster.posts[len(poster.posts)-1]
	for _, want := range []string{"worker=anvil-reviewer", "role=reviewer", "work_item=wi-77"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PR:") || strings.Contains(got, "branch:") {
		t.Errorf("pool summary must not carry the builder PR block: %q", got)
	}
}

// Global MaxConc caps pool leasing too (applies on top of the roster cap).
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
		t.Error("global MaxConc=1 should queue the second pool item even though the roster has room")
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
