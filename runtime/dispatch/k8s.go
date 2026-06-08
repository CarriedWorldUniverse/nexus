package dispatch

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type K8s struct {
	Client    kubernetes.Interface
	Namespace string
}

// NewInClusterK8s builds a K8s client from the pod's in-cluster service
// account. Recovered from the former dispatch-controller main (deleted with
// the controller in #274) so the broker-inline Runner can spawn Jobs. Only
// valid when running as a pod inside the cluster — callers gate on that
// (e.g. KUBERNETES_SERVICE_HOST set) and fall back to a nil K8sIface
// (no-spawn) for dev/test/non-k8s boots.
func NewInClusterK8s(namespace string) (*K8s, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("dispatch: in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("dispatch: k8s client: %w", err)
	}
	return &K8s{Client: cs, Namespace: namespace}, nil
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

func (k *K8s) CreateJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	return k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
}

// SetBriefOwner makes the Job own the brief ConfigMap so it is garbage-collected
// when the Job is removed (TTLSecondsAfterFinished). NEX-461: briefs otherwise
// leak — accumulating one per dispatch forever.
func (k *K8s) SetBriefOwner(ctx context.Context, taskID string, job *batchv1.Job) error {
	cm, err := k.Client.CoreV1().ConfigMaps(k.Namespace).Get(ctx, "brief-"+taskID, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}
	_, err = k.Client.CoreV1().ConfigMaps(k.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// ActiveJob is a live builder Job re-adopted on runner start.
type ActiveJob struct {
	Name  string
	Agent string
}

// ListActiveJobs returns ticket -> live builder Job for non-finished builder
// Jobs. Name + Agent let the Runner re-mark the agent busy across a restart so
// a recovered Job's agent isn't double-run by a new dispatch.
func (k *K8s) ListActiveJobs(ctx context.Context) (map[string]ActiveJob, error) {
	jl, err := k.Client.BatchV1().Jobs(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if err != nil {
		return nil, err
	}
	out := map[string]ActiveJob{}
	for i := range jl.Items {
		j := &jl.Items[i]
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			continue
		}
		if ticket := j.Labels["nexus.dispatch/ticket"]; ticket != "" {
			out[ticket] = ActiveJob{
				Name:  j.Name,
				Agent: j.Labels["nexus.dispatch/agent"],
			}
		}
	}
	return out, nil
}

func (k *K8s) WatchJobs(ctx context.Context, onDone func(JobDone)) error {
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
			done := JobDone{
				Ticket: ticket,
				Thread: thread,
				Agent:  j.Labels["nexus.dispatch/agent"],
			}
			if j.Status.StartTime != nil {
				done.StartedAt = j.Status.StartTime.Time
			}
			if j.Status.CompletionTime != nil {
				done.CompletedAt = j.Status.CompletionTime.Time
			}
			if j.Status.Succeeded > 0 {
				done.OK = true
				onDone(done)
			} else if j.Status.Failed > 0 {
				onDone(done)
			}
		}
	}
}
