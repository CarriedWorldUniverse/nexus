package dispatch_test

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeK8s satisfies the dispatch.K8sIface interface for tests.
type fakeK8s struct {
	jobs    []string
	secrets map[string]bool

	// createErr, when set, fails CreateJob for jobs whose name contains
	// the given substring — used to exercise the launch-failure path.
	createErr func(jobName string) error
	// active is returned by ListActiveJobs — used to exercise recovery.
	active map[string]dispatch.ActiveJob
}

func (f *fakeK8s) EnsureKeyfileSecret(_ context.Context, aspect string) error {
	if f.secrets == nil {
		f.secrets = map[string]bool{}
	}
	f.secrets[aspect] = true
	return nil
}

func (f *fakeK8s) PutBriefConfigMap(_ context.Context, taskID, _ string) error { return nil }

func (f *fakeK8s) CreateJob(_ context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	if f.createErr != nil {
		if err := f.createErr(job.Name); err != nil {
			return nil, err
		}
	}
	f.jobs = append(f.jobs, job.Name)
	return job, nil
}

func (f *fakeK8s) SetBriefOwner(_ context.Context, _ string, _ *batchv1.Job) error { return nil }

func (f *fakeK8s) ListActiveJobs(_ context.Context) (map[string]dispatch.ActiveJob, error) {
	return f.active, nil
}

func (f *fakeK8s) WatchJobs(_ context.Context, _ func(ticket, thread string, ok bool)) error {
	return nil
}

func TestRunnerSubmitUsesPoolSlot(t *testing.T) {
	fk := &fakeK8s{}
	r := &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
		Pool:     []string{"builder-1", "builder-2"},
		MaxConc:  2,
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	b := dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Task: "do work"}
	runID, err := r.Submit(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Error("runID should not be empty")
	}

	// second submit should get builder-2
	b2 := dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "NEX-2", Task: "more work"}
	runID2, err := r.Submit(context.Background(), b2)
	if err != nil {
		t.Fatal(err)
	}
	if runID2 == runID {
		t.Error("each run should get a distinct ID")
	}

	// third should be queued (pool exhausted)
	b3 := dispatch.Brief{Agent: "plumb", Ticket: "NEX-3", Thread: "NEX-3", Task: "plumb work"}
	_, err = r.Submit(context.Background(), b3)
	if err != dispatch.ErrPoolExhausted {
		t.Errorf("expected ErrPoolExhausted when pool full, got %v", err)
	}
}

func TestRunnerOnJobDoneReleasesSlot(t *testing.T) {
	fk := &fakeK8s{}
	r := &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
		Pool:     []string{"builder-1"},
		MaxConc:  1,
	}
	_ = r.Init(context.Background())

	b := dispatch.Brief{Agent: "anvil", Ticket: "NEX-10", Thread: "t1", Task: "work"}
	runID, _ := r.Submit(context.Background(), b)

	// Before done: pool slot occupied
	if r.SlotFree("builder-1") {
		t.Error("builder-1 should be in use")
	}

	r.OnJobDone("NEX-10", "t1", true)

	// After done: pool slot free
	if !r.SlotFree("builder-1") {
		t.Error("builder-1 should be free after job done")
	}
	_ = runID
}

// TestRunnerSubmitDedupesQueuedTicket verifies that re-submitting a ticket
// that's already queued (pool exhausted) is an idempotent no-op rather than
// enqueuing a duplicate that would double-spawn when a slot frees.
func TestRunnerSubmitDedupesQueuedTicket(t *testing.T) {
	fk := &fakeK8s{}
	r := &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
		Pool:     []string{"builder-1"},
		MaxConc:  1,
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Fill the single slot.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"}); err != nil {
		t.Fatal(err)
	}
	// Queue NEX-2.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}); err != dispatch.ErrPoolExhausted {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}
	// Re-submit NEX-2 while queued — should be a no-op (nil error), not a
	// second enqueue.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}); err != nil {
		t.Fatalf("re-submit of queued ticket should be a no-op, got %v", err)
	}

	// Free the slot. Only ONE NEX-2 job should spawn; if the duplicate was
	// enqueued, draining would create two jobs (one now, one still queued).
	before := len(fk.jobs)
	r.OnJobDone("NEX-1", "t", true)
	spawned := len(fk.jobs) - before
	if spawned != 1 {
		t.Errorf("draining queue spawned %d jobs, want exactly 1 (no duplicate)", spawned)
	}
	// builder-1 should be in use again by the drained NEX-2, with nothing
	// left queued.
	if r.SlotFree("builder-1") {
		t.Error("builder-1 should be in use by drained NEX-2")
	}
}

// TestRunnerInitRecoversPoolSlot verifies that a Job recovered on Init
// re-marks its pool slot in use, so a fresh dispatch isn't handed the same
// builder identity.
func TestRunnerInitRecoversPoolSlot(t *testing.T) {
	fk := &fakeK8s{
		active: map[string]dispatch.ActiveJob{
			"NEX-5": {Name: "builder-builder-1-abcd1234", Agent: "anvil", Slot: "builder-1"},
		},
	}
	r := &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
		Pool:     []string{"builder-1", "builder-2"},
		MaxConc:  2,
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The recovered Job holds builder-1 — it must NOT be free.
	if r.SlotFree("builder-1") {
		t.Error("builder-1 should be marked in use after recovery")
	}
	// A new dispatch must land on builder-2, not the occupied builder-1.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "plumb", Ticket: "NEX-6", Thread: "t", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if r.SlotFree("builder-2") {
		t.Error("builder-2 should be taken by the new dispatch")
	}
}

// TestRunnerReEnqueuesOnLaunchError verifies that when launching a queued
// brief fails (transient K8s error), the brief is re-enqueued rather than
// silently dropped, and its slot is released.
func TestRunnerReEnqueuesOnLaunchError(t *testing.T) {
	fk := &fakeK8s{}
	r := &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
		Pool:     []string{"builder-1"},
		MaxConc:  1,
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Occupy the slot with NEX-1, queue NEX-2.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}); err != dispatch.ErrPoolExhausted {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}

	// Make the NEX-2 launch (the drained one) fail on CreateJob.
	fk.createErr = func(name string) error {
		return errors.New("transient k8s error")
	}
	r.OnJobDone("NEX-1", "t", true)

	// NEX-2 failed to launch: its slot must be released (free again) and the
	// brief must still be recoverable on the next drain. Clear the error and
	// drive another drain via a no-op completion to confirm it re-launches.
	if !r.SlotFree("builder-1") {
		t.Error("builder-1 should be free after a failed launch rolled back the reservation")
	}
	fk.createErr = nil
	before := len(fk.jobs)
	// Re-submitting NEX-1 then completing it triggers another drain pass.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"}); err != nil {
		t.Fatal(err)
	}
	r.OnJobDone("NEX-1", "t", true)
	// +2 = the NEX-1 relaunch and the drained NEX-2. If NEX-2 had been
	// dropped on the failed launch, this would be +1.
	if got := len(fk.jobs) - before; got != 2 {
		t.Errorf("expected 2 jobs (NEX-1 relaunch + re-enqueued NEX-2), got %d", got)
	}
}
