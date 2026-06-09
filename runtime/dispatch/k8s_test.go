package dispatch

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCreateAndListJobs(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", RunID: "run-1"}, JobConfig{Namespace: "nexus"}, "run-1", "codex-cli")
	if _, err := k.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	active, err := k.ListActiveJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active["NEX-1"].Name == "" {
		t.Errorf("ticket NEX-1 not in active set: %v", active)
	}
	if active["NEX-1"].Agent != "anvil" {
		t.Errorf("active NEX-1 agent = %q, want anvil", active["NEX-1"].Agent)
	}
	if active["NEX-1"].RunID != "run-1" {
		t.Errorf("active NEX-1 run ID = %q, want run-1", active["NEX-1"].RunID)
	}
	_ = metav1.ObjectMeta{}
}

func TestSetBriefOwner(t *testing.T) {
	// NEX-461: the brief ConfigMap must end up owned by its Job so it GCs with it.
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	if err := k.PutBriefConfigMap(context.Background(), "t1", "the brief"); err != nil {
		t.Fatal(err)
	}
	job, err := k.CreateJob(context.Background(), BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, JobConfig{Namespace: "nexus"}, "t1", "codex-cli"))
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetBriefOwner(context.Background(), "t1", job); err != nil {
		t.Fatal(err)
	}
	cm, err := k.Client.CoreV1().ConfigMaps("nexus").Get(context.Background(), "brief-t1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != job.Name {
		t.Errorf("brief not owned by its Job: %+v", cm.OwnerReferences)
	}
}

