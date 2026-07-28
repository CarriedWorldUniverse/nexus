package pullchecks

import (
	"context"
	"log/slog"

	cairnv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/cairn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// BrokerGateSubject is the cwb-subject every Recorder presents to
// cairn-server. It is a constant, NOT the builder aspect's own identity —
// pull checks must be attributable to the gate that produced the verdict,
// not the worker being gated (separation of duties; the scope split proper
// comes later — see cairn#99). Every check this package records, and the
// pull it ensures, is recorded_by/opened_by "broker-gate".
const BrokerGateSubject = "broker-gate"

// pullServiceClient is the narrow slice of cairnv1.PullServiceClient this
// package uses. Declared locally so fakes in tests only need to implement
// what we call — *cairnv1.pullServiceClient (built by
// cairnv1.NewPullServiceClient) already satisfies this structurally, and
// tests can supply an in-process fake grpc server via bufconn instead.
type pullServiceClient interface {
	OpenPull(ctx context.Context, in *cairnv1.OpenPullRequest, opts ...grpc.CallOption) (*cairnv1.OpenPullResponse, error)
	RecordPullCheck(ctx context.Context, in *cairnv1.RecordPullCheckRequest, opts ...grpc.CallOption) (*cairnv1.RecordPullCheckResponse, error)
}

// Recorder is the broker's client to cairn-server's PullService, scoped to
// one cwb-org/repo-slug. Every call presents cwb-subject=BrokerGateSubject
// and a per-RPC cwb-scopes value (see callCtx) — NOT a single blanket scope:
// OpenPull needs repo:write, RecordPullCheck needs checks:attest (#105).
type Recorder struct {
	pull pullServiceClient

	// Org/Slug identify the cairn repo every OpenPull/RecordPullCheck call
	// targets (org path param + repo slug path param).
	Org  string
	Slug string
	// Project is the default ledger project key EnsurePull opens pulls
	// under, read from CW_PULL_PROJECT (see NewRecorderFromEnv). Callers may
	// still pass a different project explicitly to EnsurePull.
	Project string

	log *slog.Logger
}

// New builds a Recorder from an established gRPC connection to cairn-server.
// org/slug identify the repo every call targets; project is the default
// ledger project EnsurePull opens pulls under. log defaults to slog.Default
// when nil.
func New(conn grpc.ClientConnInterface, org, slug, project string, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{
		pull:    cairnv1.NewPullServiceClient(conn),
		Org:     org,
		Slug:    slug,
		Project: project,
		log:     log,
	}
}

// Scope literals cairn-server's authed()/hasScope() check against
// cwb-scopes (internal/grpcapi/grpcapi.go identityFromCtx —
// `strings.Fields(get("cwb-scopes"))`, a self-asserted gRPC metadata value,
// NOT derived from the mTLS cert). Per-RPC, NOT interchangeable post-#105:
//
//   - OpenPull (EnsurePull's call) still requires repo:write — unchanged by
//     #105's scope split.
//   - RecordPullCheck (Record's call) requires checks:attest — #105 narrowed
//     this off repo:write specifically so a credential that can open/write a
//     pull is not automatically trusted to attest gate verdicts on it.
//   - ListPullChecks (unused by this package today) would require repo:read.
//
// Presenting the wrong scope on a call is silently swallowed by this
// package's own best-effort failure policy (Record logs and returns an
// error, but the caller — orchestrator.RecordVerdicts — treats that as "the
// check didn't land" and moves on) — cairn#99 review found exactly this:
// callCtx used to hardcode repo:write for every call, so every Record call
// PermissionDenied'd on missing checks:attest and no check ever recorded.
const (
	scopeRepoWrite    = "repo:write"
	scopeChecksAttest = "checks:attest"
)

// callCtx attaches the cwb-subject/cwb-org/cwb-scopes identity metadata
// cairn-server's gateway-trust model requires (see cairn internal/grpcapi's
// identityFromCtx) — mirrors nexus/workgraph.Client.ctxAs. scope is the
// exact (space-separated, if more than one) scope literal THIS call needs —
// see the scope* consts above; callers must pass the minimum scope for the
// specific RPC, not a blanket value, since #105 split RecordPullCheck's
// requirement (checks:attest) off OpenPull's (repo:write).
func (r *Recorder) callCtx(ctx context.Context, scope string) context.Context {
	ctx = metadata.AppendToOutgoingContext(ctx, "cwb-subject", BrokerGateSubject, "cwb-org", r.Org)
	return metadata.AppendToOutgoingContext(ctx, "cwb-scopes", scope)
}

// EnsurePull opens the pull for (repo, source, target), returning its id.
// OpenPull is idempotent per (repo, source, target) while open — calling
// EnsurePull twice for the same run returns the SAME pull id, so callers
// never need to cache it across gate invocations. project overrides
// r.Project for this one call when non-empty; empty falls back to r.Project.
//
// source/target/title are sanitized through SanitizeName (the same
// control-char-strip + cap path Record's name field uses — title is
// name-capped at 128B since it plays the same "short label" role) before the
// RPC, so a control-char-bearing ticket/branch name (source/title are both
// commonly built from a ticket ID — see the agentfunnel wiring) can neither
// trip cairn-server's InvalidArgument validation nor land ugly text on the
// pull.
func (r *Recorder) EnsurePull(ctx context.Context, source, target, title, project string) (string, error) {
	if project == "" {
		project = r.Project
	}
	source = SanitizeName(source)
	target = SanitizeName(target)
	title = SanitizeName(title)
	resp, err := r.pull.OpenPull(r.callCtx(ctx, scopeRepoWrite), &cairnv1.OpenPullRequest{
		Org:     r.Org,
		Slug:    r.Slug,
		Source:  source,
		Target:  target,
		Title:   title,
		Project: project,
	})
	if err != nil {
		r.log.Error("pullchecks: OpenPull failed — pull check will not be recorded",
			"org", r.Org, "slug", r.Slug, "source", source, "target", target, "err", err)
		return "", err
	}
	return resp.GetPull().GetId(), nil
}

// Record upserts a check verdict on pullID by name (RecordPullCheck upserts
// by (pull, name) — re-recording the same name replaces its state/summary/
// evidence_url). name/summary/evidenceURL are sanitized before the RPC so
// this NEVER trips cairn-server's InvalidArgument validation.
//
// Best-effort by design (broker gate outcome is already authoritative
// broker-side; cairn-server's MergePull is what actually enforces on these
// checks): a failure here is logged loudly and returned to the caller so it
// can note it in the run's evidence, but must never fail the run itself.
func (r *Recorder) Record(ctx context.Context, pullID, name, state, summary, evidenceURL string) error {
	name = SanitizeName(name)
	summary = SanitizeSummary(summary)
	evidenceURL = SanitizeEvidenceURL(evidenceURL)
	_, err := r.pull.RecordPullCheck(r.callCtx(ctx, scopeChecksAttest), &cairnv1.RecordPullCheckRequest{
		Org:         r.Org,
		Slug:        r.Slug,
		Id:          pullID,
		Name:        name,
		State:       state,
		Summary:     summary,
		EvidenceUrl: evidenceURL,
	})
	if err != nil {
		r.log.Error("pullchecks: RecordPullCheck failed",
			"org", r.Org, "slug", r.Slug, "pull_id", pullID, "name", name, "state", state, "err", err)
		return err
	}
	return nil
}

// State values RecordPullCheck accepts (cairn's repo.CheckStatePass/Fail/Pending).
const (
	StatePass    = "pass"
	StateFail    = "fail"
	StatePending = "pending"
)
