package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeK8s satisfies the dispatch.K8sIface interface for tests.
type fakeK8s struct {
	jobs    []string
	secrets map[string]bool
	owners  []briefOwnerCall

	// createErr, when set, fails CreateJob — used to exercise the
	// launch-failure / re-enqueue path.
	createErr func(jobName string) error
	// active is returned by ListActiveJobs — used to exercise recovery.
	active map[string]dispatch.ActiveJob
}

type briefOwnerCall struct {
	taskID string
	job    string
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

func (f *fakeK8s) SetBriefOwner(_ context.Context, taskID string, job *batchv1.Job) error {
	f.owners = append(f.owners, briefOwnerCall{taskID: taskID, job: job.Name})
	return nil
}

func (f *fakeK8s) ListActiveJobs(_ context.Context) (map[string]dispatch.ActiveJob, error) {
	return f.active, nil
}

func (f *fakeK8s) WatchJobs(_ context.Context, _ func(dispatch.JobDone)) error {
	return nil
}

func newRunner(fk *fakeK8s) *dispatch.Runner {
	return &dispatch.Runner{
		K8sIface: fk,
		Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
	}
}

// The worker runs AS the named agent: keyfile = aspect-keyfile-<agent>, job
// named builder-<agent>-<run>, and the agent is marked busy.
func TestRunnerRunsAsNamedAgent(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	runID, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Task: "do work"})
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Error("runID should not be empty")
	}
	if !r.AgentBusy("anvil") {
		t.Error("anvil should be busy after submit")
	}
	if !fk.secrets["anvil"] {
		t.Error("worker must mount the named agent's keyfile (aspect-keyfile-anvil)")
	}
	if len(fk.jobs) != 1 || !strings.HasPrefix(fk.jobs[0], "builder-anvil-") {
		t.Errorf("job should be named builder-anvil-*, got %v", fk.jobs)
	}
}

