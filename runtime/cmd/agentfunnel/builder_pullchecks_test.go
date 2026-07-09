package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	cairnv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/cairn/v1"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/runtime/pullchecks"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakePullServer is a minimal in-process cairnv1.PullServiceServer used to
// prove the agentfunnel-side gate wiring (builderPRVerifier,
// builderAcceptanceGate.Decide) records checks against a real
// pullchecks.Recorder without needing a live cairn-server.
type fakePullServer struct {
	cairnv1.UnimplementedPullServiceServer

	mu            sync.Mutex
	openPullCalls int
	recordCalls   []*cairnv1.RecordPullCheckRequest

	openErr   error
	recordErr error

	// openErrFn, when set, overrides openErr per call (given the 1-based
	// call number) — lets a test simulate "fails the first N times, then
	// succeeds" (review finding #1's retry-after-failure case: the run's
	// branch/PR doesn't exist yet on the first EnsurePull).
	openErrFn func(callNum int) error

	// blockOpen/blockRecord, when true, make the corresponding RPC hang
	// until the caller's context is canceled — simulating an
	// unreachable/hung cairn-server, to prove the RPC-timeout wrapper
	// (review finding #2) actually bounds the wait.
	blockOpen   bool
	blockRecord bool
}

func (f *fakePullServer) OpenPull(ctx context.Context, req *cairnv1.OpenPullRequest) (*cairnv1.OpenPullResponse, error) {
	f.mu.Lock()
	f.openPullCalls++
	callNum := f.openPullCalls
	errFn := f.openErrFn
	staticErr := f.openErr
	block := f.blockOpen
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if errFn != nil {
		if err := errFn(callNum); err != nil {
			return nil, err
		}
	} else if staticErr != nil {
		return nil, staticErr
	}
	return &cairnv1.OpenPullResponse{Pull: &cairnv1.Pull{Id: "pull-1", Repo: req.Slug, Source: req.Source, Target: req.Target}}, nil
}

func (f *fakePullServer) RecordPullCheck(ctx context.Context, req *cairnv1.RecordPullCheckRequest) (*cairnv1.RecordPullCheckResponse, error) {
	f.mu.Lock()
	block := f.blockRecord
	err := f.recordErr
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	f.recordCalls = append(f.recordCalls, req)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &cairnv1.RecordPullCheckResponse{Check: &cairnv1.PullCheck{Id: "check-1", PullId: req.Id, Name: req.Name, State: req.State}}, nil
}

func (f *fakePullServer) snapshot() (openCalls int, records []*cairnv1.RecordPullCheckRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPullCalls, append([]*cairnv1.RecordPullCheckRequest(nil), f.recordCalls...)
}

// dialFakePullServer starts fake over bufconn and returns a *pullRunRecorder
// wired to it. t.Cleanup tears the server/conn down.
func dialFakePullServer(t *testing.T, fake *fakePullServer) *pullRunRecorder {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	cairnv1.RegisterPullServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	rec := pullchecks.New(conn, "org-1", "widgets", "PROJ", slog.Default())
	return &pullRunRecorder{rec: rec}
}

// TestPullCheckDarkByDefault is the agentfunnel-side half of the DARK
// DEFAULT acceptance proof: with the package-level pullCheckRun left at its
// zero value (nil — exactly as every existing test in this package leaves
// it), the PR-exists/PR-substantial gate makes ZERO PullService calls. A
// fake server configured to fail the test on ANY call would only ever be
// reached if pullCheckRun were non-nil, so simply never wiring one (as this
// test does) is itself the proof: the gate's return value and prExistsFn
// call count are unaffected by this file's existence.
func TestPullCheckDarkByDefault(t *testing.T) {
	if pullCheckRun != nil {
		t.Fatalf("pullCheckRun = %v, want nil (no test in this package should set it as a package-level default)", pullCheckRun)
	}
	origExists, origStats, origURL := prExistsFn, prDiffStatsFn, prURLFn
	defer func() { prExistsFn, prDiffStatsFn, prURLFn = origExists, origStats, origURL }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}
	// review finding (2nd pass): prURLBestEffort shells out via prURLFn
	// (`gh pr list`) with no timeout — it must NEVER run when pull-checks is
	// dark, not even to compute an evidence_url that a nil-recorder guard
	// deeper in the call chain would just discard (Go evaluates a call's
	// arguments before the callee's own nil check ever runs).
	urlCalls := 0
	prURLFn = func(context.Context, string, string) (string, error) {
		urlCalls++
		return "https://should-not-be-called.example", nil
	}

	got := builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")()
	if !got {
		t.Fatal("builderPRVerifier() = false, want true (PR gate itself unaffected by pull-checks being dark)")
	}
	if pullCheckRun != nil {
		t.Fatal("pullCheckRun became non-nil as a side effect of running the gate — dark default violated")
	}
	if urlCalls != 0 {
		t.Fatalf("prURLFn was called %d times with pull-checks dark, want 0 (eager prURLBestEffort regression)", urlCalls)
	}
}

