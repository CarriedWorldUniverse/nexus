package pullchecks

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

	cairnv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/cairn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// fakePullServer is an in-process cairnv1.PullServiceServer used to exercise
// Recorder without a real cairn-server. It records every call it receives
// (openPullCalls/recordCalls) so tests can assert on exactly what a gate
// verdict produced, and supports injecting an error to prove the
// best-effort failure policy.
type fakePullServer struct {
	cairnv1.UnimplementedPullServiceServer

	mu            sync.Mutex
	openPullCalls []*cairnv1.OpenPullRequest
	recordCalls   []*cairnv1.RecordPullCheckRequest
	mdCalls       []metadata.MD // incoming metadata captured per RecordPullCheck call

	// openErr/recordErr, when non-nil, make the corresponding RPC fail —
	// used to exercise the failure-policy test.
	openErr   error
	recordErr error

	// pullID is the id every OpenPull call returns, proving idempotency: two
	// OpenPull calls for the same (source,target) return the SAME id in the
	// real server (dedupe is server-side) — the fake just always returns
	// this fixed id, which is enough to prove the CLIENT treats repeat
	// EnsurePull calls as safe/idempotent from its own side.
	pullID string
}

func (f *fakePullServer) OpenPull(ctx context.Context, req *cairnv1.OpenPullRequest) (*cairnv1.OpenPullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openPullCalls = append(f.openPullCalls, req)
	if f.openErr != nil {
		return nil, f.openErr
	}
	id := f.pullID
	if id == "" {
		id = "pull-1"
	}
	return &cairnv1.OpenPullResponse{Pull: &cairnv1.Pull{
		Id: id, Repo: req.Slug, Source: req.Source, Target: req.Target, Title: req.Title, State: "open",
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
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	return &cairnv1.RecordPullCheckResponse{Check: &cairnv1.PullCheck{
		Id: "check-1", PullId: req.Id, Name: req.Name, State: req.State,
		Summary: req.Summary, EvidenceUrl: req.EvidenceUrl, RecordedBy: BrokerGateSubject,
	}}, nil
}

// startFakePullServer starts fake in-process over bufconn and returns a
// dialed grpc.ClientConnInterface plus a stop func.
func startFakePullServer(t *testing.T, fake *fakePullServer) grpc.ClientConnInterface {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	cairnv1.RegisterPullServiceServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestRecorderEnsurePullAndRecord(t *testing.T) {
	fake := &fakePullServer{pullID: "pull-42"}
	conn := startFakePullServer(t, fake)
	rec := New(conn, "org-1", "widgets", "PROJ", slog.Default())

	ctx := context.Background()
	id1, err := rec.EnsurePull(ctx, "builder/NET-1", "main", "builder gate checks: NET-1", "")
	if err != nil {
		t.Fatalf("EnsurePull #1: %v", err)
	}
	if id1 != "pull-42" {
		t.Fatalf("EnsurePull #1 id = %q, want pull-42", id1)
	}

	// Idempotency: a second EnsurePull for the same source/target must be
	// safe to call and return the SAME id (the real server dedupes
	// server-side; this proves the client doesn't do anything that would
	// break under repeat calls, e.g. mutate org/slug or panic on reuse).
	id2, err := rec.EnsurePull(ctx, "builder/NET-1", "main", "builder gate checks: NET-1", "")
	if err != nil {
		t.Fatalf("EnsurePull #2: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("EnsurePull #2 id = %q, want %q (idempotent)", id2, id1)
	}

	fake.mu.Lock()
	if len(fake.openPullCalls) != 2 {
		t.Fatalf("OpenPull calls = %d, want 2 (idempotency exercised via a second call)", len(fake.openPullCalls))
	}
	first := fake.openPullCalls[0]
	fake.mu.Unlock()
	if first.Org != "org-1" || first.Slug != "widgets" || first.Source != "builder/NET-1" || first.Target != "main" || first.Project != "PROJ" {
		t.Fatalf("OpenPull request = %+v, unexpected fields", first)
	}

	if err := rec.Record(ctx, id1, "pr-exists", StatePass, "PR builder/NET-1 found", "https://cairn.example/org-1/widgets/pulls/pull-42"); err != nil {
		t.Fatalf("Record pr-exists: %v", err)
	}
	if err := rec.Record(ctx, id1, "acceptance-judge", StateFail, "criteria not met: missing token", ""); err != nil {
		t.Fatalf("Record acceptance-judge: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.recordCalls) != 2 {
		t.Fatalf("RecordPullCheck calls = %d, want 2", len(fake.recordCalls))
	}
	byName := map[string]*cairnv1.RecordPullCheckRequest{}
	for _, c := range fake.recordCalls {
		byName[c.Name] = c
	}
	prExists := byName["pr-exists"]
	if prExists == nil {
		t.Fatal("no pr-exists check recorded")
	}
	if prExists.State != StatePass || prExists.Id != id1 {
		t.Errorf("pr-exists check = %+v, want state=pass id=%s", prExists, id1)
	}
	judge := byName["acceptance-judge"]
	if judge == nil {
		t.Fatal("no acceptance-judge check recorded")
	}
	if judge.State != StateFail {
		t.Errorf("acceptance-judge check state = %q, want fail", judge.State)
	}

	// recorded_by/subject metadata: every RecordPullCheck call must present
	// cwb-subject=broker-gate (BrokerGateSubject), NOT the builder aspect's
	// own identity — the check is attributable to the gate.
	for i, md := range fake.mdCalls {
		if md == nil {
			t.Fatalf("call %d: no incoming metadata captured", i)
		}
		got := md.Get("cwb-subject")
		if len(got) != 1 || got[0] != BrokerGateSubject {
			t.Errorf("call %d: cwb-subject metadata = %v, want [%s]", i, got, BrokerGateSubject)
		}
		if org := md.Get("cwb-org"); len(org) != 1 || org[0] != "org-1" {
			t.Errorf("call %d: cwb-org metadata = %v, want [org-1]", i, org)
		}
	}
}

func TestRecorderSanitizesBeforeSending(t *testing.T) {
	fake := &fakePullServer{}
	conn := startFakePullServer(t, fake)
	rec := New(conn, "org-1", "widgets", "PROJ", slog.Default())

	if err := rec.Record(context.Background(), "pull-1", "pr-exists\n", StatePass, "line one\nline two", "https://x/\nevil"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.recordCalls) != 1 {
		t.Fatalf("RecordPullCheck calls = %d, want 1", len(fake.recordCalls))
	}
	got := fake.recordCalls[0]
	if got.Name != "pr-exists" {
		t.Errorf("sanitized name = %q, want %q", got.Name, "pr-exists")
	}
	if got.Summary != "line oneline two" {
		t.Errorf("sanitized summary = %q, want %q", got.Summary, "line oneline two")
	}
	if got.EvidenceUrl != "https://x/evil" {
		t.Errorf("sanitized evidence_url = %q, want %q", got.EvidenceUrl, "https://x/evil")
	}
}

// TestRecorderFailurePolicy proves the best-effort failure contract: a
// PullService error is returned to the caller (so it CAN log/note it) but
// the Recorder itself never panics, retries forever, or otherwise takes an
// action that would block a run — the caller (the gate wiring) is
// responsible for treating this as non-fatal, exactly as
// docs/network/ACCEPTANCE-GATE-HARDENING.md specifies.
func TestRecorderFailurePolicy(t *testing.T) {
	fake := &fakePullServer{
		openErr:   errors.New("cairn-server unavailable"),
		recordErr: errors.New("cairn-server unavailable"),
	}
	conn := startFakePullServer(t, fake)
	rec := New(conn, "org-1", "widgets", "PROJ", slog.Default())

	ctx := context.Background()
	if _, err := rec.EnsurePull(ctx, "builder/NET-1", "main", "t", ""); err == nil {
		t.Fatal("EnsurePull: want error surfaced, got nil")
	}
	if err := rec.Record(ctx, "pull-1", "pr-exists", StatePass, "", ""); err == nil {
		t.Fatal("Record: want error surfaced, got nil")
	}
	// No panic, no goroutine leak, no infinite retry loop above — reaching
	// this line at all is the proof of the non-fatal contract.
}
