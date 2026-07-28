// #474 (cairn#99 Option B, final unit) — this file MOVES the cairn
// pull-check recorder from the worker pod to HERE, the orchestrator, and
// wires it to RunAuthoritativeGates' verdicts (gates.go). Before this unit,
// runtime/cmd/agentfunnel's builder gates recorded their own verdicts as
// cairn pull checks (#468) — a run-scoped mesh credential (CW_PULL_*) sat in
// the WORKER pod's env, the exact surface a gated model can shell out from.
// cairn#99's invariant is that an attester must run where the gated code
// cannot read its credential: the worker no longer records anything (see
// runtime/cmd/agentfunnel/main.go's advisory-only gates and #474's removal
// of builder_pullchecks.go), and CW_PULL_* is no longer forwarded onto a
// worker Job (runtime/dispatch/jobspec.go acceptanceGateEnvKeys). Recording
// now happens ONLY here, driven by the SAME orchestrator process that
// already re-runs the gates against ground truth (RunAuthoritativeGates) —
// see docs/network/ACCEPTANCE-GATE-HARDENING.md.
package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/pullchecks"
)

// PullCheckRecorder is the narrow slice of runtime/pullchecks.Recorder's API
// RecordVerdicts uses — declared locally (mirrors this codebase's other
// narrowed-interface convention, e.g. WorkGraph/Dispatcher above) so tests
// can supply a fake without standing up a real cairn-server, and so this
// package depends on pullchecks only for the concrete *pullchecks.Recorder
// constructor, not for every method it exposes. *pullchecks.Recorder
// satisfies this structurally.
type PullCheckRecorder interface {
	EnsurePull(ctx context.Context, source, target, title, project string) (string, error)
	Record(ctx context.Context, pullID, name, state, summary, evidenceURL string) error
}

var _ PullCheckRecorder = (*pullchecks.Recorder)(nil)

// pullRecordRPCTimeout bounds every individual EnsurePull/RecordPullCheck RPC
// RecordVerdicts issues — mirrors authGateRPCTimeout's rationale (gates.go):
// OnJobDoneHook runs inside WatchJobs' single-goroutine select loop with no
// external kill switch, so a hung/unreachable cairn-server must only ever
// eat its own bounded budget, never wedge job-done processing for every
// builder in the namespace. A var (not const) so tests can shrink it rather
// than sleeping 5s+ per case.
var pullRecordRPCTimeout = 5 * time.Second

// NewPullRecorderFromEnv builds the orchestrator's cairn pull-check recorder
// from CW_PULL_* env (see pullchecks.NewRecorderFromEnv for the full env
// reference) — dark by default: CW_PULL_SERVER_ADDR unset returns a nil
// interface, and a nil Orchestrator.PullRecorder is RecordVerdicts' contract
// for "make zero PullService calls" (preserving RunAuthoritativeGates'/
// LogVerdicts' pre-#474 behavior exactly for any caller that never wires
// this).
//
// Unlike the worker's now-removed buildPullCheckRun (builder_pullchecks.go,
// deleted by #474), this carries no builder-mode CW_PULL_DEV_INSECURE fatal
// guard: that guard existed because a builder process runs inside the very
// pod the gated model can shell out from — dialing insecure there would let
// any shell in that pod forge a gate pass. The orchestrator process is the
// TRUSTED side of the separation of duties this relocation establishes, not
// the gated surface, so CW_PULL_DEV_INSECURE=1 here is an ordinary
// local-dev opt-in (see pullchecks.DialCreds' own doc) — same posture as
// every other CW_PULL_* consumer that isn't a worker.
func NewPullRecorderFromEnv(log *slog.Logger) PullCheckRecorder {
	rec := pullchecks.NewRecorderFromEnv(log)
	if rec == nil {
		// A nil *pullchecks.Recorder assigned to the PullCheckRecorder
		// interface would NOT compare == nil (typed-nil-in-interface) — return
		// an explicit untyped nil so callers' `== nil` checks (RecordVerdicts,
		// Orchestrator.PullRecorder) work as intended.
		return nil
	}
	return rec
}

// pullRecordTarget resolves the target branch EnsurePull links the run's
// pull against. CW_PULL_TARGET, when set, overrides resolution entirely —
// same operator escape hatch the (now-removed) worker-side wiring offered.
// Otherwise falls back to "main": unlike the worker's pullCheckTarget (which
// shelled out to `gh repo view` for the repo's actual default branch), this
// deliberately does NOT add another gh subprocess to the job-done path —
// RunAuthoritativeGates already bounds its own gh calls tightly
// (authGateRPCTimeout), and a wrong target branch only ever affects
// OpenPull's linkage metadata, never any gate's own pass/fail decision.
func pullRecordTarget() string {
	if v := strings.TrimSpace(os.Getenv("CW_PULL_TARGET")); v != "" {
		return v
	}
	return "main"
}

// RecordVerdicts records RunAuthoritativeGates' verdicts as durable cairn
// pull checks: EnsurePull once, then one Record call per verdict — the
// orchestrator-side counterpart of the worker's removed pullRunRecorder
// (builder_pullchecks.go). Dark/best-effort by design:
//
//   - rec == nil (Orchestrator.PullRecorder unset, or NewPullRecorderFromEnv
//     found no CW_PULL_* config) makes ZERO PullService calls — the dark
//     default RunAuthoritativeGates/LogVerdicts already guarantee is
//     unaffected by this function existing.
//   - len(verdicts) == 0 (RunAuthoritativeGates disabled, or every gate's
//     precondition unmet) is also a no-op — nothing to record.
//   - Any RPC failure (EnsurePull or an individual RecordPullCheck) is
//     logged loudly and swallowed: RecordVerdicts never returns an error and
//     never blocks/derails OnJobDoneHook's own job-done bookkeeping — the
//     broker gate's own pass/fail decision (already applied via done.OK/
//     RecordJobResult) is authoritative regardless of whether recording it
//     durably succeeded.
func RecordVerdicts(ctx context.Context, rec PullCheckRecorder, log *slog.Logger, repo, branch, ticket string, verdicts []GateVerdict) {
	if rec == nil || len(verdicts) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	title := ticket
	if title == "" {
		title = branch
	}

	rpcCtx, cancel := context.WithTimeout(ctx, pullRecordRPCTimeout)
	pullID, err := rec.EnsurePull(rpcCtx, branch, pullRecordTarget(), "authoritative gate checks: "+title, "")
	cancel()
	if err != nil {
		// rec.EnsurePull (pullchecks.Recorder.EnsurePull) already slog.Error's
		// the details — nothing more to log here, and no verdict can be
		// recorded without a pull id.
		return
	}

	for _, v := range verdicts {
		rpcCtx, cancel := context.WithTimeout(ctx, pullRecordRPCTimeout)
		rerr := rec.Record(rpcCtx, pullID, v.Name, v.State, v.Summary, v.EvidenceURL)
		cancel()
		if rerr != nil {
			// rec.Record (pullchecks.Recorder.Record) already slog.Error's its
			// own details; this line adds the work-item/gate context a bare
			// Recorder call can't know.
			log.Error("orchestrator: RecordVerdicts: RecordPullCheck failed",
				"work_item", ticket, "repo", repo, "gate", v.Name, "state", v.State, "err", rerr)
		}
	}
}
