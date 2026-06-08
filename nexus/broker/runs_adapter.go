package broker

import (
	"context"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
)

// runsAdapter bridges the dispatch runner's RunsRecorder to the runs.Store and
// emits an onChange callback used to push runs.update to operators.
type runsAdapter struct {
	store    runs.Store
	onChange func(runs.Run)
}

func newRunsAdapter(store runs.Store, onChange func(runs.Run)) *runsAdapter {
	return &runsAdapter{store: store, onChange: onChange}
}

func (a *runsAdapter) RecordRunStart(ctx context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64) {
	r := runs.Run{
		RunID:         runID,
		Ticket:        ticket,
		Agent:         agent,
		Thread:        thread,
		Repo:          repo,
		Command:       command,
		ParentRunID:   parentRunID,
		DispatchMsgID: dispatchMsgID,
		Status:        runs.StatusRunning,
		StartedAt:     time.Now(),
	}
	_ = a.store.Insert(ctx, r)
	if a.onChange != nil {
		a.onChange(r)
	}
}

func (a *runsAdapter) RecordRunDone(ctx context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int) {
	_ = a.store.MarkDone(ctx, runID, runs.Status(status), completedAt, prURL, durationSecs)
	if a.onChange != nil {
		if r, err := a.store.Get(ctx, runID); err == nil {
			a.onChange(r)
		}
	}
}

func (b *Broker) broadcastRunsUpdate(r runs.Run) {
	b.opMu.RLock()
	targets := make([]*wsConn, 0, len(b.operators))
	for c := range b.operators {
		targets = append(targets, c)
	}
	b.opMu.RUnlock()

	frame, err := frames.NewResponse(frames.KindRunsUpdate, "", runToPayload(r))
	if err != nil {
		b.log.Warn("runs.update: build failed", "run_id", r.RunID, "err", err)
		return
	}
	for _, c := range targets {
		c.send(frame)
	}
}

func runToPayload(r runs.Run) frames.RunPayload {
	p := frames.RunPayload{
		RunID:         r.RunID,
		Ticket:        r.Ticket,
		Agent:         r.Agent,
		Thread:        r.Thread,
		DispatchMsgID: r.DispatchMsgID,
		Command:       r.Command,
		Repo:          r.Repo,
		Status:        string(r.Status),
		StartedAt:     r.StartedAt.UnixMilli(),
		PRURL:         r.PRURL,
		DurationSecs:  r.DurationSecs,
		ParentRunID:   r.ParentRunID,
	}
	if !r.CompletedAt.IsZero() {
		p.CompletedAt = r.CompletedAt.UnixMilli()
	}
	return p
}
