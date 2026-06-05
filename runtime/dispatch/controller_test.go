package dispatch

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestController(maxConc int) *Controller {
	return &Controller{
		K8s:     &K8s{Client: fake.NewSimpleClientset(keyfileSecret("anvil"), keyfileSecret("a")), Namespace: "nexus"},
		Cfg:     JobConfig{Namespace: "nexus", Image: "img"},
		MaxConc: maxConc,
		active:  map[string]string{},
		acked:   map[string]bool{},
	}
}

func TestHandle_Idempotent(t *testing.T) {
	c := newTestController(4)
	b := Brief{Agent: "anvil", Ticket: "NEX-1", Task: "do it"}
	if err := c.handle(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	n1 := len(c.active)
	if err := c.handle(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(c.active) != n1 {
		t.Errorf("duplicate dispatch double-spawned: active=%d", len(c.active))
	}
}

func TestHandle_ConcurrencyCap(t *testing.T) {
	c := newTestController(1)
	_ = c.handle(context.Background(), Brief{Agent: "a", Ticket: "T1", Task: "x"})
	_ = c.handle(context.Background(), Brief{Agent: "a", Ticket: "T2", Task: "x"})
	if len(c.active) != 1 {
		t.Errorf("cap not enforced: active=%d want 1", len(c.active))
	}
	if len(c.queue) != 1 {
		t.Errorf("over-cap brief not queued: queue=%d want 1", len(c.queue))
	}
}

func TestInitRebuildsActiveJobs(t *testing.T) {
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, JobConfig{Namespace: "nexus"}, "t1", "codex-cli")
	c := &Controller{
		K8s:     &K8s{Client: fake.NewSimpleClientset(job), Namespace: "nexus"},
		Cfg:     JobConfig{Namespace: "nexus", Image: "img"},
		MaxConc: 4,
	}
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.active["NEX-1"] != job.Name {
		t.Fatalf("active jobs not rebuilt: %v", c.active)
	}
}

func TestSpawn_ResolvesProvider(t *testing.T) {
	tests := []struct {
		name      string
		brief     Brief
		wantCodex bool
	}{
		{
			name:      "default provider",
			brief:     Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Task: "x"},
			wantCodex: true,
		},
		{
			name:      "override provider",
			brief:     Brief{Agent: "anvil", Provider: "openai", Ticket: "NEX-2", Thread: "NEX-2", Task: "x"},
			wantCodex: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestController(4)
			c.NewID = func() string { return "id-" + tt.brief.Ticket }
			if err := c.handle(context.Background(), tt.brief); err != nil {
				t.Fatal(err)
			}
			jobs, err := c.K8s.Client.BatchV1().Jobs("nexus").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 1 {
				t.Fatalf("created jobs = %d, want 1", len(jobs.Items))
			}
			hasCodexInit := batchJobHasInit(jobs.Items[0], "codex-auth")
			if hasCodexInit != tt.wantCodex {
				t.Fatalf("codex init = %v, want %v", hasCodexInit, tt.wantCodex)
			}
		})
	}
}

func batchJobHasInit(job batchv1.Job, name string) bool {
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}
