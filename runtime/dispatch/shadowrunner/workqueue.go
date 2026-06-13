// Package shadowrunner implements the coalescing trigger loop that drives
// shadow's stateless orchestrate drain (NEX-642). The workqueue here holds
// the ONLY loop state: one drain in flight + a pending bit. Pure logic so the
// coalescing semantics are unit-tested without timers or claude.
package shadowrunner

import "sync"

type wqState int

const (
	wqIdle wqState = iota
	wqRunning
	wqRunningPending
)

// Workqueue is the level-triggered coalescing workqueue: N triggers during a
// drain collapse to exactly one follow-up drain; a dropped trigger can't strand
// work because the next drain re-reads ledger truth (the design's
// level-triggered semantics).
type Workqueue struct {
	mu    sync.Mutex
	state wqState
}

func NewWorkqueue() *Workqueue { return &Workqueue{state: wqIdle} }

// Trigger records a wake. Returns true iff the caller should START a drain now
// (i.e. we were idle). While a drain runs, it only sets the pending bit.
func (q *Workqueue) Trigger() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch q.state {
	case wqIdle:
		q.state = wqRunning
		return true
	case wqRunning:
		q.state = wqRunningPending
		return false
	default: // wqRunningPending
		return false
	}
}

// Done marks the current drain finished. Returns true iff a follow-up drain
// should start immediately (a trigger arrived mid-drain).
func (q *Workqueue) Done() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.state == wqRunningPending {
		q.state = wqRunning
		return true
	}
	q.state = wqIdle
	return false
}

// Pending reports whether a follow-up is queued (for persistence on restart).
func (q *Workqueue) Pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state == wqRunningPending
}
