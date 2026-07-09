package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

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
}

func (f *fakePullServer) OpenPull(_ context.Context, req *cairnv1.OpenPullRequest) (*cairnv1.OpenPullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openPullCalls++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &cairnv1.OpenPullResponse{Pull: &cairnv1.Pull{Id: "pull-1", Repo: req.Slug, Source: req.Source, Target: req.Target}}, nil
}

func (f *fakePullServer) RecordPullCheck(_ context.Context, req *cairnv1.RecordPullCheckRequest) (*cairnv1.RecordPullCheckResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls = append(f.recordCalls, req)
	if f.recordErr != nil {
		return nil, f.recordErr
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
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}

	got := builderPRVerifier(slog.Default(), "plumb", "org/repo", "NET-1", "")()
	if !got {
		t.Fatal("builderPRVerifier() = false, want true (PR gate itself unaffected by pull-checks being dark)")
	}
	if pullCheckRun != nil {
		t.Fatal("pullCheckRun became non-nil as a side effect of running the gate — dark default violated")
	}
}

// TestPullCheckWiringRecordsPRGateVerdicts proves builderPRVerifier records
// pr-exists and pr-substantial against the SAME pull (EnsurePull idempotency
// exercised via two ensurePull calls under the hood — one per check) when a
// recorder IS configured.
func TestPullCheckWiringRecordsPRGateVerdicts(t *testing.T) {
	origExists, origStats := prExistsFn, prDiffStatsFn
	defer func() { prExistsFn, prDiffStatsFn = origExists, origStats }()
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prDiffStatsFn = func(string, string) (prDiffStats, bool, error) {
		return prDiffStats{Additions: 5, Deletions: 1, ChangedFiles: 1}, true, nil
	}
	origURL := prURLFn
	defer func() { prURLFn = origURL }()
	prURLFn = func(string, string) (string, error) { return "https://cairn.example/org-1/widgets/pulls/pull-1", nil }

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
