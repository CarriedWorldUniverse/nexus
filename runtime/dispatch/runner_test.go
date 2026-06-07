package dispatch_test

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeK8s satisfies the dispatch.K8sIface interface for tests.
type fakeK8s struct {
	jobs    []string
	secrets map[string]bool
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
	f.jobs = append(f.jobs, job.Name)
	return job, nil
}

func (f *fakeK8s) SetBriefOwner(_ context.Context, _ string, _ *batchv1.Job) error { return nil }

func (f *fakeK8s) ListActiveJobs(_ context.Context) (map[string]dispatch.ActiveJob, error) {
	return nil, nil
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
