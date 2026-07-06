package orchestrator

import (
	"context"
	"log/slog"

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
		result := workgraph.Result{
			WorkItemID: done.Ticket,
			Verdict:    workgraph.VerdictDone,
		}
		if !done.OK {
			result.Verdict = workgraph.VerdictBlocked
			result.Reasons = []string{"job did not complete successfully"}
		}
		if _, err := o.RecordJobResult(context.Background(), done.Ticket, result); err != nil {
			slog.Error("orchestrator: OnJobDoneHook: RecordJobResult failed", "work_item", done.Ticket, "err", err)
		}
	}
}