// TestPullCheckWiringRecordsPRGateVerdicts proves builderPRVerifier records
// pr-exists and pr-substantial against the SAME pull (EnsurePull idempotency
// exercised via two ensurePull calls under the hood — one per check) when a
// recorder IS configured.
func TestPullCheckWiringRecordsPRGateVerdicts(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}
	origURL := prURLFn
	defer func() { prURLFn = origURL }()
	prURLFn = func(context.Context, string, string) (string, error) {
		return "https://cairn.example/org-1/widgets/pulls/pull-1", nil
	}

	fake := &fakePullServer{}
	run := dialFakePullServer(t, fake)
	origRun := pullCheckRun
	pullCheckRun = run
	defer func() { pullCheckRun = origRun }()

	got := builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")()
	if !got {
		t.Fatal("builderPRVerifier() = false, want true")
	}

	openCalls, records := fake.snapshot()
	if openCalls != 1 {
		t.Fatalf("OpenPull calls = %d, want 1 (both checks share one ensured pull)", openCalls)
	}
	if len(records) != 2 {
		t.Fatalf("RecordPullCheck calls = %d, want 2 (pr-exists + pr-substantial)", len(records))
	}
	byName := map[string]*cairnv1.RecordPullCheckRequest{}
	for _, r := range records {
		byName[r.Name] = r
	}
	exists := byName["pr-exists"]
	if exists == nil || exists.State != pullchecks.StatePass || exists.Id != "pull-1" {
		t.Fatalf("pr-exists check = %+v, want state=pass id=pull-1", exists)
	}
	if exists.EvidenceUrl == "" {
		t.Error("pr-exists check has no evidence_url, want the PR URL")
	}
	substantial := byName["pr-substantial"]
	if substantial == nil || substantial.State != pullchecks.StatePass {
		t.Fatalf("pr-substantial check = %+v, want state=pass", substantial)
	}
}

// TestPullCheckWiringRecordsAcceptanceJudgeVerdict proves
// builderAcceptanceGate.Decide records the acceptance-judge check, and only
// records test-evidence when Unit 3 was actually active.
func TestPullCheckWiringRecordsAcceptanceJudgeVerdict(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origDiff := prDiffFn
	defer func() { prDiffFn = origDiff }()
	t.Setenv("ACCEPTANCE_JUDGE_DIFF", "0")
	t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "0") // Unit 3 inactive for this test
	prDiffFn = func(string, string, string) (string, bool, error) { return "", false, nil }

	fake := &fakePullServer{}
	run := dialFakePullServer(t, fake)

	verify := func(_ context.Context, _, _ string) (funnel.AcceptanceVerdict, error) {
		return funnel.AcceptanceVerdict{Met: true, Reason: "all criteria satisfied"}, nil
	}
	gate := newBuilderAcceptanceGate("implement X", verify)
	gate.repo, gate.branch, gate.ticket = "org/repo", "", "NET-1"
	gate.pullCheck = run

	step, verdict := gate.Decide(context.Background(), "done", slog.Default())
	if !verdict.Met || step != taskDoneHonor {
		t.Fatalf("Decide = (%v, %+v), want met/taskDoneHonor", step, verdict)
	}

	_, records := fake.snapshot()
	if len(records) != 1 {
		t.Fatalf("RecordPullCheck calls = %d, want 1 (acceptance-judge only — Unit 3 inactive)", len(records))
	}
	if records[0].Name != "acceptance-judge" || records[0].State != pullchecks.StatePass {
		t.Fatalf("check = %+v, want name=acceptance-judge state=pass", records[0])
	}
	if records[0].Summary != "all criteria satisfied" {
		t.Errorf("summary = %q, want the judge's reason", records[0].Summary)
	}
}

