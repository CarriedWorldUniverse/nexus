package broker

import (
	"context"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func envHealthSnapshot(ctx context.Context, cs kubernetes.Interface, ns string) (frames.EnvHealthResultPayload, error) {
	var out frames.EnvHealthResultPayload
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}
	wanted := map[string]string{"nexus-broker": "broker", "sqld": "sqld", "gemma": "gemma"}
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
	return out, nil
}

func (c *wsConn) handleOperatorEnvHealth(env frames.Envelope) {
	cs := c.broker.k8sReader
	if cs == nil {
		c.operatorError(env, "env.health not available (no in-cluster client)")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	snap, err := envHealthSnapshot(ctx, cs, c.broker.k8sNamespace)
	if err != nil {
		c.operatorError(env, "env.health: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindEnvHealthResult, env.ID, snap)
	c.send(resp)
}
