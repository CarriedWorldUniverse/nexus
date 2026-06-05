package dispatch

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCreateAndListJobs(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, JobConfig{Namespace: "nexus"}, "t1")
	if err := k.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	active, err := k.ListActiveJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active["NEX-1"] == "" {
		t.Errorf("ticket NEX-1 not in active set: %v", active)
	}
	_ = metav1.ObjectMeta{}
}

func TestWatchJobsDone(t *testing.T) {
	fw := watch.NewFake()
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	k.Client.(*fake.Clientset).PrependWatchReactor("jobs", ktesting.DefaultWatchReactor(fw, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		_ = k.WatchJobs(ctx, func(ticket, thread string, ok bool) {
			if ticket == "NEX-1" && thread == "THREAD-1" {
				done <- ok
			}
		})
	}()
	fw.Modify(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "builder-anvil-t1",
			Labels: map[string]string{
				"app":                   "nexus-builder",
				"nexus.dispatch/ticket": "NEX-1",
			},
			Annotations: map[string]string{"nexus.dispatch/thread": "THREAD-1"},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	})
	if ok := <-done; !ok {
		t.Fatal("watch callback reported failure")
	}
}

func keyfileSecret(agent string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aspect-keyfile-" + agent, Namespace: "nexus"}}
}
