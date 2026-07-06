package dispatch_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

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

// jobEnvValue returns the named env var's value from the builder
// container of a captured Job, or ("", false) if absent.
func jobEnvValue(job *batchv1.Job, name string) (string, bool) {
	if job == nil || len(job.Spec.Template.Spec.Containers) == 0 {
		return "", false
	}
	for _, v := range job.Spec.Template.Spec.Containers[0].Env {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

// TestSubmitPool_JobEnvCarriesRoleWorkItemPersonality is the M1 Unit 5
// regression test for the pool-lease gap: jobspec.go's BuildJob has always
// translated Brief.Role/WorkItemID/Personality into CW_ROLE/CW_WORK_ITEM_ID/
// CW_PERSONALITY, but SubmitPoolItem never stamped Brief.Personality (only
// SpawnParent/Agent) — so CW_PERSONALITY was silently omitted from every
// pool-leased worker's Job env, and the worker.status heartbeat's
// `personality` field was permanently empty for pool workers. Covers both
// the immediate-lease path (SubmitPoolItem) and the queued-drain path
// (reserveQueued in runner.go), which each independently stamp Personality.
func TestSubmitPool_JobEnvCarriesRoleWorkItemPersonality(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	// Immediate lease: personality "anvil" (first free in roster order).
	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-1", ""); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobObjs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(fk.jobObjs))
	}
	job := fk.jobObjs[0]
	for name, want := range map[string]string{
		"CW_ROLE":         "builder",
		"CW_WORK_ITEM_ID": "wi-1",
		"CW_PERSONALITY":  "anvil",
	} {
		if got, ok := jobEnvValue(job, name); !ok || got != want {
			t.Errorf("immediate-lease job env %s = %q (present=%v), want %q", name, got, ok, want)
		}
	}

	// Fill the remaining two slots so a 4th item queues, then complete one
	// to drive the reserveQueued drain path (runner.go), which stamps
	// Personality independently of SubmitPoolItem's immediate-lease path.
	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-3", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-4", ""); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobObjs) != 3 {
		t.Fatalf("jobs before drain = %d, want 3 (wi-4 queued)", len(fk.jobObjs))
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true})
	if len(fk.jobObjs) != 4 {
		t.Fatalf("jobs after drain = %d, want 4 (queued wi-4 launched)", len(fk.jobObjs))
	}
	drained := fk.jobObjs[3]
	for name, want := range map[string]string{
		"CW_ROLE":         "builder",
		"CW_WORK_ITEM_ID": "wi-4",
		"CW_PERSONALITY":  "anvil", // the freed slot, re-leased
	} {
		if got, ok := jobEnvValue(drained, name); !ok || got != want {
			t.Errorf("queued-drain job env %s = %q (present=%v), want %q", name, got, ok, want)
		}
	}
}

// TestSubmitPoolItemThreadsRepo covers the Phase 4 "real REPO tickets" gap:
// PoolItem.Repo must reach the leased Brief and, from there, the Job's
// -repo arg (jobspec.go's builderArgs) exactly like named dispatch's
// Brief.Repo already does — see runtime/dispatch/README.md "The pool
// model" and the orchestrator's dispatchOne (drain.go). Branch is
// deliberately NOT set here: it always falls back to the builder/<ticket>
// convention (ticket == WorkItemID for pool dispatch), unchanged.
func TestSubmitPoolItemThreadsRepo(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	if _, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role:       "builder",
		Task:       "do work",
		WorkItemID: "wi-repo-1",
		Repo:       "CarriedWorldUniverse/nexus",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobObjs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(fk.jobObjs))
	}
	c := fk.jobObjs[0].Spec.Template.Spec.Containers[0]
	if !argValueEquals(c.Args, "-repo", "CarriedWorldUniverse/nexus") {
		t.Errorf("args missing -repo: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-ticket", "wi-repo-1") {
		t.Errorf("args missing -ticket: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-branch", "") {
		t.Errorf("args should carry empty -branch (falls back to builder/<ticket> convention): %v", c.Args)
	}
}

// TestSubmitPoolWithoutRepoReproducesRespondOnlyBehavior: a PoolItem with no
// Repo (the zero value, and every pre-Phase-4 caller) must produce an empty
// -repo arg — no accidental repo/branch/PR-gate activation for respond-only
// pool work.
func TestSubmitPoolWithoutRepoReproducesRespondOnlyBehavior(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-norepo", ""); err != nil {
		t.Fatal(err)
	}
	c := fk.jobObjs[0].Spec.Template.Spec.Containers[0]
	if !argValueEquals(c.Args, "-repo", "") {
		t.Errorf("args should carry empty -repo, got: %v", c.Args)
	}
}

// TestSubmitPoolItem_RequestedPersonalityLeasesExactlyThatOne covers
// per-personality routing (ROLE-MODEL.md "routing a work item to a
// personality"): a PoolItem.Personality request must be leased to exactly
// that personality — never the roster's plain "first free" order — even
// when an earlier-in-roster personality is free too.
func TestSubmitPoolItem_RequestedPersonalityLeasesExactlyThatOne(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	if _, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role: "builder", Task: "do work", WorkItemID: "wi-1", Personality: "keel",
	}); err != nil {
		t.Fatal(err)
	}
	if r.AgentBusy("anvil-builder") {
		t.Error("roster-order personality (anvil) must NOT be leased when a specific personality was requested")
	}
	if !r.AgentBusy("keel-builder") {
		t.Error("requested personality (keel) should be leased")
	}
}

