package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/gates"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// seedGates returns a GateRunnerOptions wired to fake gh lookups, seeded
// with a substantial diff on the run's own branch — the "everything real"
// baseline several tests start from and mutate.
func seedGates(diff string) GateRunnerOptions {
	return GateRunnerOptions{
		Enabled:      true,
		MinDiffLines: 1,
		ExistsFn: func(_ context.Context, repo, branch string) (bool, error) {
			return true, nil
		},
		ExistsByTicketFn: func(_ context.Context, repo, ticket string) (bool, error) {
			return false, nil
		},
		DiffStatsFn: func(_ context.Context, repo, branch string) (gates.PRDiffStats, bool, error) {
			if diff == "" {
				return gates.PRDiffStats{}, true, nil // real PR, empty diff
			}
			return gates.PRDiffStats{Additions: 10, Deletions: 2, ChangedFiles: 1}, true, nil
		},
		DiffStatsByTicketFn: func(_ context.Context, repo, ticket string) (gates.PRDiffStats, bool, error) {
			return gates.PRDiffStats{}, false, errors.New("ticket fallback should not be called")
		},
		DiffFn: func(_ context.Context, repo, branch, ticket string) (string, bool, error) {
			return diff, diff != "", nil
		},
	}
}

func verdictByName(vs []GateVerdict, name string) (GateVerdict, bool) {
	for _, v := range vs {
		if v.Name == name {
			return v, true
		}
	}
	return GateVerdict{}, false
}

// TestRunAuthoritativeGatesAllPass — the happy path: a real PR with a real
// diff produces pass verdicts for pr-exists and pr-substantial. No Verifier
// configured, so acceptance-judge is silent; RequireTestEvidence is off, so
// test-evidence is silent too.
func TestRunAuthoritativeGatesAllPass(t *testing.T) {
	opts := seedGates("diff --git a/x.go b/x.go\n+++ b/x.go\n+func X(){}\n")
	verdicts, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "do the thing", opts)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	exists, ok := verdictByName(verdicts, GatePRExists)
	if !ok || exists.State != GateStatePass {
		t.Fatalf("pr-exists verdict = %+v, ok=%v; want pass", exists, ok)
	}
	sub, ok := verdictByName(verdicts, GatePRSubstantial)
	if !ok || sub.State != GateStatePass {
		t.Fatalf("pr-substantial verdict = %+v, ok=%v; want pass", sub, ok)
	}
	if _, ok := verdictByName(verdicts, GateAcceptanceJudge); ok {
		t.Fatal("acceptance-judge should not run without a configured Verifier")
	}
	if _, ok := verdictByName(verdicts, GateTestEvidence); ok {
		t.Fatal("test-evidence should not run when RequireTestEvidence is off")
	}
}

// TestRunAuthoritativeGatesGroundTruthNotClaim is THE key test: a scenario
// where the WORKER'S OWN advisory gate would have passed (a PR exists —
// prExists is satisfied) but the ARTIFACT itself is empty (no real diff).
// RunAuthoritativeGates must fail pr-substantial — proving it reads the
// pushed artifact (ground truth), not a worker's claim, which is exactly
// what a forged/confabulated task_done cannot fake.
func TestRunAuthoritativeGatesGroundTruthNotClaim(t *testing.T) {
	// diff="" seeds a REAL PR (exists=true, found=true) whose diff is
	// empty — the "worker opened an empty/token PR and narrated success"
	// failure mode ACCEPTANCE-GATE-HARDENING.md Unit 2 exists to catch.
	opts := seedGates("")
	verdicts, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "do the thing", opts)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	exists, ok := verdictByName(verdicts, GatePRExists)
	if !ok || exists.State != GateStatePass {
		t.Fatalf("pr-exists should still pass (a PR really exists): %+v ok=%v", exists, ok)
	}

	sub, ok := verdictByName(verdicts, GatePRSubstantial)
	if !ok {
		t.Fatal("pr-substantial should have run (pr-exists passed)")
	}
	if sub.State != GateStateFail {
		t.Fatalf("pr-substantial must FAIL on an empty diff — ground truth, not a worker claim; got %+v", sub)
	}
}

// TestRunAuthoritativeGatesPRSubstantialSkippedWhenNoPR: pr-substantial only
// runs after pr-exists passes (matches the #468 pull-checks "recorded when"
// table) — no PR at all emits only a failed pr-exists verdict.
func TestRunAuthoritativeGatesPRSubstantialSkippedWhenNoPR(t *testing.T) {
	opts := GateRunnerOptions{
		Enabled:      true,
		MinDiffLines: 1,
		ExistsFn:     func(context.Context, string, string) (bool, error) { return false, nil },
		ExistsByTicketFn: func(context.Context, string, string) (bool, error) {
			return false, nil
		},
		DiffStatsFn: func(context.Context, string, string) (gates.PRDiffStats, bool, error) {
			t.Fatal("pr-substantial's diff-stats lookup should not run when pr-exists failed")
			return gates.PRDiffStats{}, false, nil
		},
		DiffFn: func(context.Context, string, string, string) (string, bool, error) { return "", false, nil },
	}
	verdicts, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "criteria", opts)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	exists, ok := verdictByName(verdicts, GatePRExists)
	if !ok || exists.State != GateStateFail {
		t.Fatalf("pr-exists = %+v ok=%v; want fail", exists, ok)
	}
	if _, ok := verdictByName(verdicts, GatePRSubstantial); ok {
		t.Fatal("pr-substantial should not have run")
	}
}

