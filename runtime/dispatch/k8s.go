package dispatch

import (
	"context"
	"fmt"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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

func (k *K8s) EnsureHomeRepo(ctx context.Context, agent string) error {
	name := HomePVCName(agent)
	_, err := k.Client.CoreV1().PersistentVolumeClaims(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.Namespace,
			Labels: map[string]string{
				"app":                  "nexus-builder",
				"nexus.dispatch/agent": agent,
				"nexus.dispatch/home":  "true",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	_, err = k.Client.CoreV1().PersistentVolumeClaims(k.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *K8s) EnsureSharedReposPVC(ctx context.Context) error {
	name := SharedReposPVCName()
	_, err := k.Client.CoreV1().PersistentVolumeClaims(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.Namespace,
			Labels: map[string]string{
				"app":                  "nexus-builder",
				"nexus.dispatch/repos": "true",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("50Gi"),
				},
			},
		},
	}
	_, err = k.Client.CoreV1().PersistentVolumeClaims(k.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
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

// DeleteJob deletes a builder Job by name with the given grace period (seconds).
// PropagationPolicy=Foreground so the pod is torn down with the Job. A non-zero
// grace lets the pod catch SIGTERM (graceful cancel); 0 forces SIGKILL. Missing
// Job is not an error (idempotent -- it may have completed/TTL'd already).
func (k *K8s) DeleteJob(ctx context.Context, name string, gracePeriodSecs *int64) error {
	fg := metav1.DeletePropagationForeground
	err := k.Client.BatchV1().Jobs(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: gracePeriodSecs,
		PropagationPolicy:  &fg,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
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

// ScaleDeployment sets the replica count on a named Deployment via the
// scale subresource. The napping-presence seams (roundtable spec component
// 1): the broker's wake controller scales 0→1 when chat arrives for a
// napping wake-on-mention aspect; the idle reaper scales 1→0 when it goes
// quiet. UpdateScale (not Get+Update) so concurrent spec edits aren't
// clobbered.
func (k *K8s) ScaleDeployment(ctx context.Context, name string, replicas int32) error {
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	if _, err := k.Client.AppsV1().Deployments(k.Namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("dispatch: scale deployment %s to %d: %w", name, replicas, err)
	}
	return nil
}

// ActiveJob is a live builder Job re-adopted on runner start.
type ActiveJob struct {
	Name  string
	Agent string
	RunID string
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
				RunID: j.Labels["nexus.dispatch/run-id"],
			}
		}
	}
	return out, nil
}

func (k *K8s) WatchJobs(ctx context.Context, onDone func(JobDone)) error {
	seen := map[string]bool{}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	for {
		if err := k.reconcileDoneJobs(ctx, seen, onDone); err != nil {
			return err
		}
		w, err := k.Client.BatchV1().Jobs(k.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
		if err != nil {
			return err
		}
		restart := false
		for !restart {
			select {
			case <-ctx.Done():
				w.Stop()
				return ctx.Err()
			case <-tick.C:
				if err := k.reconcileDoneJobs(ctx, seen, onDone); err != nil {
					w.Stop()
					return err
				}
			case ev, ok := <-w.ResultChan():
				if !ok {
					w.Stop()
					restart = true
					continue
				}
				j, ok := ev.Object.(*batchv1.Job)
				if !ok {
					continue
				}
				if ev.Type == watch.Deleted {
					emitJobDeleted(j, seen, onDone)
					continue
				}
				emitJobDone(j, seen, onDone)
			}
		}
	}
}

func (k *K8s) reconcileDoneJobs(ctx context.Context, seen map[string]bool, onDone func(JobDone)) error {
	jl, err := k.Client.BatchV1().Jobs(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range jl.Items {
		emitJobDone(&jl.Items[i], seen, onDone)
	}
	return nil
}

func emitJobDone(j *batchv1.Job, seen map[string]bool, onDone func(JobDone)) {
	ok, terminal := jobTerminal(j)
	if !terminal {
		return
	}
	seenKey := string(j.UID)
	if seenKey == "" {
		seenKey = j.Namespace + "/" + j.Name
	}
	if seen[seenKey] {
		return
	}
	ticket := j.Labels["nexus.dispatch/ticket"]
	if ticket == "" {
		return
	}
	seen[seenKey] = true
	thread := j.Annotations["nexus.dispatch/thread"]
	if thread == "" {
		thread = ticket
	}
	done := JobDone{
		Ticket: ticket,
		Thread: thread,
		Agent:  j.Labels["nexus.dispatch/agent"],
		OK:     ok,
	}
	if j.Status.StartTime != nil {
		done.StartedAt = j.Status.StartTime.Time
	}
	if j.Status.CompletionTime != nil {
		done.CompletedAt = j.Status.CompletionTime.Time
	}
	onDone(done)
}

// emitJobDeleted emits a terminal (failed) JobDone for a builder Job deleted
// while still non-terminal — a manual `kubectl delete` of a stuck/looping run.
// Without this the watch never reports the run done (it only fires on
// Complete/Failed), so the runner's agentBusy/active stay stuck until a broker
// restart (NEX-528). No-op if the Job was already reported terminal.
func emitJobDeleted(j *batchv1.Job, seen map[string]bool, onDone func(JobDone)) {
	seenKey := string(j.UID)
	if seenKey == "" {
		seenKey = j.Namespace + "/" + j.Name
	}
	if seen[seenKey] {
		return
	}
	ticket := j.Labels["nexus.dispatch/ticket"]
	if ticket == "" {
		return
	}
	seen[seenKey] = true
	thread := j.Annotations["nexus.dispatch/thread"]
	if thread == "" {
		thread = ticket
	}
	done := JobDone{
		Ticket: ticket,
		Thread: thread,
		Agent:  j.Labels["nexus.dispatch/agent"],
		OK:     false,
	}
	if j.Status.StartTime != nil {
		done.StartedAt = j.Status.StartTime.Time
	}
	done.CompletedAt = time.Now()
	onDone(done)
}

func jobTerminal(j *batchv1.Job) (ok bool, terminal bool) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true
		case batchv1.JobFailed:
			return false, true
		}
	}
	return false, false
}