func TestRunnerDefaultRunIDsAreUUIDBackedAndDistinct(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	id1, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Submit(context.Background(), dispatch.Brief{Agent: "plumb", Ticket: "NEX-2", Thread: "t", Task: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("run IDs should be distinct, both were %q", id1)
	}
	re := regexp.MustCompile(`^run-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !re.MatchString(id1) || !re.MatchString(id2) {
		t.Fatalf("run IDs should be run-<uuid>, got %q and %q", id1, id2)
	}
}

func TestRunnerSetsBriefOwnerToCreatedJob(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	runID, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fk.owners) != 1 {
		t.Fatalf("SetBriefOwner calls = %d, want 1", len(fk.owners))
	}
	if fk.owners[0].taskID != runID {
		t.Fatalf("brief owner taskID = %q, want run ID %q", fk.owners[0].taskID, runID)
	}
	if fk.owners[0].job != fk.jobs[0] {
		t.Fatalf("brief owner job = %q, want created job %q", fk.owners[0].job, fk.jobs[0])
	}
}

// One run per agent name at a time (NEX-464); different agents run in parallel.
func TestRunnerSerializesPerAgentConcurrentAcrossAgents(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	_ = r.Init(context.Background())

	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"}); err != nil {
		t.Fatal(err)
	}
	// Second task for the SAME agent → queued (empty runID, nil error), no 2nd job.
	id2, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"})
	if err != nil {
		t.Fatalf("queued submit should not error, got %v", err)
	}
	if id2 != "" {
		t.Errorf("second same-agent submit should queue (empty runID), got %q", id2)
	}
	// A DIFFERENT agent runs concurrently.
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "plumb", Ticket: "NEX-3", Thread: "t", Task: "c"}); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("plumb") {
		t.Error("plumb should run concurrently with anvil")
	}
	if len(fk.jobs) != 2 {
		t.Errorf("expected 2 jobs (anvil + plumb; anvil's NEX-2 queued), got %d", len(fk.jobs))
	}
}

// OnJobDone frees the agent and drains a queued same-agent task onto it.
func TestRunnerOnJobDoneFreesAgentAndDrains(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	_ = r.Init(context.Background())

	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}) // queued
	if !r.AgentBusy("anvil") {
		t.Fatal("anvil should be busy")
	}
	before := len(fk.jobs)

	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})

	if got := len(fk.jobs) - before; got != 1 {
		t.Errorf("expected 1 drained job for the queued NEX-2, got %d", got)
	}
	if !r.AgentBusy("anvil") {
		t.Error("anvil should be busy again with the drained NEX-2")
	}
}

// Re-submitting an already-queued ticket is a no-op (no double-spawn on drain).
func TestRunnerDedupesQueuedTicket(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	_ = r.Init(context.Background())

	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}); err != nil {
		t.Fatal(err)
	}

	before := len(fk.jobs)
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})
	if got := len(fk.jobs) - before; got != 1 {
		t.Errorf("draining spawned %d jobs, want exactly 1 (no duplicate)", got)
	}
}

// A recovered Job re-marks its agent busy so a fresh same-agent dispatch can't
// double-run the identity; a different agent still runs.
func TestRunnerInitRecoversAgentBusy(t *testing.T) {
	fk := &fakeK8s{
		active: map[string]dispatch.ActiveJob{
			"NEX-5": {Name: "builder-anvil-abcd1234", Agent: "anvil"},
		},
	}
	r := newRunner(fk)
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !r.AgentBusy("anvil") {
		t.Error("anvil should be busy after recovery")
	}
	id, _ := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-6", Thread: "t", Task: "x"})
	if id != "" {
		t.Error("a fresh anvil dispatch should queue while the recovered anvil job runs")
	}
	if _, err := r.Submit(context.Background(), dispatch.Brief{Agent: "plumb", Ticket: "NEX-7", Thread: "t", Task: "y"}); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("plumb") {
		t.Error("plumb should run despite anvil being busy")
	}
}

// A drained brief that fails to launch is re-enqueued (not dropped) and its
// agent freed; a later completion re-drains it successfully.
func TestRunnerReEnqueuesOnLaunchError(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	_ = r.Init(context.Background())

	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "t", Task: "b"}) // queued

	fk.createErr = func(string) error { return errors.New("transient k8s error") }
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})
	if r.AgentBusy("anvil") {
		t.Error("anvil should be free after the drained NEX-2 launch failed + rolled back")
	}

	fk.createErr = nil
	_, _ = r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", Task: "a"})
	before := len(fk.jobs)
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "t", OK: true})
	if got := len(fk.jobs) - before; got != 1 {
		t.Errorf("expected the re-enqueued NEX-2 to launch (1 job), got %d", got)
	}
}

type recordingPoster struct {
	posts []string
}

func (p *recordingPoster) Post(_, text string) error {
	p.posts = append(p.posts, text)
	return nil
}

func TestRunnerPostsCompletionSummary(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	poster := &recordingPoster{}
	r.Poster = poster
	r.Cfg.ActivityDir = t.TempDir()
	_ = r.Init(context.Background())

	start := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	writeTurnFrame(t, r.Cfg.ActivityDir, "anvil", start.Add(time.Minute), observability.TurnComplete, "")

	t.Cleanup(dispatch.SetLookupPRURLForTest(func(repo, branch string) (string, error) {
		if repo != "CarriedWorldUniverse/nexus" || branch != "builder/NEX-1" {
			t.Fatalf("lookup args repo=%q branch=%q", repo, branch)
		}
		return "https://github.com/CarriedWorldUniverse/nexus/pull/123", nil
	}))

	_, err := r.Submit(context.Background(), dispatch.Brief{
		Agent: "anvil", Ticket: "NEX-1", Thread: "thread", Repo: "CarriedWorldUniverse/nexus", Task: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "thread", Agent: "anvil", OK: true, StartedAt: start, CompletedAt: end})

	got := poster.posts[len(poster.posts)-1]
	for _, want := range []string{
		"builder done: NEX-1",
		"branch: builder/NEX-1",
		"duration: 2m0s",
		"turns: 1",
		"PR: https://github.com/CarriedWorldUniverse/nexus/pull/123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in:\n%s", want, got)
		}
	}
}

func TestRunnerCompletionSummaryFailSoftNoPR(t *testing.T) {
	fk := &fakeK8s{}
	r := newRunner(fk)
	poster := &recordingPoster{}
	r.Poster = poster
	_ = r.Init(context.Background())

	t.Cleanup(dispatch.SetLookupPRURLForTest(func(string, string) (string, error) {
		return "", errors.New("gh unavailable")
	}))

	_, err := r.Submit(context.Background(), dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "thread", Task: "a"})
	if err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "NEX-1", Thread: "thread", Agent: "anvil", OK: false})

	got := poster.posts[len(poster.posts)-1]
	for _, want := range []string{
		"builder failed: NEX-1",
		"branch: builder/NEX-1",
		"PR: not resolved",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in:\n%s", want, got)
		}
	}
}

func writeTurnFrame(t *testing.T, root, aspect string, ts time.Time, status observability.TurnStatus, label string) {
	t.Helper()
	dir := filepath.Join(root, aspect)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(observability.TurnFrame{Status: status, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	frame := observability.Frame{
		Kind:    observability.FrameTurn,
		Aspect:  aspect,
		TS:      ts,
		Payload: payload,
	}
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ts.Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
