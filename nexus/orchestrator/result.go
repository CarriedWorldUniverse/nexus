package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
)

// RecordJobResult is the orchestrator's result-intake half of a completion
// (PHASE2-DESIGN §2 "wake"): it records result on the graph, advances the
// item per its verdict, then re-drains — a done result may have unblocked
// dependents; a reject files a rework follow-up that is itself now queued.
//
//   - VerdictDone: workgraph.RecordResult transitions the item to Done
//     (ledger-side, see workgraph's RecordResult).
//   - VerdictReject: workgraph.RecordResult leaves the item's status alone
//     (ledger has no "rejected" state); RecordJobResult additionally calls
//     workgraph.Rework to create the follow-up work item, carrying forward
//     the rejected item's acceptance criteria and cairn line.
//   - VerdictBlocked: workgraph.RecordResult transitions the item to
//     Blocked; RecordJobResult alerts (operator escalation, PHASE2-DESIGN
//     §1 "blocked -> escalate to operator").
//   - VerdictPass: workgraph.RecordResult leaves the item's status alone —
//     passing a gate doesn't terminate the item, the next role picks it up
//     once a later hop marks it done.
//
// Returns the re-drain's DrainReport so a caller can see what the result
// unblocked.
func (o *Orchestrator) RecordJobResult(ctx context.Context, workItemID string, result workgraph.Result) (DrainReport, error) {
	result.WorkItemID = workItemID

	if err := o.Graph.RecordResult(ctx, workItemID, result); err != nil {
		return DrainReport{}, fmt.Errorf("orchestrator: RecordJobResult: record result for %s: %w", workItemID, err)
	}

	switch result.Verdict {
	case workgraph.VerdictReject:
		old, err := o.Graph.GetWorkItem(ctx, workItemID)
		if err != nil {
			return DrainReport{}, fmt.Errorf("orchestrator: RecordJobResult: get rejected item %s: %w", workItemID, err)
		}
		newSpec := workgraph.WorkItem{
			TaskSpec:           reworkTaskSpec(workItemID, result),
			AcceptanceCriteria: old.AcceptanceCriteria,
			CairnLine:          old.CairnLine,
		}
		if _, err := o.Graph.Rework(ctx, workItemID, newSpec); err != nil {
			return DrainReport{}, fmt.Errorf("orchestrator: RecordJobResult: rework %s: %w", workItemID, err)
		}
	case workgraph.VerdictBlocked:
		o.alert(ctx, "orchestrator-work-item-blocked",
			fmt.Sprintf("work item %s blocked: %s", workItemID, strings.Join(result.Reasons, "; ")))
	}

	report, err := o.DrainOnce(ctx)
	if err != nil {
		return report, fmt.Errorf("orchestrator: RecordJobResult: re-drain: %w", err)
	}
	return report, nil
}

// reworkTaskSpec derives the follow-up item's task_spec from the rejecting
// result's reasons — mechanical relaying of the rejection, not a judgment
// about what new work to create (that stays out of scope; see package
// doc). An empty Reasons list still produces a usable task_spec.
func reworkTaskSpec(workItemID string, result workgraph.Result) string {
	if len(result.Reasons) == 0 {
		return fmt.Sprintf("Rework of %s: prior attempt rejected (no reasons recorded).", workItemID)
	}
	return fmt.Sprintf("Rework of %s: address rejection — %s", workItemID, strings.Join(result.Reasons, "; "))
}
