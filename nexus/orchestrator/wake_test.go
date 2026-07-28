package orchestrator

import (
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// NEX-818: a spawned hand's ticket is synthetic ("hand-<run-id>", minted by
// dispatch's handTicket) and is never filed in the ledger, so recording a
// result against it fails with `ledger: issue not found`. That error buried
// three hand outcomes on 2026-07-23. The hook must skip hands entirely —
// they report to their parent through the dispatch completion summary, and
// the work graph has nothing to say about them.
func TestOnJobDoneHookIgnoresHandTickets(t *testing.T) {
	graph := newFakeGraph()
	o := &Orchestrator{Graph: graph}

	o.OnJobDoneHook()(dispatch.JobDone{
		Ticket: dispatch.HandTicketPrefix + "a75deecc-6a28-457f-bfcf-1951e584895b",
		OK:     false,
	})

	graph.mu.Lock()
	defer graph.mu.Unlock()
	if len(graph.results) != 0 {
		t.Fatalf("RecordResult was called for a hand ticket (%d entr(ies)); hands are not work items", len(graph.results))
	}
}

// The hand guard must not swallow ordinary pool dispatches: a REAL work-item
// ticket is still recorded exactly as before.
func TestOnJobDoneHookStillRecordsWorkItems(t *testing.T) {
	graph := newFakeGraph()
	o := &Orchestrator{Graph: graph}

	o.OnJobDoneHook()(dispatch.JobDone{Ticket: "NEX-1234", OK: true})

	graph.mu.Lock()
	defer graph.mu.Unlock()
	if len(graph.results["NEX-1234"]) != 1 {
		t.Fatalf("work-item ticket was not recorded: results = %+v", graph.results)
	}
}
