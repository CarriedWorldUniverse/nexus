package broker

import (
	"sort"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func activityType(f observability.Frame) string {
	switch f.Kind {
	case observability.FrameTurn:
		return "turn"
	case observability.FramePresence:
		return "presence"
	case observability.FrameFilterDecision:
		return "thought"
	default:
		return string(f.Kind)
	}
}

// mergeTimeline interleaves chat messages and activity frames by time. Ties
// order chat before activity because the command precedes the work it triggers.
func mergeTimeline(msgs []chat.Message, acts []observability.Frame) []frames.TimelineItemPayload {
	out := make([]frames.TimelineItemPayload, 0, len(msgs)+len(acts))
	for _, m := range msgs {
		out = append(out, frames.TimelineItemPayload{
			Kind: "chat",
			At:   m.CreatedAt.UnixMilli(),
			Chat: &frames.ChatItemPayload{
				MsgID:   m.ID,
				From:    m.From,
				Content: m.Content,
				ReplyTo: m.ReplyTo,
			},
		})
	}
	for _, f := range acts {
		out = append(out, frames.TimelineItemPayload{
			Kind:     "activity",
			At:       f.TS.UnixMilli(),
			Activity: &frames.ActivityItemPayload{Type: activityType(f)},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].Kind == "chat" && out[j].Kind == "activity"
	})
	return out
}

func (c *wsConn) handleOperatorRunsList(env frames.Envelope) {
	store := c.broker.cfg.RunsStore
	if store == nil {
		c.operatorError(env, "runs store not configured")
		return
	}
	var p frames.RunsListPayload
	_ = frames.PayloadAs(env, &p)
	ctx, cancel := c.opCtx()
	defer cancel()
	rs, err := store.List(ctx, p.Limit)
	if err != nil {
		c.operatorError(env, "runs.list: "+err.Error())
		return
	}
	out := make([]frames.RunPayload, 0, len(rs))
	for _, r := range rs {
		out = append(out, runToPayload(r))
	}
	resp, _ := frames.NewResponse(frames.KindRunsListResult, env.ID, frames.RunsListResultPayload{Runs: out})
	c.send(resp)
}

func (c *wsConn) handleOperatorRunGet(env frames.Envelope) {
	store := c.broker.cfg.RunsStore
	if store == nil {
		c.operatorError(env, "runs store not configured")
		return
	}
	var p frames.RunGetPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "run.get: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	run, err := store.Get(ctx, p.RunID)
	if err != nil {
		resp, _ := frames.NewResponse(frames.KindRunGetResult, env.ID, frames.RunGetResultPayload{
			Run: frames.RunPayload{RunID: p.RunID, Status: "unknown"},
		})
		c.send(resp)
		return
	}

	var msgs []chat.Message
	if cs := c.broker.cfg.ChatStore; cs != nil && run.DispatchMsgID > 0 {
		msgs, _ = cs.ListThread(ctx, run.DispatchMsgID, 0, 1000)
	}
	var acts []observability.Frame
	partial := false
	if c.broker.activityReader != nil {
		acts, err = c.broker.activityReader.ReadByRun(ctx, run.Agent, run.RunID, 2000)
		if err != nil {
			partial = true
		}
	}
	resp, _ := frames.NewResponse(frames.KindRunGetResult, env.ID, frames.RunGetResultPayload{
		Run:      runToPayload(run),
		Timeline: mergeTimeline(msgs, acts),
		Partial:  partial,
	})
	c.send(resp)
}
