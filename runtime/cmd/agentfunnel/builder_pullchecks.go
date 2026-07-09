package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/pullchecks"
)

// pullCheckRun is the run's cairn pull-check recorder — nil (dark) unless
// CW_PULL_* env config is present. Set once in main()'s builder wiring
// (alongside acceptanceGate.repo/branch/ticket) and read from
// builderPRVerifier's returned closure via the package-level convention this
// file already uses for prExistsFn/prCreateFn/etc: a swappable var rather
// than threading a new parameter through builderPRVerifier/
// builderCompleteCheck, whose signatures existing tests call directly and
// must stay unchanged. Every test in this package leaves pullCheckRun nil,
// so every existing test's behavior — and the PullService call count — is
// unaffected by this file (dark-default back-compat).
var pullCheckRun *pullRunRecorder

// pullCheckRPCTimeout bounds every individual EnsurePull/RecordPullCheck RPC
// this file issues. Pull-checks recording is best-effort and must never let
// a hung/unreachable cairn-server stall a gate — a run's own timeout/backstop
// machinery is what should govern the run, not a pull-checks side call. A
// var (not a const) so tests can shrink it rather than sleeping 5s+ per case.
var pullCheckRPCTimeout = 5 * time.Second

// pullRunRecorder wraps a pullchecks.Recorder with the run-scoped pull id
// (ACCEPTANCE-GATE-HARDENING pull-checks wiring, cairn#99): the builder
// gates — pr-exists, pr-substantial, acceptance-judge, test-evidence — all
// record their verdicts against the SAME cairn pull, so the pull is ensured
// (OpenPull, idempotent) once and cached for the rest of the process.
//
// Dark by default: pullCheckRun (the package-level instance main() builds)
// is nil unless CW_PULL_* env config is present (see
// pullchecks.NewRecorderFromEnv). Every method on a nil *pullRunRecorder is a
// safe no-op, so a run with no cairn-pull addressing makes ZERO PullService
// calls and behaves byte-identically to before this file existed — the same
// nil-receiver-safe convention the rest of this codebase uses for optional
// wiring (e.g. wsClient, statusEmitter).
type pullRunRecorder struct {
	rec *pullchecks.Recorder

	mu      sync.Mutex
	pullID  string
	ensured bool
}

// buildPullCheckRun constructs the run's pull-check recorder from env
// (CW_PULL_SERVER_ADDR/_ORG/_SLUG/_PROJECT/_TLS_*/_DEV_INSECURE — see
// pullchecks.NewRecorderFromEnv). Returns nil (dark) when unconfigured.
func buildPullCheckRun(log *slog.Logger) *pullRunRecorder {
	rec := pullchecks.NewRecorderFromEnv(log)
	if rec == nil {
		return nil
	}
	return &pullRunRecorder{rec: rec}
}

