package dispatch

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type K8s struct {
	Client    kubernetes.Interface
	Namespace string
}

func (k *K8s) EnsureKeyfileSecret(ctx context.Context, agent string) error {
	_, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, "aspect-keyfile-"+agent, metav1.GetOptions{})
	return err
}

func (k *K8s) PutBriefConfigMap(ctx context.Context, taskID, brief string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "brief-" + taskID,
			Namespace: k.Namespace,
			Labels:    map[string]string{"app": "nexus-builder"},
		},
		Data: map[string]string{"brief.md": brief},
	}
	_, err := k.Client.CoreV1().ConfigMaps(k.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

func (k *K8s) CreateJob(ctx context.Context, job *batchv1.Job) error {
	_, err := k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

// ListActiveJobs returns ticket -> job name for non-finished builder Jobs.
func (k *K8s) ListActiveJobs(ctx context.Context) (map[string]string, error) {
	jl, err := k.Client.BatchV1().Jobs(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range jl.Items {
		j := &jl.Items[i]
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			continue
		}
		if ticket := j.Labels["nexus.dispatch/ticket"]; ticket != "" {
			out[ticket] = j.Name
		}
	}
	return out, nil
}

func (k *K8s) WatchJobs(ctx context.Context, onDone func(ticket, thread string, ok bool)) error {
	w, err := k.Client.BatchV1().Jobs(k.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			j, ok := ev.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			ticket := j.Labels["nexus.dispatch/ticket"]
			thread := j.Annotations["nexus.dispatch/thread"]
			if thread == "" {
				thread = ticket
			}
			if ticket == "" {
				continue
			}
			if j.Status.Succeeded > 0 {
				onDone(ticket, thread, true)
			} else if j.Status.Failed > 0 {
				onDone(ticket, thread, false)
			}
		}
	}
}