// TestPullCheckWiringRecordsTestEvidenceWhenActive proves test-evidence IS
// recorded once Unit 3 (ACCEPTANCE_REQUIRE_TEST_DIFF=1) is active.
func TestPullCheckWiringRecordsTestEvidenceWhenActive(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origDiff := prDiffFn
	defer func() { prDiffFn = origDiff }()
	t.Setenv("ACCEPTANCE_JUDGE_DIFF", "0")
	t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "1")
	prDiffFn = func(string, string, string) (string, bool, error) {
		return "diff --git a/x.go b/x.go\n+++ b/x.go\n+code\n", true, nil // no test file
	}

	fake := &fakePullServer{}
	run := dialFakePullServer(t, fake)

	verify := func(_ context.Context, _, _ string) (funnel.AcceptanceVerdict, error) {
		return funnel.AcceptanceVerdict{Met: true, Reason: "judge says done"}, nil
	}
	gate := newBuilderAcceptanceGate("implement X with passing tests", verify)
	gate.repo, gate.ticket = "org/repo", "NET-1"
	gate.pullCheck = run

	_, verdict := gate.Decide(context.Background(), "done", slog.Default())
	if verdict.Met {
		t.Fatal("Unit 3 should have overridden met to false")
	}

	_, records := fake.snapshot()
	byName := map[string]*cairnv1.RecordPullCheckRequest{}
	for _, r := range records {
		byName[r.Name] = r
	}
	te := byName["test-evidence"]
	if te == nil {
		t.Fatal("no test-evidence check recorded, want one (Unit 3 was active)")
	}
	if te.State != pullchecks.StateFail {
		t.Errorf("test-evidence state = %q, want fail", te.State)
	}
	judge := byName["acceptance-judge"]
	if judge == nil || judge.State != pullchecks.StateFail {
		t.Fatalf("acceptance-judge check = %+v, want state=fail (overridden by Unit 3)", judge)
	}
}

// TestPullCheckWiringFailurePolicy proves the failure-policy contract at the
// wiring layer: a PullService outage never changes the gate's own pass/fail
// return value — only the recording side-effect is lost (and logged).
func TestPullCheckWiringFailurePolicy(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}

	fake := &fakePullServer{openErr: errors.New("cairn-server unavailable")}
	run := dialFakePullServer(t, fake)
	origRun := pullCheckRun
	pullCheckRun = run
	defer func() { pullCheckRun = origRun }()

	got := builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")()
	if !got {
		t.Fatal("builderPRVerifier() = false, want true — a pull-checks outage must never change the gate's own verdict")
	}
}

