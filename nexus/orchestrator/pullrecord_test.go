package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

	cairnv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/cairn/v1"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/gates"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"github.com/CarriedWorldUniverse/nexus/runtime/pullchecks"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// spyRecorder is a minimal PullCheckRecorder fake for the RecordVerdicts
// unit tests below — it records every call it receives and can be told to
// fail EnsurePull/Record, without standing up a real gRPC server. The
// bufconn-backed fakePullServer further down is used separately, for the
// acceptance criterion that explicitly calls for "a fake PullService"
// (integration-style, exercising the REAL pullchecks.Recorder end to end).
type spyRecorder struct {
	mu sync.Mutex

	ensureCalls int
	ensureErr   error
	pullID      string

	recordCalls []recordCall
	recordErr   error
}

type recordCall struct {
	pullID, name, state, summary, evidenceURL string
}

func (s *spyRecorder) EnsurePull(_ context.Context, _, _, _, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls++
	if s.ensureErr != nil {
		return "", s.ensureErr
	}
	id := s.pullID
	if id == "" {
		id = "pull-1"
	}
	return id, nil
}

func (s *spyRecorder) Record(_ context.Context, pullID, name, state, summary, evidenceURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCalls = append(s.recordCalls, recordCall{pullID, name, state, summary, evidenceURL})
	return s.recordErr
}

// TestRecordVerdictsDarkByDefault: rec == nil must make zero calls — the
// contract RecordVerdicts and Orchestrator.PullRecorder's doc both promise.
func TestRecordVerdictsDarkByDefault(t *testing.T) {
	verdicts := []GateVerdict{{Name: GatePRExists, State: GateStatePass, Summary: "found"}}
	// A nil rec must never even be type-asserted/called into — passing a
	// literal nil interface value proves RecordVerdicts' own guard, not a
	// spy that happens to tolerate nil receivers.
	RecordVerdicts(context.Background(), nil, slog.Default(), "org/repo", "builder/NET-1", "NET-1", verdicts)
}

// TestRecordVerdictsEmptyVerdictsNoOp: a configured recorder with zero
// verdicts to record (e.g. GateRunner disabled, or every gate's
// precondition unmet) must still make zero calls — nothing to record.
func TestRecordVerdictsEmptyVerdictsNoOp(t *testing.T) {
	spy := &spyRecorder{}
	RecordVerdicts(context.Background(), spy, slog.Default(), "org/repo", "builder/NET-1", "NET-1", nil)
	if spy.ensureCalls != 0 || len(spy.recordCalls) != 0 {
		t.Fatalf("expected zero calls for empty verdicts, got ensure=%d record=%d", spy.ensureCalls, len(spy.recordCalls))
	}
}

// TestRecordVerdictsEnsuresOnceRecordsEach: a configured recorder with N
// verdicts must EnsurePull exactly once, then Record once per verdict,
// carrying each verdict's own name/state/summary/evidence_url through.
func TestRecordVerdictsEnsuresOnceRecordsEach(t *testing.T) {
	spy := &spyRecorder{pullID: "pull-77"}
	verdicts := []GateVerdict{
		{Name: GatePRExists, State: GateStatePass, Summary: "PR found", EvidenceURL: "https://example/pr/1"},
		{Name: GatePRSubstantial, State: GateStatePass, Summary: "clears floor"},
		{Name: GateAcceptanceJudge, State: GateStateFail, Summary: "criteria not met"},
	}
	RecordVerdicts(context.Background(), spy, slog.Default(), "org/repo", "builder/NET-1", "NET-1", verdicts)

	if spy.ensureCalls != 1 {
		t.Fatalf("EnsurePull calls = %d, want 1", spy.ensureCalls)
	}
	if len(spy.recordCalls) != 3 {
		t.Fatalf("Record calls = %d, want 3", len(spy.recordCalls))
	}
	for i, v := range verdicts {
		got := spy.recordCalls[i]
		if got.pullID != "pull-77" || got.name != v.Name || got.state != v.State || got.summary != v.Summary || got.evidenceURL != v.EvidenceURL {
			t.Errorf("record call %d = %+v, want pullID=pull-77 name=%s state=%s summary=%s evidenceURL=%s",
				i, got, v.Name, v.State, v.Summary, v.EvidenceURL)
		}
	}
}

