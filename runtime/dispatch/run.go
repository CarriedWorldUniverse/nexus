package dispatch

// Run tracks one in-flight dispatch job.
type Run struct {
	ID       string
	ParentID string
	Brief    Brief
	JobName  string
	PoolSlot string
}

// RunResult is the outcome of a completed run.
type RunResult struct {
	RunID  string
	OK     bool
	Thread string
}
