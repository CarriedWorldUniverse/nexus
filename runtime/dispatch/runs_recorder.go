package dispatch

import (
	"context"
	"time"
)

// RunsRecorder persists run lifecycle. Primitive params keep dispatch free of a
// store-package import. The broker adapts nexus/runs.Store to this.
type RunsRecorder interface {
	RecordRunStart(ctx context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64)
	RecordRunDone(ctx context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int)
}

func statusFor(ok bool) string {
	if ok {
		return "complete"
	}
	return "failed"
}

func prURLForRun(run *Run) string {
	if run == nil {
		return ""
	}
	branch := run.Brief.Branch
	if branch == "" {
		branch = "builder/" + run.Brief.Ticket
	}
	prURL, err := lookupPRURL(run.Brief.Repo, branch)
	if err != nil {
		return ""
	}
	return prURL
}
