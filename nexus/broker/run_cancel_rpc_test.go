package broker

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCancelResolvesJobByRunIDLabelAndGrace(t *testing.T) {
	cs := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "builder-anvil-run-7", Namespace: "nexus",
			Labels: map[string]string{"app": "nexus-builder", "nexus.dispatch/run-id": "run-7"},
		},
	})
	// graceful -> non-zero grace
	name, grace, err := resolveCancelTarget(context.Background(), cs, "nexus", "run-7", false)
	if err != nil || name != "builder-anvil-run-7" {
		t.Fatalf("resolve: name=%q err=%v", name, err)
	}
	if grace == nil || *grace == 0 {
		t.Fatalf("graceful grace = %v, want non-zero", grace)
	}
	// force -> grace 0
	_, grace0, _ := resolveCancelTarget(context.Background(), cs, "nexus", "run-7", true)
	if grace0 == nil || *grace0 != 0 {
		t.Fatalf("force grace = %v, want 0", grace0)
	}
	// unknown run -> not found
	if _, _, err := resolveCancelTarget(context.Background(), cs, "nexus", "run-none", false); err == nil {
		t.Fatal("unknown run should error")
	}
}
