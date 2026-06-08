package broker

import "github.com/CarriedWorldUniverse/nexus/nexus/frames"

func (c *wsConn) handleOperatorActivityHistory(env frames.Envelope) {
	if c.broker.activityReader == nil || c.broker.cfg.RunsStore == nil {
		c.operatorError(env, "activity history not configured")
		return
	}
	var p frames.ActivityHistoryPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "activity.history: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	run, err := c.broker.cfg.RunsStore.Get(ctx, p.RunID)
	if err != nil {
		c.operatorError(env, "activity.history: unknown run")
		return
	}
	acts, err := c.broker.activityReader.ReadByRun(ctx, run.Agent, p.RunID, p.Limit)
	partial := err != nil
	items := make([]frames.ActivityItemPayload, 0, len(acts))
	for _, f := range acts {
		items = append(items, frames.ActivityItemPayload{Type: activityType(f)})
	}
	resp, _ := frames.NewResponse(frames.KindActivityHistoryResult, env.ID,
		frames.ActivityHistoryResultPayload{Items: items, Partial: partial})
	c.send(resp)
}
