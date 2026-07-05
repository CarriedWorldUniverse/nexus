package orchestrator

import (
	"context"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
)

// ReapStale scans workerstatus.Store for rows whose heartbeat has gone
// stale (Status.Stale(now, o.staleAfter())) and requeues their bound work
// item (workgraph.Cancel(id, requeue=true, "stale heartbeat")) — a stale
// worker's item goes back to StatusQueued so a later drain redispatches it
// to a fresh slot (PHASE2-DESIGN §2.1 "Orchestrator automation").
//
// A stale heartbeat row is NOT, by itself, proof the item is still in
// flight (live-reproduced NET-30, 2026-07-05): worker_status rows are
// never deleted/closed just because a run ended (see
// runtime/dispatch/runner.go OnJobDone, which now retires them on
// completion, but a row can still lag or predate that fix). So before
// requeueing, ReapStale re-checks the item's CURRENT ledger status
// (o.Graph.GetWorkItem) and only requeues an item genuinely
// StatusDispatched. An item that is queued/done/blocked/cancelled in the
// ledger is left alone — its confirmed-stale-and-harmless row is deleted
// as a cleanup candidate instead, so it is never reasoned about (or
// mis-requeued) again. This is also what breaks the reap<->cancel
// requeue loop: once an item IS requeued (StatusQueued), its still-stale
// row from the old run no longer matches StatusDispatched on the next
// pass, so it stops being reaped and gets cleaned up instead of
// requeued forever.
//
// Dedupe: two stale rows can be bound to the same work item (e.g. two
// finished runs of it, as seen live) — at most one Cancel/requeue per
// work item per pass.
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
	requeuedThisPass := map[string]bool{}
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

		wi, err := o.Graph.GetWorkItem(ctx, st.WorkItemID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("orchestrator: ReapStale: get work item %s: %w", st.WorkItemID, err)
			}
			continue
		}
		if wi.Status != workgraph.StatusDispatched {
			// Not in flight per the ledger — never requeue. The row is a
			// confirmed-stale, confirmed-harmless leftover (a finished,
			// cancelled, or blocked item's worker never got its row
			// retired) — clean it up so it stops showing up as "stale"
			// on every future pass, and clear any strike it was carrying.
			if delErr := o.WorkerStatus.Delete(ctx, st.Agent); delErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("orchestrator: ReapStale: cleanup stale row agent=%s: %w", st.Agent, delErr)
			}
			o.clearStrike(st.WorkItemID)
			continue
		}

		if requeuedThisPass[st.WorkItemID] {
			// Dedupe: another stale row already requeued this same work
			// item this pass — one requeue per item per pass.
			continue
		}

		reason := fmt.Sprintf("stale heartbeat: agent=%s last_heartbeat=%s", st.Agent, st.LastHeartbeat)
		if err := o.Graph.Cancel(ctx, st.WorkItemID, true, reason); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("orchestrator: ReapStale: requeue %s: %w", st.WorkItemID, err)
			}
			continue
		}
		requeuedThisPass[st.WorkItemID] = true
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
