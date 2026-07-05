package orchestrator

import (
	"context"
	"fmt"
)

// ReapStale scans workerstatus.Store for rows whose heartbeat has gone
// stale (Status.Stale(now, o.staleAfter())) and requeues their bound work
// item (workgraph.Cancel(id, requeue=true, "stale heartbeat")) — a stale
// worker's item goes back to StatusQueued so a later drain redispatches it
// to a fresh slot (PHASE2-DESIGN §2.1 "Orchestrator automation").
//
// Recovery-before-escalation: a work item alerts only on its SECOND
// consecutive stale-and-reaped strike, tracked in the one piece of
// in-process state this package keeps (o.strikes). Any worker row found
// NOT stale clears that work item's strike count — a redispatch that comes
// back healthy resets the counter, so escalation only fires when reaping
// alone isn't fixing it.
//
// Runs at the top of every DrainOnce (see drain.go) as well as being
// independently callable (e.g. a tighter reap-only cadence than the full
// drain, if ever wanted).
func (o *Orchestrator) ReapStale(ctx context.Context) ([]string, error) {
	statuses, err := o.WorkerStatus.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: ReapStale: list worker status: %w", err)
	}

	now := o.now()
	threshold := o.staleAfter()

	o.mu.Lock()
	if o.strikes == nil {
		o.strikes = map[string]int{}
	}
	o.mu.Unlock()

	var reaped []string
	var firstErr error
	for _, st := range statuses {
		if st.WorkItemID == "" {
			// Not bound to a work item (e.g. an idle/spawning row) —
			// nothing to reap.
			continue
		}
		if !st.Stale(now, threshold) {
			o.clearStrike(st.WorkItemID)
			continue
		}

		reason := fmt.Sprintf("stale heartbeat: agent=%s last_heartbeat=%s", st.Agent, st.LastHeartbeat)
		if err := o.Graph.Cancel(ctx, st.WorkItemID, true, reason); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("orchestrator: ReapStale: requeue %s: %w", st.WorkItemID, err)
			}
			continue
		}
		reaped = append(reaped, st.WorkItemID)

		strike := o.bumpStrike(st.WorkItemID)
		if strike >= 2 {
			o.alert(ctx, "orchestrator-stale-worker-second-strike",
				fmt.Sprintf("work item %s reaped again (strike %d): %s", st.WorkItemID, strike, reason))
		}
	}
	return reaped, firstErr
}

func (o *Orchestrator) bumpStrike(workItemID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.strikes[workItemID]++
	return o.strikes[workItemID]
}

func (o *Orchestrator) clearStrike(workItemID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.strikes, workItemID)
}
