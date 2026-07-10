package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/gates"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// writeFakeGh drops an executable named "gh" (containing script) into dir,
// so a test can put dir first on PATH and have exec.CommandContext("gh",
// ...) run the fake instead of a real gh binary.
func writeFakeGh(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writeFakeGh: %v", err)
	}
}

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
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "do the thing", opts)
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
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "do the thing", opts)

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
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "criteria", opts)
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
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "must have passing tests", opts)
	te, ok := verdictByName(verdicts, GateTestEvidence)
	if !ok || te.State != GateStateFail {
		t.Fatalf("test-evidence = %+v ok=%v; want fail (diff has no _test.go)", te, ok)
	}

	opts2 := seedGates("diff --git a/x_test.go b/x_test.go\n+++ b/x_test.go\n+func TestX(){}\n")
	opts2.RequireTestEvidence = true
	verdicts2 := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "must have passing tests", opts2)
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
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "criteria", opts)
	if verdicts != nil {
		t.Fatalf("disabled runner should return nil verdicts, got %v", verdicts)
	}
}

// TestRunAuthoritativeGatesDefaultsAreBounded proves the review must-fix:
// with NO opts.*Fn overrides (so RunAuthoritativeGates falls back to its own
// default gh-backed implementations), a `gh` subprocess that never returns
// must not hang RunAuthoritativeGates past authGateRPCTimeout. Shrinks
// authGateRPCTimeout to keep the test fast, then execs a real subprocess
// (`sleep 30`, aliased as PATH's "gh") that would otherwise block for far
// longer than the bound — proving the timeout is enforced on the actual
// exec.CommandContext call, not merely plumbed through unused.
func TestRunAuthoritativeGatesDefaultsAreBounded(t *testing.T) {
	origTimeout := authGateRPCTimeout
	authGateRPCTimeout = 200 * time.Millisecond
	defer func() { authGateRPCTimeout = origTimeout }()

	fakeGhDir := t.TempDir()
	// `exec sleep 30` (not a plain `sleep 30`) REPLACES the shell process
	// rather than forking a child — so exec.CommandContext's kill-on-
	// timeout hits the actual sleeping process directly, with no orphaned
	// grandchild left holding the stdout/stderr pipes open (the classic
	// os/exec gotcha: a killed shell whose forked child inherited the
	// pipes leaves Cmd.Wait() blocked on that child's exit, defeating the
	// timeout for the TEST regardless of whether the production code is
	// correctly bounded).
	writeFakeGh(t, fakeGhDir, "#!/bin/sh\nexec sleep 30\n")
	t.Setenv("PATH", fakeGhDir+":"+os.Getenv("PATH"))

	opts := GateRunnerOptions{Enabled: true} // no overrides -> hits defaultPRExistsFn -> the fake gh above

	start := time.Now()
	verdicts := RunAuthoritativeGates(context.Background(), "org/repo", "builder/NET-1", "NET-1", "criteria", opts)
	elapsed := time.Since(start)

	// Generous multiple of the shrunk timeout (not a tight bound) — this is
	// asserting "returned promptly", not measuring exact latency; CI/test
	// scheduling jitter should never flake this.
	if elapsed > 3*time.Second {
		t.Fatalf("RunAuthoritativeGates took %v with a %v gh timeout — the hang was NOT bounded", elapsed, authGateRPCTimeout)
	}

	// The bounded call errors (context deadline exceeded) -> pr-exists
	// reports a fail verdict, not a hang and not a crash.
	exists, ok := verdictByName(verdicts, GatePRExists)
	if !ok || exists.State != GateStateFail {
		t.Fatalf("pr-exists = %+v ok=%v; want fail (bounded gh call timed out)", exists, ok)
	}
}

// TestOnJobDoneHookGateRunnerDark: with Orchestrator.GateRunner nil (the
// package default), runAuthoritativeGates' own nil-pointer guard (wake.go:
// `if o.GateRunner == nil { return }`) short-circuits BEFORE it ever calls
// o.Graph.GetWorkItem — so no gh/judge lookup happens, by construction, not
// by anything this test itself observes (the fake GetWorkItem below would
// happily answer if called; this test does not spy on it). What this test
// DOES assert is the thing that matters externally: job-done intake
// (RecordJobResult -> the item transitions to Done) still runs exactly as
// before #473, proving the dark-default gate wiring changed nothing
// observable about that path.
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
