package pullchecks

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	cairnv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/cairn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// scopeCheckingPullServer is a fake PullService that enforces cairn-server's
// REAL scope rule (cairn internal/grpcapi/grpcapi.go identityFromCtx +
// authed()/hasScope(): cwb-scopes is self-asserted gRPC metadata,
// space-separated, checked with strings.Fields — NOT derived from the mTLS
// cert). Unlike fakePullServer (which accepts any call unconditionally),
// this fake reproduces the actual PermissionDenied cairn#99 review found:
// OpenPull requires repo:write, RecordPullCheck requires checks:attest
// (post-#105) — a Recorder presenting the wrong scope on either call must
// see exactly the failure a real cairn-server would return.
type scopeCheckingPullServer struct {
	cairnv1.UnimplementedPullServiceServer
}

func hasScope(ctx context.Context, want string) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, raw := range md.Get("cwb-scopes") {
		for _, s := range strings.Fields(raw) {
			if s == want {
				return true
			}
		}
	}
	return false
}

func (s *scopeCheckingPullServer) OpenPull(ctx context.Context, req *cairnv1.OpenPullRequest) (*cairnv1.OpenPullResponse, error) {
	if !hasScope(ctx, "repo:write") {
		return nil, status.Error(codes.PermissionDenied, "missing scope repo:write")
	}
	return &cairnv1.OpenPullResponse{Pull: &cairnv1.Pull{
		Id: "pull-1", Repo: req.Slug, Source: req.Source, Target: req.Target, Title: req.Title, State: "open",
	}}, nil
}

func (s *scopeCheckingPullServer) RecordPullCheck(ctx context.Context, req *cairnv1.RecordPullCheckRequest) (*cairnv1.RecordPullCheckResponse, error) {
	if !hasScope(ctx, "checks:attest") {
		return nil, status.Error(codes.PermissionDenied, "missing scope checks:attest")
	}
	return &cairnv1.RecordPullCheckResponse{Check: &cairnv1.PullCheck{
		Id: "check-1", PullId: req.Id, Name: req.Name, State: req.State,
	}}, nil
}

// dialScopeCheckingPullServer starts scopeCheckingPullServer in-process over
// bufconn and returns a dialed grpc.ClientConnInterface — a local copy of
// recorder_test.go's startFakePullServer, duplicated because that helper's
// signature is narrowed to *fakePullServer specifically (not an interface),
// matching this package's existing per-test-file fake convention.
func dialScopeCheckingPullServer(t *testing.T, fake cairnv1.PullServiceServer) grpc.ClientConnInterface {
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

// TestRecorderPresentsCorrectPerCallScope is the #474 MUST-FIX regression
// test: against a fake PullService that enforces cairn's real per-RPC scope
// rule (OpenPull needs repo:write, RecordPullCheck needs checks:attest —
// #105), BOTH EnsurePull and Record must succeed. Before the fix, callCtx
// hardcoded cwb-scopes=repo:write for every call, so Record here would fail
// with PermissionDenied and the failure would be silently swallowed by the
// best-effort caller (RecordVerdicts) — no check would ever land, and
// MergePull's enforcement would never fire. This test has teeth: run it
// against the pre-fix callCtx (single hardcoded "repo:write" scope for every
// call) and Record fails with PermissionDenied: missing scope checks:attest
// — see this file's companion mutation note below.
func TestRecorderPresentsCorrectPerCallScope(t *testing.T) {
	fake := &scopeCheckingPullServer{}
	conn := dialScopeCheckingPullServer(t, fake)
	rec := New(conn, "org-1", "widgets", "PROJ", slog.Default())

	ctx := context.Background()
	pullID, err := rec.EnsurePull(ctx, "builder/NET-1", "main", "authoritative gate checks: NET-1", "")
	if err != nil {
		t.Fatalf("EnsurePull (OpenPull, wants repo:write) failed: %v", err)
	}
	if pullID != "pull-1" {
		t.Fatalf("EnsurePull id = %q, want pull-1", pullID)
	}

	if err := rec.Record(ctx, pullID, "pr-exists", StatePass, "PR found", ""); err != nil {
		t.Fatalf("Record (RecordPullCheck, wants checks:attest) failed: %v — "+
			"this is exactly the #474 MUST-FIX regression (callCtx presenting "+
			"repo:write instead of checks:attest on the Record call)", err)
	}
}