// TestEnsurePullRetriesAfterFailure is the regression test for review
// finding #1: the acceptance-judge gate's first call routinely lands BEFORE
// the run's branch/PR exists (the exact case the gate exists to catch), so
// the first EnsurePull legitimately 404s. That must NOT permanently latch
// "ensured" — a later call, once the branch/PR is real, has to retry
// EnsurePull and land its check, not silently no-op for the rest of the run.
func TestEnsurePullRetriesAfterFailure(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	fake := &fakePullServer{
		openErrFn: func(callNum int) error {
			if callNum == 1 {
				return errors.New("source branch not found yet")
			}
			return nil
		},
	}
	run := dialFakePullServer(t, fake)
	ctx := context.Background()
	log := slog.Default()

	// First attempt: branch/PR doesn't exist yet — EnsurePull fails.
	id1, ok1 := run.ensurePull(ctx, log, "org/repo", "", "NET-1")
	if ok1 || id1 != "" {
		t.Fatalf("first ensurePull = (%q, %v), want (\"\", false)", id1, ok1)
	}
	run.mu.Lock()
	stillUnensured := !run.ensured
	run.mu.Unlock()
	if !stillUnensured {
		t.Fatal("ensured latched true after a FAILED EnsurePull — a later gate can never retry (finding #1 regression)")
	}

	// Second attempt (e.g. the PR-exists gate, once the branch/PR is real):
	// must retry EnsurePull, not reuse a cached empty id.
	id2, ok2 := run.ensurePull(ctx, log, "org/repo", "", "NET-1")
	if !ok2 || id2 == "" {
		t.Fatalf("second ensurePull = (%q, %v), want a real pull id now that OpenPull succeeds", id2, ok2)
	}
	run.mu.Lock()
	nowEnsured := run.ensured
	run.mu.Unlock()
	if !nowEnsured {
		t.Fatal("ensured not latched true after a SUCCESSFUL EnsurePull")
	}

	openCalls, _ := fake.snapshot()
	if openCalls != 2 {
		t.Fatalf("OpenPull calls = %d, want 2 (one failed attempt + one retry)", openCalls)
	}

	// And the check actually lands now that the pull is ensured.
	run.record(ctx, log, "org/repo", "", "NET-1", pullCheckPRExists, pullchecks.StatePass, "PR found", "")
	_, records := fake.snapshot()
	if len(records) != 1 || records[0].Id != id2 {
		t.Fatalf("records = %+v, want one check landed against pull id %q", records, id2)
	}
}

// TestPullRunRecorderRecordRespectsRPCTimeout is the regression test for
// review finding #2: a blocked/unreachable cairn-server must not stall
// record() beyond pullCheckRPCTimeout.
func TestPullRunRecorderRecordRespectsRPCTimeout(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origTimeout := pullCheckRPCTimeout
	pullCheckRPCTimeout = 200 * time.Millisecond
	defer func() { pullCheckRPCTimeout = origTimeout }()

	fake := &fakePullServer{blockOpen: true}
	run := dialFakePullServer(t, fake)

	done := make(chan struct{})
	go func() {
		run.record(context.Background(), slog.Default(), "org/repo", "", "NET-1",
			pullCheckPRExists, pullchecks.StatePass, "s", "")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("record() did not return within the RPC timeout — a blocked cairn-server can stall a gate")
	}
}