// TestSubmitPoolItem_RequestedPersonalityBusyQueuesNeverSubstitutes: when the
// requested personality is already busy, the item must queue — NOT fall
// back to a different free personality (the request is about the BRAIN
// behind that name; substitution would defeat it).
func TestSubmitPoolItem_RequestedPersonalityBusyQueuesNeverSubstitutes(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _ := newPoolFixture(fk)

	// Occupy keel with a plain (unrequested) lease first.
	if _, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role: "builder", Task: "do work", WorkItemID: "wi-1", Personality: "keel",
	}); err != nil {
		t.Fatal(err)
	}
	if !r.AgentBusy("keel-builder") {
		t.Fatal("keel-builder should be leased")
	}

	// A second item also requests keel: must queue, not lease anvil/plumb.
	id, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role: "builder", Task: "do more work", WorkItemID: "wi-2", Personality: "keel",
	})
	if err != nil {
		t.Fatalf("queued submit should not error: %v", err)
	}
	if id != "" {
		t.Errorf("busy-requested-personality item should queue (empty run id), got %q", id)
	}
	if r.AgentBusy("anvil-builder") || r.AgentBusy("plumb-builder") {
		t.Error("must not substitute a different free personality for a busy request")
	}
	found := false
	for _, p := range poster.posts {
		if strings.Contains(p, "requested personality keel") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a requested-personality-busy queued notice, got %v", poster.posts)
	}

	// Freeing keel drains the queued item onto keel specifically (not
	// whichever personality happens to be first-free).
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true})
	if !r.AgentBusy("keel-builder") {
		t.Error("queued request should drain onto the freed requested personality")
	}
}

// TestSubmitPoolItem_UnknownRequestedPersonalityQueues: a requested
// personality that isn't a roster member can never be leased (there is no
// such worker to run it as) — it queues forever rather than silently
// substituting or erroring, same "never substitute" contract as busy.
func TestSubmitPoolItem_UnknownRequestedPersonalityQueues(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)

	id, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role: "builder", Task: "do work", WorkItemID: "wi-1", Personality: "nonexistent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("unknown-personality request should queue (empty run id), got %q", id)
	}
	if len(fk.jobs) != 0 {
		t.Errorf("jobs = %d, want 0 (nothing leased for an unknown personality)", len(fk.jobs))
	}
}

// TestSubmitPoolItem_InheritsLeasedPersonalityProvider covers the item-4
// finding (per-personality routing docs): a pool lease must stamp the
// leased personality's aspects-row provider onto the Job, mirroring
// SubmitSpawn's parent-provider inheritance (spawn_test.go
// TestSubmitSpawnInheritsParentProvider) — otherwise a claude-code-routed
// personality never gets CLAUDE_CODE_OAUTH_TOKEN injected and silently runs
// as the launch default instead.
func TestSubmitPoolItem_InheritsLeasedPersonalityProvider(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.HandProvider = func(_ context.Context, parent string) string {
		if parent == "keel" {
			return "codex-cli"
		}
		return ""
	}

	if _, err := r.SubmitPoolItem(context.Background(), dispatch.PoolItem{
		Role: "builder", Task: "do work", WorkItemID: "wi-1", Personality: "keel",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobObjs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(fk.jobObjs))
	}
	hasCodexAuth := false
	for _, ic := range fk.jobObjs[0].Spec.Template.Spec.InitContainers {
		if ic.Name == "codex-auth" {
			hasCodexAuth = true
		}
	}
	if !hasCodexAuth {
		t.Error("pool lease should inherit the leased personality's provider (codex-auth init container missing)")
	}
}

// TestSubmitPoolItem_QueuedDrainInheritsLeasedPersonalityProvider: the same
// provider inheritance must apply on the reserveQueued drain path
// (runner.go's launchPending), not just the immediate-lease path in
// SubmitPoolItem — mirrors TestSubmitPool_JobEnvCarriesRoleWorkItemPersonality's
// two-path coverage for CW_PERSONALITY.
func TestSubmitPoolItem_QueuedDrainInheritsLeasedPersonalityProvider(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _ := newPoolFixture(fk)
	r.HandProvider = func(_ context.Context, parent string) string {
		if parent == "anvil" {
			return "codex-cli"
		}
		return ""
	}

	// Fill all three slots (anvil, plumb, keel), then complete anvil so a
	// queued item drains onto the freed anvil slot via reserveQueued.
	for i := 1; i <= 3; i++ {
		if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-"+itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SubmitPool(context.Background(), "builder", "do work", "wi-4", ""); err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: "wi-1", OK: true})
	if len(fk.jobObjs) != 4 {
		t.Fatalf("jobs after drain = %d, want 4", len(fk.jobObjs))
	}
	hasCodexAuth := false
	for _, ic := range fk.jobObjs[3].Spec.Template.Spec.InitContainers {
		if ic.Name == "codex-auth" {
			hasCodexAuth = true
		}
	}
	if !hasCodexAuth {
		t.Error("queued-drain pool lease should also inherit the leased (freed) personality's provider")
	}
}

// argValueEquals mirrors jobspec_test.go's (package dispatch, internal)
// helper of the same name — duplicated here rather than exported since
// this file is package dispatch_test (external).
func argValueEquals(ss []string, key, want string) bool {
	for i := 0; i < len(ss)-1; i++ {
		if ss[i] == key {
			return ss[i+1] == want
		}
	}
	return false
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
