package broker

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnvHealthSnapshot(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nexus-broker-x", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gemma-y", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "aspect-home-anvil", Namespace: "nexus"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	)
	h, err := envHealthSnapshot(context.Background(), cs, "nexus")
	if err != nil {
		t.Fatal(err)
	}
	if h.PodsTotal != 2 || h.PodsRunning != 1 {
		t.Fatalf("pods: %+v", h)
	}
	if len(h.PVCs) != 1 || h.PVCs[0].Status != "Bound" {
		t.Fatalf("pvcs: %+v", h)
	}
}