// TestRecordVerdictsEnsurePullFailureRecordsNothing: EnsurePull erroring
// must skip every Record call (no pull id to record against) without
// panicking or returning an error itself.
func TestRecordVerdictsEnsurePullFailureRecordsNothing(t *testing.T) {
	spy := &spyRecorder{ensureErr: errors.New("boom")}
	verdicts := []GateVerdict{{Name: GatePRExists, State: GateStatePass}}
	RecordVerdicts(context.Background(), spy, slog.Default(), "org/repo", "builder/NET-1", "NET-1", verdicts)
	if spy.ensureCalls != 1 {
		t.Fatalf("EnsurePull calls = %d, want 1", spy.ensureCalls)
	}
	if len(spy.recordCalls) != 0 {
		t.Fatalf("Record calls = %d, want 0 (EnsurePull failed)", len(spy.recordCalls))
	}
}

// TestRecordVerdictsRecordFailureIsBestEffort: a Record error for one
// verdict must not stop the remaining verdicts from being recorded, and
// RecordVerdicts must not panic or signal failure to its caller (no error
// return — best-effort by design).
func TestRecordVerdictsRecordFailureIsBestEffort(t *testing.T) {
	spy := &spyRecorder{pullID: "pull-1", recordErr: errors.New("cairn-server unavailable")}
	verdicts := []GateVerdict{
		{Name: GatePRExists, State: GateStatePass},
		{Name: GatePRSubstantial, State: GateStatePass},
	}
	RecordVerdicts(context.Background(), spy, slog.Default(), "org/repo", "builder/NET-1", "NET-1", verdicts)
	if len(spy.recordCalls) != 2 {
		t.Fatalf("Record calls = %d, want 2 (both attempted despite errors)", len(spy.recordCalls))
	}
}

// --- integration test against a fake PullService, exercising the REAL
// pullchecks.Recorder (not the spy above) end to end over gRPC. ---

type fakePullServer struct {
	cairnv1.UnimplementedPullServiceServer

	mu            sync.Mutex
	openPullCalls []*cairnv1.OpenPullRequest
	recordCalls   []*cairnv1.RecordPullCheckRequest
	mdCalls       []metadata.MD
}

func (f *fakePullServer) OpenPull(ctx context.Context, req *cairnv1.OpenPullRequest) (*cairnv1.OpenPullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openPullCalls = append(f.openPullCalls, req)
	return &cairnv1.OpenPullResponse{Pull: &cairnv1.Pull{
		Id: "pull-99", Repo: req.Slug, Source: req.Source, Target: req.Target, Title: req.Title, State: "open",
	}}, nil
}

func (f *fakePullServer) RecordPullCheck(ctx context.Context, req *cairnv1.RecordPullCheckRequest) (*cairnv1.RecordPullCheckResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls = append(f.recordCalls, req)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		f.mdCalls = append(f.mdCalls, md)
	} else {
		f.mdCalls = append(f.mdCalls, nil)
	}
	return &cairnv1.RecordPullCheckResponse{Check: &cairnv1.PullCheck{
		Id: "check-1", PullId: req.Id, Name: req.Name, State: req.State,
	}}, nil
}

// dialFakePullServer starts fake in-process over bufconn and returns a
// dialed grpc.ClientConnInterface — mirrors runtime/pullchecks's own test
// helper (startFakePullServer), duplicated here narrowly rather than
// exported cross-package, matching this codebase's existing test-helper
// convention (see gates_test.go writeFakeGh).
func dialFakePullServer(t *testing.T, fake *fakePullServer) grpc.ClientConnInterface {
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
	return conn
}

// TestRecordVerdictsAgainstFakePullService is the integration proof called
// for explicitly (#474 acceptance): a configured recorder (the REAL
// pullchecks.Recorder, dialed to a fake PullService) records one EnsurePull
// + one RecordPullCheck per verdict, every RPC presenting
// cwb-subject=broker-gate (pullchecks.BrokerGateSubject) — the same subject
// the (now-removed) worker-side recorder used, preserving check attribution.
func TestRecordVerdictsAgainstFakePullService(t *testing.T) {
	fake := &fakePullServer{}
	conn := dialFakePullServer(t, fake)
	rec := pullchecks.New(conn, "org-1", "widgets", "PROJ", slog.Default())

	verdicts := []GateVerdict{
		{Name: GatePRExists, State: GateStatePass, Summary: "PR found"},
		{Name: GatePRSubstantial, State: GateStatePass, Summary: "clears floor"},
		{Name: GateAcceptanceJudge, State: GateStatePass, Summary: "criteria met"},
		{Name: GateTestEvidence, State: GateStateFail, Summary: "no test file touched"},
	}
	RecordVerdicts(context.Background(), rec, slog.Default(), "org/repo", "builder/NET-9", "NET-9", verdicts)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.openPullCalls) != 1 {
		t.Fatalf("OpenPull calls = %d, want 1", len(fake.openPullCalls))
	}
	if len(fake.recordCalls) != len(verdicts) {
		t.Fatalf("RecordPullCheck calls = %d, want %d", len(fake.recordCalls), len(verdicts))
	}
	for i, v := range verdicts {
		got := fake.recordCalls[i]
		if got.Name != v.Name || got.State != v.State || got.Summary != v.Summary {
			t.Errorf("RecordPullCheck call %d = %+v, want name=%s state=%s summary=%s", i, got, v.Name, v.State, v.Summary)
		}
	}
	for i, md := range fake.mdCalls {
		subs := md.Get("cwb-subject")
		if len(subs) != 1 || subs[0] != pullchecks.BrokerGateSubject {
			t.Errorf("RecordPullCheck call %d cwb-subject = %v, want [%s]", i, subs, pullchecks.BrokerGateSubject)
		}
	}
}

