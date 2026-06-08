package broker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// envHealthSnapshot reports the in-namespace pods (broker, gemma — both in the
// broker's namespace) and PVCs, plus sqld via a reachability probe. sqld runs
// in a different namespace (cwb) and the broker's Role is namespaced, so we
// check sqld by connection health (the broker's own sqld DB) rather than a
// cross-namespace pod read — listing it in `wanted` here only ever yielded a
// false "not found" that read as a storage outage (NEX-533).
func envHealthSnapshot(ctx context.Context, cs kubernetes.Interface, ns string, sqldPing func(context.Context) error) (frames.EnvHealthResultPayload, error) {
	var out frames.EnvHealthResultPayload
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}
	wanted := map[string]string{"nexus-broker": "broker", "gemma": "gemma"}
	seen := map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		out.PodsTotal++
		running := p.Status.Phase == corev1.PodRunning
		if running {
			out.PodsRunning++
		}
		for prefix, name := range wanted {
			if strings.HasPrefix(p.Name, prefix) && !seen[name] {
				seen[name] = true
				out.Components = append(out.Components, frames.EnvComponentPayload{
					Name: name, Kind: "pod", Healthy: running, Detail: string(p.Status.Phase),
				})
			}
		}
	}
	for _, name := range wanted {
		if !seen[name] {
			out.Components = append(out.Components, frames.EnvComponentPayload{
				Name: name, Kind: "pod", Healthy: false, Detail: "not found",
			})
		}
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range pvcs.Items {
			pv := &pvcs.Items[i]
			out.PVCs = append(out.PVCs, frames.EnvPVCPayload{Name: pv.Name, Status: string(pv.Status.Phase)})
		}
	}
	out.Components = append(out.Components, sqldComponent(ctx, sqldPing))
	return out, nil
}

// sqldComponent reports sqld health by reachability (the broker's sqld
// connection), not pod presence.
func sqldComponent(ctx context.Context, sqldPing func(context.Context) error) frames.EnvComponentPayload {
	c := frames.EnvComponentPayload{Name: "sqld", Kind: "db"}
	switch {
	case sqldPing == nil:
		c.Healthy, c.Detail = false, "no connection configured"
	default:
		if err := sqldPing(ctx); err != nil {
			c.Healthy, c.Detail = false, "unreachable: "+err.Error()
		} else {
			c.Healthy, c.Detail = true, "reachable"
		}
	}
	return c
}

// sqldPing reports whether the broker's sqld connection is reachable.
func (b *Broker) sqldPing(ctx context.Context) error {
	if b.cfg.SQLDB == nil {
		return errors.New("no sqld connection")
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return b.cfg.SQLDB.PingContext(pctx)
}

func (c *wsConn) handleOperatorEnvHealth(env frames.Envelope) {
	cs := c.broker.k8sReader
	if cs == nil {
		c.operatorError(env, "env.health not available (no in-cluster client)")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	snap, err := envHealthSnapshot(ctx, cs, c.broker.k8sNamespace, c.broker.sqldPing)
	if err != nil {
		c.operatorError(env, "env.health: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindEnvHealthResult, env.ID, snap)
	c.send(resp)
}