// ensurePull resolves this run's cairn pull id, ensuring it (OpenPull) on
// first SUCCESS and caching it for every subsequent gate this process
// records. repo/branch/ticket give the (source, target) pair the same way
// builderPRVerifier/builderAcceptanceGate already resolve them
// (builderBranch(branch, ticket) as source; target is the repo's resolved
// default branch — see pullCheckTarget — falling back to "main"). ok=false
// means either p is nil (dark) or EnsurePull failed (already logged loudly
// by the Recorder) — a caller sees ok=false and simply skips recording,
// never fails the run.
//
// review fix (finding #1): the acceptance-judge gate's first call often
// lands BEFORE the run's branch/PR exists — that is the exact case the gate
// exists to catch — so an early EnsurePull 404 is expected, not exceptional.
// The "ensured" latch is only set on SUCCESS; a failed attempt leaves it
// false so a LATER call (once the PR really exists) retries EnsurePull
// instead of permanently no-op'ing for the rest of the run.
//
// review fix (finding #2): the RPC itself runs OUTSIDE p.mu (compute the
// source/target/title under a quick lock-free read, call out, then latch the
// result under a quick lock) so a slow/hung cairn-server can only block the
// goroutine that's actually waiting on it, never every other gate's
// recording. A short per-RPC timeout (pullCheckRPCTimeout), derived from the
// caller's ctx, bounds the wait regardless.
func (p *pullRunRecorder) ensurePull(ctx context.Context, log *slog.Logger, repo, branch, ticket string) (string, bool) {
	if p == nil || repo == "" {
		return "", false
	}
	p.mu.Lock()
	if p.ensured {
		id := p.pullID
		p.mu.Unlock()
		return id, id != ""
	}
	p.mu.Unlock()

	source := builderBranch(branch, ticket)
	if source == "" {
		return "", false
	}
	title := ticket
	if title == "" {
		title = source
	}
	target := pullCheckTarget(repo)

	rpcCtx, cancel := context.WithTimeout(ctx, pullCheckRPCTimeout)
	defer cancel()
	id, err := p.rec.EnsurePull(rpcCtx, source, target, "builder gate checks: "+title, p.rec.Project)
	if err != nil {
		// Recorder.EnsurePull already slog.Error'd the details. Deliberately
		// do NOT latch p.ensured here — leave it false so a later call (once
		// the branch/PR this run is waiting on actually exists) retries
		// rather than silently no-op'ing for the rest of the process
		// (review finding #1).
		return "", false
	}

	p.mu.Lock()
	// Another goroutine may have raced this one to a successful EnsurePull
	// first (concurrent gate recordings) — OpenPull is idempotent per
	// (repo,source,target), so both calls resolve the SAME pull id; keep
	// whichever landed first rather than overwriting it.
	if !p.ensured {
		p.ensured = true
		p.pullID = id
	}
	winningID := p.pullID
	p.mu.Unlock()
	return winningID, winningID != ""
}

// record upserts one gate's verdict as a cairn pull check on this run's
// pull. Best-effort: p nil, ensurePull failing, or the RecordPullCheck RPC
// itself failing are all swallowed here (Recorder.Record already
// slog.Error's the details) — a pull-checks outage NEVER fails the run; the
// broker gate's own pass/fail decision is already authoritative broker-side.
// The RecordPullCheck RPC is bounded by pullCheckRPCTimeout (review finding
// #2), derived from ctx.
func (p *pullRunRecorder) record(ctx context.Context, log *slog.Logger, repo, branch, ticket, name, state, summary, evidenceURL string) {
	if p == nil {
		return
	}
	pullID, ok := p.ensurePull(ctx, log, repo, branch, ticket)
	if !ok {
		return
	}
	rpcCtx, cancel := context.WithTimeout(ctx, pullCheckRPCTimeout)
	defer cancel()
	_ = p.rec.Record(rpcCtx, pullID, name, state, summary, evidenceURL)
}

