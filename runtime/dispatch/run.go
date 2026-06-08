package dispatch

import "time"

// Run tracks one in-flight dispatch job.
type Run struct {
	ID       string
	ParentID string
	Brief    Brief
	JobName  string
	Started  time.Time
}

// JobDone is the terminal signal emitted by the Kubernetes job watch.
type JobDone struct {
	Ticket      string
	Thread      string
	Agent       string
	OK          bool
	StartedAt   time.Time
	CompletedAt time.Time
}