func TestDeleteJobGracefulAndForce(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	ctx := context.Background()
	mk := func(name string) {
		_, _ = k.Client.BatchV1().Jobs("nexus").Create(ctx,
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nexus"}}, metav1.CreateOptions{})
	}
	mk("builder-anvil-run-1")
	if err := k.DeleteJob(ctx, "builder-anvil-run-1", int64p(30)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Client.BatchV1().Jobs("nexus").Get(ctx, "builder-anvil-run-1", metav1.GetOptions{}); err == nil {
		t.Fatal("graceful delete: job should be gone")
	}
	mk("builder-anvil-run-2")
	if err := k.DeleteJob(ctx, "builder-anvil-run-2", int64p(0)); err != nil {
		t.Fatal(err)
	}
	// deleting a missing job is not an error (idempotent)
	if err := k.DeleteJob(ctx, "does-not-exist", int64p(0)); err != nil {
		t.Fatalf("delete missing job should be nil, got %v", err)
	}
}

func int64p(v int64) *int64 { return &v }

func TestEnsureHomeRepoCreatesRWOClaimIdempotently(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	ctx := context.Background()

	if err := k.EnsureHomeRepo(ctx, "anvil"); err != nil {
		t.Fatal(err)
	}
	if err := k.EnsureHomeRepo(ctx, "anvil"); err != nil {
		t.Fatal(err)
	}
	pvcs, err := k.Client.CoreV1().PersistentVolumeClaims("nexus").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("PVC count = %d, want 1", len(pvcs.Items))
	}
	pvc := pvcs.Items[0]
	if pvc.Name != "aspect-home-anvil" {
		t.Fatalf("PVC name = %q, want aspect-home-anvil", pvc.Name)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("PVC access modes = %v, want RWO", pvc.Spec.AccessModes)
	}
	if pvc.Labels["nexus.dispatch/agent"] != "anvil" || pvc.Labels["nexus.dispatch/home"] != "true" {
		t.Fatalf("PVC labels missing agent/home: %v", pvc.Labels)
	}
}

func TestEnsureSharedReposPVCCreatesRWXClaimIdempotently(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	ctx := context.Background()

	if err := k.EnsureSharedReposPVC(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.EnsureSharedReposPVC(ctx); err != nil {
		t.Fatal(err)
	}
	pvcs, err := k.Client.CoreV1().PersistentVolumeClaims("nexus").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("PVC count = %d, want 1", len(pvcs.Items))
	}
	pvc := pvcs.Items[0]
	if pvc.Name != SharedReposPVCName() {
		t.Fatalf("PVC name = %q, want %s", pvc.Name, SharedReposPVCName())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatalf("PVC access modes = %v, want RWX", pvc.Spec.AccessModes)
	}
	if pvc.Labels["nexus.dispatch/repos"] != "true" {
		t.Fatalf("PVC labels missing repos marker: %v", pvc.Labels)
	}
}

func TestWatchJobsDoneCompleteAndFailed(t *testing.T) {
	fw := watch.NewFake()
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	k.Client.(*fake.Clientset).PrependWatchReactor("jobs", ktesting.DefaultWatchReactor(fw, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan watchedJob, 2)
	go func() {
		_ = k.WatchJobs(ctx, func(jd JobDone) {
			done <- watchedJob{ticket: jd.Ticket, thread: jd.Thread, ok: jd.OK}
		})
	}()

	fw.Modify(terminalJob("builder-anvil-t1", "NEX-1", "THREAD-1", batchv1.JobComplete))
	assertWatchedJob(t, done, watchedJob{ticket: "NEX-1", thread: "THREAD-1", ok: true})

	fw.Modify(terminalJob("builder-anvil-t2", "NEX-2", "THREAD-2", batchv1.JobFailed))
	assertWatchedJob(t, done, watchedJob{ticket: "NEX-2", thread: "THREAD-2", ok: false})
}

func TestWatchJobsReconcilesExistingTerminalJobs(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	_, err := k.Client.BatchV1().Jobs("nexus").Create(
		context.Background(),
		terminalJob("builder-anvil-t1", "NEX-1", "THREAD-1", batchv1.JobComplete),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan watchedJob, 1)
	go func() {
		_ = k.WatchJobs(ctx, func(jd JobDone) {
			done <- watchedJob{ticket: jd.Ticket, thread: jd.Thread, ok: jd.OK}
		})
	}()

	assertWatchedJob(t, done, watchedJob{ticket: "NEX-1", thread: "THREAD-1", ok: true})
}

func TestWatchJobsEmitsTerminalOnDelete(t *testing.T) {
	// NEX-528: a manual delete of a stuck/looping (non-terminal) Job must emit a
	// terminal JobDone so the runner frees the agent without a broker restart.
	fw := watch.NewFake()
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	k.Client.(*fake.Clientset).PrependWatchReactor("jobs", ktesting.DefaultWatchReactor(fw, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan watchedJob, 1)
	go func() {
		_ = k.WatchJobs(ctx, func(jd JobDone) {
			done <- watchedJob{ticket: jd.Ticket, thread: jd.Thread, ok: jd.OK}
		})
	}()

	// A non-terminal builder Job (no Complete/Failed condition) that gets deleted.
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "builder-anvil-stuck",
			Namespace: "nexus",
			Labels: map[string]string{
				"app":                   "nexus-builder",
				"nexus.dispatch/ticket": "NEX-9",
				"nexus.dispatch/agent":  "anvil",
			},
			Annotations: map[string]string{"nexus.dispatch/thread": "THREAD-9"},
		},
	}
	fw.Delete(j)
	assertWatchedJob(t, done, watchedJob{ticket: "NEX-9", thread: "THREAD-9", ok: false})
}

func keyfileSecret(agent string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aspect-keyfile-" + agent, Namespace: "nexus"}}
}

type watchedJob struct {
	ticket string
	thread string
	ok     bool
}

func assertWatchedJob(t *testing.T, ch <-chan watchedJob, want watchedJob) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("done = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for job done callback")
	}
}

func terminalJob(name, ticket, thread string, condition batchv1.JobConditionType) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "nexus",
			Labels: map[string]string{
				"app":                   "nexus-builder",
				"nexus.dispatch/ticket": ticket,
			},
			Annotations: map[string]string{"nexus.dispatch/thread": thread},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   condition,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}
