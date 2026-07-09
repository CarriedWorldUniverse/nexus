package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

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
// first use and caching it for every subsequent gate this process records.
// repo/branch/ticket give the (source, target) pair the same way
// builderPRVerifier/builderAcceptanceGate already resolve them
// (builderBranch(branch, ticket) as source, "main" as target — mirroring
// prCreateFn's --base main convention). ok=false means either p is nil
// (dark) or EnsurePull failed (already logged loudly by the Recorder) — a
// caller sees ok=false and simply skips recording, never fails the run.
func (p *pullRunRecorder) ensurePull(ctx context.Context, log *slog.Logger, repo, branch, ticket string) (string, bool) {
	if p == nil || repo == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ensured {
		return p.pullID, p.pullID != ""
	}
	p.ensured = true
	source := builderBranch(branch, ticket)
	if source == "" {
		return "", false
	}
	title := ticket
	if title == "" {
		title = source
	}
	id, err := p.rec.EnsurePull(ctx, source, "main", "builder gate checks: "+title, "")
	if err != nil {
		// Recorder.EnsurePull already slog.Error'd the details; nothing more
		// to do here — best-effort, never fails the run (ACCEPTANCE-GATE-
		// HARDENING pull-checks wiring failure policy).
		return "", false
	}
	p.pullID = id
	return id, true
}

// record upserts one gate's verdict as a cairn pull check on this run's
// pull. Best-effort: p nil, ensurePull failing, or the RecordPullCheck RPC
// itself failing are all swallowed here (Recorder.Record already
// slog.Error's the details) — a pull-checks outage NEVER fails the run; the
// broker gate's own pass/fail decision is already authoritative broker-side.
func (p *pullRunRecorder) record(ctx context.Context, log *slog.Logger, repo, branch, ticket, name, state, summary, evidenceURL string) {
	if p == nil {
		return
	}
	pullID, ok := p.ensurePull(ctx, log, repo, branch, ticket)
	if !ok {
		return
	}
	_ = p.rec.Record(ctx, pullID, name, state, summary, evidenceURL)
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
// "fail open" path having no verdict.Reason to speak of).
func recordAcceptanceJudgeCheck(run *pullRunRecorder, log *slog.Logger, repo, branch, ticket string, pass bool, summary, evidenceURL string) {
	if run == nil {
		return
	}
	run.record(context.Background(), log, repo, branch, ticket,
		pullCheckAcceptanceJudge, pullCheckState(pass), summary, evidenceURL)
}

// recordTestEvidenceCheck records the test-evidence gate's verdict
// (ACCEPTANCE-GATE-HARDENING Unit 3, builderAcceptanceGate.testEvidenceMissing)
// against run. Only called when Unit 3 actually evaluated (opted in via
// ACCEPTANCE_REQUIRE_TEST_DIFF=1, criteria mention tests, a PR diff was
// available) — see the call site in Decide.
func recordTestEvidenceCheck(run *pullRunRecorder, log *slog.Logger, repo, branch, ticket string, pass bool, summary string) {
	if run == nil {
		return
	}
	run.record(context.Background(), log, repo, branch, ticket,
		pullCheckTestEvidence, pullCheckState(pass), summary, "")
}