// pullCheckTargetFn resolves repo's default branch via `gh repo view`.
// Swappable in tests. Used only to pick the pull-checks target branch (see
// pullCheckTarget) — never on any gate's own pass/fail decision path, so a
// failure here can only ever widen to the "main" fallback, not affect a run.
var pullCheckTargetFn = func(repo string) (string, error) {
	out, err := exec.Command("gh", "repo", "view", repo, "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh repo view (default branch): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// pullCheckTarget resolves the target branch OpenPull should link the run's
// pull against (review finding #4: a hardcoded "main" 404s OpenPull's target
// GetRef on a repo whose default branch is "master"). CW_PULL_TARGET, when
// set, overrides resolution entirely (operator escape hatch — e.g. a repo
// with an unconventional default). Otherwise resolves repo's actual default
// branch via gh; any failure (gh unavailable, repo not found, empty result)
// falls back to "main" — today's original hardcoded behavior, unchanged when
// resolution isn't possible.
func pullCheckTarget(repo string) string {
	if v := strings.TrimSpace(os.Getenv("CW_PULL_TARGET")); v != "" {
		return v
	}
	if repo == "" {
		return "main"
	}
	branch, err := pullCheckTargetFn(repo)
	if err != nil || branch == "" {
		return "main"
	}
	return branch
}

// Pull check names (ACCEPTANCE-GATE-HARDENING vocabulary — see
// docs/network/ACCEPTANCE-GATE-HARDENING.md). Fixed literals so they always
// round-trip clean through pullchecks.SanitizeName unchanged.
const (
	pullCheckPRExists        = "pr-exists"
	pullCheckPRSubstantial   = "pr-substantial"
	pullCheckAcceptanceJudge = "acceptance-judge"
	pullCheckTestEvidence    = "test-evidence"
)

// pullCheckState maps a bool gate outcome onto the pullchecks pass/fail
// vocabulary (RecordPullCheck's state is one of pass|fail|pending — the
// gates below always have a definite verdict by the time they record, so
// pending is never used here).
func pullCheckState(pass bool) string {
	if pass {
		return pullchecks.StatePass
	}
	return pullchecks.StateFail
}

// recordPRExistsCheck records the pr-exists gate's verdict (builderPRVerifier)
// against pullCheckRun. No-op when pullCheckRun is nil (dark default).
// builderPRVerifier's returned closure carries no ctx (its signature is
// fixed — existing tests call it directly), so this uses context.Background;
// pullCheckRPCTimeout still bounds every RPC it triggers (review finding #2).
func recordPRExistsCheck(log *slog.Logger, repo, branch, ticket string, pass bool, summary, evidenceURL string) {
	if pullCheckRun == nil {
		return
	}
	pullCheckRun.record(context.Background(), log, repo, branch, ticket,
		pullCheckPRExists, pullCheckState(pass), summary, evidenceURL)
}

// recordPRSubstantialCheck records the pr-substantial gate's verdict
// (builderPRVerifier) against pullCheckRun. No-op when pullCheckRun is nil.
func recordPRSubstantialCheck(log *slog.Logger, repo, branch, ticket string, pass bool, floor int, summary string) {
	if pullCheckRun == nil {
		return
	}
	summary = fmt.Sprintf("%s (min_diff_lines=%d)", summary, floor)
	pullCheckRun.record(context.Background(), log, repo, branch, ticket,
		pullCheckPRSubstantial, pullCheckState(pass), summary, "")
}

// recordAcceptanceJudgeCheck records the acceptance-judge gate's verdict
// (builderAcceptanceGate.Decide) against run. No-op when run is nil (dark
// default) or the gate never actually ran the judge (hasCriteria/verify
// unavailable — nothing to record, same as the gate's own taskDoneHonor
// "fail open" path having no verdict.Reason to speak of). ctx is Decide's own
// context (review finding #2a) — NOT context.Background — so a canceled run
// aborts the RPC promptly instead of outliving it.
func recordAcceptanceJudgeCheck(ctx context.Context, run *pullRunRecorder, log *slog.Logger, repo, branch, ticket string, pass bool, summary, evidenceURL string) {
	if run == nil {
		return
	}
	run.record(ctx, log, repo, branch, ticket,
		pullCheckAcceptanceJudge, pullCheckState(pass), summary, evidenceURL)
}

// recordTestEvidenceCheck records the test-evidence gate's verdict
// (ACCEPTANCE-GATE-HARDENING Unit 3, builderAcceptanceGate.testEvidenceMissing)
// against run. Only called when Unit 3 actually evaluated (opted in via
// ACCEPTANCE_REQUIRE_TEST_DIFF=1, criteria mention tests, a PR diff was
// available) — see the call site in Decide. ctx is Decide's own context
// (review finding #2a).
func recordTestEvidenceCheck(ctx context.Context, run *pullRunRecorder, log *slog.Logger, repo, branch, ticket string, pass bool, summary string) {
	if run == nil {
		return
	}
	run.record(ctx, log, repo, branch, ticket,
		pullCheckTestEvidence, pullCheckState(pass), summary, "")
}
