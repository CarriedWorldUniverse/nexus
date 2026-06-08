package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func sqldComp(h frames.EnvHealthResultPayload) *frames.EnvComponentPayload {
	for i := range h.Components {
		if h.Components[i].Name == "sqld" {
			return &h.Components[i]
		}
	}
	return nil
}

func TestEnvHealthSnapshot(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nexus-broker-x", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gemma-y", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "aspect-home-anvil", Namespace: "nexus"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	)
	// sqld probe succeeds → sqld healthy via connection, not pod presence.
	h, err := envHealthSnapshot(context.Background(), cs, "nexus", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if h.PodsTotal != 2 || h.PodsRunning != 1 {
		t.Fatalf("pods: %+v", h)
	}
	if len(h.PVCs) != 1 || h.PVCs[0].Status != "Bound" {
		t.Fatalf("pvcs: %+v", h)
	}
	sqld := sqldComp(h)
	if sqld == nil {
		t.Fatal("sqld component missing")
	}
	if !sqld.Healthy || sqld.Detail == "not found" {
		t.Fatalf("sqld should be healthy via probe, not a pod not-found: %+v", sqld)
	}
	if sqld.Kind != "db" {
		t.Fatalf("sqld kind = %q, want db", sqld.Kind)
	}
}

func TestEnvHealthSqldUnreachable(t *testing.T) {
	cs := fake.NewSimpleClientset()
	h, err := envHealthSnapshot(context.Background(), cs, "nexus", func(context.Context) error { return errors.New("boom") })
	if err != nil {
		t.Fatal(err)
	}
	sqld := sqldComp(h)
	if sqld == nil || sqld.Healthy {
		t.Fatalf("sqld should be unhealthy when the probe errors: %+v", sqld)
	}
}