// TestRunAuthoritativeGatesTestEvidence: RequireTestEvidence + criteria
// mentioning tests + a real diff that touches no _test.go file -> fail.
func TestRunAuthoritativeGatesTestEvidence(t *testing.T) {
	opts := seedGates("diff --git a/x.go b/x.go\n+++ b/x.go\n+func X(){}\n")
	opts.RequireTestEvidence = true
	verdicts, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "must have passing tests", opts)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	te, ok := verdictByName(verdicts, GateTestEvidence)
	if !ok || te.State != GateStateFail {
		t.Fatalf("test-evidence = %+v ok=%v; want fail (diff has no _test.go)", te, ok)
	}

	opts2 := seedGates("diff --git a/x_test.go b/x_test.go\n+++ b/x_test.go\n+func TestX(){}\n")
	opts2.RequireTestEvidence = true
	verdicts2, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "must have passing tests", opts2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	te2, ok := verdictByName(verdicts2, GateTestEvidence)
	if !ok || te2.State != GateStatePass {
		t.Fatalf("test-evidence = %+v ok=%v; want pass (diff touches a _test.go file)", te2, ok)
	}
}

// TestRunAuthoritativeGatesDarkWhenDisabled: opts.Enabled=false (the zero
// value) must make ZERO gh/judge calls — asserted by fatal-ing inside every
// injected fetch function.
func TestRunAuthoritativeGatesDarkWhenDisabled(t *testing.T) {
	fail := func(string) { t.Fatal("should not be called: gate runner is disabled") }
	opts := GateRunnerOptions{
		Enabled: false,
		ExistsFn: func(context.Context, string, string) (bool, error) {
			fail("ExistsFn")
			return false, nil
		},
		ExistsByTicketFn: func(context.Context, string, string) (bool, error) {
			fail("ExistsByTicketFn")
			return false, nil
		},
		DiffStatsFn: func(context.Context, string, string) (gates.PRDiffStats, bool, error) {
			fail("DiffStatsFn")
			return gates.PRDiffStats{}, false, nil
		},
		DiffFn: func(context.Context, string, string, string) (string, bool, error) {
			fail("DiffFn")
			return "", false, nil
		},
	}
	verdicts, err := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "criteria", opts)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if verdicts != nil {
		t.Fatalf("disabled runner should return nil verdicts, got %v", verdicts)
	}
}

// TestOnJobDoneHookGateRunnerDark: with Orchestrator.GateRunner nil (the
// package default), OnJobDoneHook must never call GetWorkItem's gate-running
// path with a live gh/judge lookup — asserted by a WorkGraph fake whose
// GetWorkItem would fatal if called from the gate path with real repo data,
// combined with confirming the job-done intake (RecordJobResult) still ran
// exactly as before #473.
func TestOnJobDoneHookGateRunnerDark(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("wi-1", workgraph.WorkItem{Role: "builder", TaskSpec: "build", Repo: "org/repo"})
	graph.items["wi-1"].status = workgraph.StatusDispatched
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
		// GateRunner intentionally left nil.
	}

	hook := o.OnJobDoneHook()
	hook(dispatch.JobDone{Ticket: "wi-1", OK: true})

	if graph.items["wi-1"].status != workgraph.StatusDone {
		t.Errorf("wi-1 status = %v, want done — job-done intake must be unaffected by #473's dark-default gate wiring",
			graph.items["wi-1"].status)
	}
}

// TestOnJobDoneHookGateRunnerRuns: with a live GateRunner, OnJobDoneHook
// looks up the work item and runs the gates — proven by a fetch func that
// records it was called with the expected repo/branch/ticket.
func TestOnJobDoneHookGateRunnerRuns(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("NET-9", workgraph.WorkItem{
		Role: "builder", TaskSpec: "build", Repo: "org/repo",
		AcceptanceCriteria: []string{"do the thing"},
	})
	graph.items["NET-9"].status = workgraph.StatusDispatched
	disp := &fakeDispatcher{}

	var calledRepo, calledBranch string
	opts := &GateRunnerOptions{
		Enabled: true,
		ExistsFn: func(_ context.Context, repo, branch string) (bool, error) {
			calledRepo, calledBranch = repo, branch
			return true, nil
		},
		ExistsByTicketFn: func(context.Context, string, string) (bool, error) { return false, nil },
		DiffStatsFn: func(context.Context, string, string) (gates.PRDiffStats, bool, error) {
			return gates.PRDiffStats{Additions: 5, Deletions: 0, ChangedFiles: 1}, true, nil
		},
		DiffFn: func(context.Context, string, string, string) (string, bool, error) { return "", false, nil },
	}

	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
		GateRunner:   opts,
	}

	hook := o.OnJobDoneHook()
	hook(dispatch.JobDone{Ticket: "NET-9", OK: true})

	if calledRepo != "org/repo" || calledBranch != "builder/NET-9" {
		t.Fatalf("gate ExistsFn called with repo=%q branch=%q, want org/repo, builder/NET-9", calledRepo, calledBranch)
	}
}