// TestPullCheckWiringBlockedServerDoesNotStallGate is the wiring-layer half
// of finding #2's acceptance test: a blocked pull-checks server must not
// change builderPRVerifier's own timing-sensitive return value, and the call
// must complete within (a small multiple of) pullCheckRPCTimeout.
func TestPullCheckWiringBlockedServerDoesNotStallGate(t *testing.T) {
	t.Setenv("CW_PULL_TARGET", "main") // skip the gh default-branch lookup in tests
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}

	origTimeout := pullCheckRPCTimeout
	pullCheckRPCTimeout = 200 * time.Millisecond
	defer func() { pullCheckRPCTimeout = origTimeout }()

	fake := &fakePullServer{blockOpen: true}
	run := dialFakePullServer(t, fake)
	origRun := pullCheckRun
	pullCheckRun = run
	defer func() { pullCheckRun = origRun }()

	done := make(chan bool)
	start := time.Now()
	go func() { done <- builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")() }()

	select {
	case got := <-done:
		if !got {
			t.Fatal("builderPRVerifier() = false, want true — a blocked pull-checks server must never change the gate's own verdict")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("builderPRVerifier() took %v, want bounded by pullCheckRPCTimeout (server was blocked)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("builderPRVerifier() did not return — a blocked pull-checks server stalled the gate")
	}
}

// TestDecideDarkDefaultNeverCallsPrURL is the acceptance-judge-path half of
// the eager-prURLBestEffort regression (2nd review pass): with gate.pullCheck
// left nil (dark), Decide's evidenceURL computation must not shell out via
// prURLFn at all — pre-fix it did, on every acceptance-judge evaluation,
// even with no recorder configured (the pre-existing TestDecideUnit3OverridesMet
// exercised exactly this without ever noticing, since it never asserted on
// prURLFn).
func TestDecideDarkDefaultNeverCallsPrURL(t *testing.T) {
	origURL := prURLFn
	defer func() { prURLFn = origURL }()
	urlCalls := 0
	prURLFn = func(context.Context, string, string) (string, error) {
		urlCalls++
		return "https://should-not-be-called.example", nil
	}

	verify := func(_ context.Context, _, _ string) (funnel.AcceptanceVerdict, error) {
		return funnel.AcceptanceVerdict{Met: true, Reason: "judge says done"}, nil
	}
	gate := newBuilderAcceptanceGate("implement X", verify)
	gate.repo, gate.ticket = "org/repo", "NET-1"
	// gate.pullCheck deliberately left nil — dark default.

	if gate.pullCheck != nil {
		t.Fatal("gate.pullCheck != nil — test setup broken, this test requires the dark default")
	}
	_, verdict := gate.Decide(context.Background(), "done", slog.Default())
	if !verdict.Met {
		t.Fatalf("verdict.Met = false, want true")
	}
	if urlCalls != 0 {
		t.Fatalf("prURLFn was called %d times with gate.pullCheck nil, want 0 (eager prURLBestEffort regression)", urlCalls)
	}
}

// TestPrURLBestEffortRespectsTimeout is the regression test for the
// unbounded-gh-subprocess finding: a hung prURLFn (simulating a stalled `gh
// pr list`) must not block prURLBestEffort past pullCheckRPCTimeout.
func TestPrURLBestEffortRespectsTimeout(t *testing.T) {
	origTimeout := pullCheckRPCTimeout
	pullCheckRPCTimeout = 200 * time.Millisecond
	defer func() { pullCheckRPCTimeout = origTimeout }()

	origURL := prURLFn
	defer func() { prURLFn = origURL }()
	prURLFn = func(ctx context.Context, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	done := make(chan string)
	go func() { done <- prURLBestEffort(context.Background(), "org/repo", "builder/NET-1") }()

	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("prURLBestEffort = %q, want \"\" (timed out)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prURLBestEffort did not return — a hung gh subprocess can stall the gate")
	}
}

// TestPullCheckTargetRespectsTimeout is the same regression test for
// pullCheckTarget's `gh repo view` call.
func TestPullCheckTargetRespectsTimeout(t *testing.T) {
	origTimeout := pullCheckRPCTimeout
	pullCheckRPCTimeout = 200 * time.Millisecond
	defer func() { pullCheckRPCTimeout = origTimeout }()

	origTarget := pullCheckTargetFn
	defer func() { pullCheckTargetFn = origTarget }()
	pullCheckTargetFn = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	done := make(chan string)
	go func() { done <- pullCheckTarget(context.Background(), "org/repo") }()

	select {
	case got := <-done:
		if got != "main" {
			t.Fatalf("pullCheckTarget = %q, want \"main\" (fallback on timeout)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pullCheckTarget did not return — a hung gh subprocess can stall the gate")
	}
}

// TestPullCheckWiringBlockedGhDoesNotStallGate is the wiring-layer proof: a
// hung `gh` call (via pullCheckTargetFn, exercised through
// builderPRVerifier's ensurePull path) must not change the PR-exists gate's
// own return value, and the whole call must complete within a small multiple
// of pullCheckRPCTimeout — mirrors TestPullCheckWiringBlockedServerDoesNotStallGate
// but for the gh side instead of the gRPC side.
func TestPullCheckWiringBlockedGhDoesNotStallGate(t *testing.T) {
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}
	origTarget := pullCheckTargetFn
	defer func() { pullCheckTargetFn = origTarget }()
	pullCheckTargetFn = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	origTimeout := pullCheckRPCTimeout
	pullCheckRPCTimeout = 200 * time.Millisecond
	defer func() { pullCheckRPCTimeout = origTimeout }()

	fake := &fakePullServer{}
	run := dialFakePullServer(t, fake)
	origRun := pullCheckRun
	pullCheckRun = run
	defer func() { pullCheckRun = origRun }()

	done := make(chan bool)
	start := time.Now()
	go func() { done <- builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")() }()

	select {
	case got := <-done:
		if !got {
			t.Fatal("builderPRVerifier() = false, want true — a hung gh default-branch lookup must never change the gate's own verdict")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("builderPRVerifier() took %v, want bounded by pullCheckRPCTimeout (gh was hung)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("builderPRVerifier() did not return — a hung gh call stalled the gate")
	}
}
