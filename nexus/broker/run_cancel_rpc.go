package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const cancelGraceSecs int64 = 30

// resolveCancelTarget finds the builder Job for a run_id by label and returns
// its name plus the grace period to use: 0 for force, cancelGraceSecs otherwise.
func resolveCancelTarget(ctx context.Context, cs kubernetes.Interface, ns, runID string, force bool) (string, *int64, error) {
	jl, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "nexus.dispatch/run-id=" + runID,
	})
	if err != nil {
		return "", nil, err
	}
	if len(jl.Items) == 0 {
		return "", nil, fmt.Errorf("no active job for run %s", runID)
	}
	grace := cancelGraceSecs
	if force {
		grace = 0
	}
	return jl.Items[0].Name, &grace, nil
}

func (c *wsConn) handleOperatorRunCancel(env frames.Envelope) {
	if c.broker.k8sReader == nil || c.broker.dispatchK8s == nil {
		c.operatorError(env, "run.cancel unavailable (no in-cluster client)")
		return
	}
	var p frames.RunCancelPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "run.cancel: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()

	name, grace, err := resolveCancelTarget(ctx, c.broker.k8sReader, c.broker.k8sNamespace, p.RunID, p.Force)
	if err != nil {
		// Already finished/aged-out: mark cancelled best-effort and report ok.
		if c.broker.cfg.RunsStore != nil {
			_ = c.broker.cfg.RunsStore.MarkDone(ctx, p.RunID, runs.StatusCancelled, time.Now(), "", 0)
		}
		resp, _ := frames.NewResponse(frames.KindRunCancelResult, env.ID, frames.RunCancelResultPayload{OK: true, Message: "run already ended"})
		c.send(resp)
		return
	}
	// Mark cancelled first so the async emitJobDeleted failed-mark is a no-op.
	if c.broker.cfg.RunsStore != nil {
		_ = c.broker.cfg.RunsStore.MarkDone(ctx, p.RunID, runs.StatusCancelled, time.Now(), "", 0)
	}
	if err := c.broker.dispatchK8s.DeleteJob(ctx, name, grace); err != nil {
		c.operatorError(env, "run.cancel: delete job: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindRunCancelResult, env.ID, frames.RunCancelResultPayload{OK: true, Message: "cancelled"})
	c.send(resp)
}