// TestOnJobDoneHookRecordsVerdictsWhenPullRecorderConfigured wires
// RunAuthoritativeGates through OnJobDoneHook end to end with BOTH a
// GateRunner and a PullRecorder configured, proving wake.go's
// runAuthoritativeGates calls RecordVerdicts (not just LogVerdicts) once
// verdicts exist.
func TestOnJobDoneHookRecordsVerdictsWhenPullRecorderConfigured(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("NET-10", workgraph.WorkItem{
		Role: "builder", TaskSpec: "build", Repo: "org/repo",
		AcceptanceCriteria: []string{"do the thing"},
	})
	graph.items["NET-10"].status = workgraph.StatusDispatched
	disp := &fakeDispatcher{}

	opts := &GateRunnerOptions{
		Enabled: true,
		ExistsFn: func(context.Context, string, string) (bool, error) {
			return true, nil
		},
		ExistsByTicketFn: func(context.Context, string, string) (bool, error) { return false, nil },
		DiffStatsFn: func(context.Context, string, string) (gates.PRDiffStats, bool, error) {
			return gates.PRDiffStats{Additions: 5, Deletions: 0, ChangedFiles: 1}, true, nil
		},
		DiffFn: func(context.Context, string, string, string) (string, bool, error) { return "", false, nil },
	}

	spy := &spyRecorder{pullID: "pull-10"}
	o := &Orchestrator{
		Graph:        graph,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{"builder"},
		GateRunner:   opts,
		PullRecorder: spy,
	}

	hook := o.OnJobDoneHook()
	hook(dispatch.JobDone{Ticket: "NET-10", OK: true})

	if spy.ensureCalls != 1 {
		t.Fatalf("EnsurePull calls = %d, want 1 — RunAuthoritativeGates should have produced at least a pr-exists verdict", spy.ensureCalls)
	}
	if len(spy.recordCalls) == 0 {
		t.Fatal("Record calls = 0, want at least one (pr-exists/pr-substantial)")
	}
}

// TestOnJobDoneHookDoesNotRecordWhenPullRecorderNil is the dark-default
// proof at the OnJobDoneHook layer (Orchestrator.PullRecorder unset): even
// with a live GateRunner producing real verdicts, zero PullService calls are
// made — RunAuthoritativeGates/LogVerdicts still run/log exactly as #473
// left them.
func TestOnJobDoneHookDoesNotRecordWhenPullRecorderNil(t *testing.T) {
	graph := newFakeGraph()
	graph.addReady("NET-11", workgraph.WorkItem{
		Role: "builder", TaskSpec: "build", Repo: "org/repo",
		AcceptanceCriteria: []string{"do the thing"},
	})
	graph.items["NET-11"].status = workgraph.StatusDispatched
	disp := &fakeDispatcher{}

	opts := &GateRunnerOptions{
		Enabled: true,
		ExistsFn: func(context.Context, string, string) (bool, error) {
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
		// PullRecorder intentionally left nil.
	}

	hook := o.OnJobDoneHook()
	// mutation-style dark-default proof: hook must not panic/error dialing
	// a nil PullRecorder, and — the real assertion — RecordVerdicts' own
	// `rec == nil` guard means this whole path made zero PullService calls
	// by construction (there is no fake server here at all to accidentally
	// dial: if RecordVerdicts ever forgot its nil guard this test would
	// panic on a nil interface method call rather than silently pass).
	hook(dispatch.JobDone{Ticket: "NET-11", OK: true})

	if graph.items["NET-11"].status != workgraph.StatusDone {
		t.Errorf("wi status = %v, want done", graph.items["NET-11"].status)
	}
}
