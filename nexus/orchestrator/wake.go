package orchestrator

import (
	"context"
	"log/slog"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// OnJobDoneHook returns a dispatch.Runner.OnJobDoneHook-shaped function
// (PHASE2-DESIGN §2 "wake triggers: OnJobDone completion hook — primary,
// hop latency becomes seconds") — wire it directly:
//
//	runner.OnJobDoneHook = orch.OnJobDoneHook()
//
// dispatch.JobDone carries only Ticket (== the pool work-item id,
// SubmitPool/SubmitPoolItem stamp Ticket=WorkItemID) and an OK bool, not a
// full workgraph.Result — there is no richer worker-reported verdict
// (pass/reject/blocked + reasons/artifacts) channel in this codebase yet
// (see README.md "OnJobDone wake: OK-bool is a stand-in"). This hook is
// therefore the FALLBACK translation: OK=true -> VerdictDone, OK=false ->
// VerdictBlocked (escalate). A richer channel, once built, should call
// RecordJobResult directly with the worker's real verdict instead of
// going through this hook — RecordJobResult is exported for exactly that.
//
// A JobDone with an empty Ticket (not a pool dispatch this orchestrator
// tracks) is ignored.
func (o *Orchestrator) OnJobDoneHook() func(dispatch.JobDone) {
	return func(done dispatch.JobDone) {
		if done.Ticket == "" {
			return
		}
		ctx := context.Background()

		// NEX-473: re-run the authoritative gates BEFORE trusting done.OK —
		// synchronous-on-job-done, so a later drain/dependent-item decision
		// never runs ahead of ground-truth verification. Dark by default
		// (o.GateRunner nil): zero gh/judge calls, byte-identical to
		// pre-#473 behavior. #474 will additionally record these verdicts
		// as durable cairn pull checks; for now they are only logged (see
		// RunAuthoritativeGates/LogVerdicts doc in gates.go).
		o.runAuthoritativeGates(ctx, done.Ticket)

		result := workgraph.Result{
			WorkItemID: done.Ticket,
			Verdict:    workgraph.VerdictDone,
		}
		if !done.OK {
			result.Verdict = workgraph.VerdictBlocked
			result.Reasons = []string{"job did not complete successfully"}
		}
		if _, err := o.RecordJobResult(ctx, done.Ticket, result); err != nil {
			slog.Error("orchestrator: OnJobDoneHook: RecordJobResult failed", "work_item", done.Ticket, "err", err)
		}
	}
}

// runAuthoritativeGates is OnJobDoneHook's #473 wiring, split out so it's
// independently testable: it looks up the work item's repo/criteria, derives
// its builder branch (the builder/<ticket> convention — see
// runtime/cmd/agentfunnel builderBranch), runs RunAuthoritativeGates, and
// slogs the verdicts. A no-op (zero gh/judge calls) whenever o.GateRunner is
// nil (dark default) or the work item carries no Repo (respond-only work,
// no PR to gate — mirrors agentfunnel's own Repo=="" short-circuit).
func (o *Orchestrator) runAuthoritativeGates(ctx context.Context, ticket string) {
	if o.GateRunner == nil {
		return
	}
	item, err := o.Graph.GetWorkItem(ctx, ticket)
	if err != nil {
		slog.Warn("orchestrator: authoritative gates: GetWorkItem failed — skipping", "work_item", ticket, "err", err)
		return
	}
	if item.Repo == "" {
		return
	}
	branch := "builder/" + ticket
	criteria := strings.Join(item.AcceptanceCriteria, "\n")

	verdicts, err := RunAuthoritativeGates(ctx, item.Repo, branch, ticket, criteria, *o.GateRunner)
	if err != nil {
		slog.Error("orchestrator: authoritative gates: run failed", "work_item", ticket, "repo", item.Repo, "branch", branch, "err", err)
		return
	}
	LogVerdicts(slog.Default(), ticket, verdicts)
}
