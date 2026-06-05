package dispatch

import (
	"context"
	"testing"

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
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, JobConfig{Namespace: "nexus"}, "t1")
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
