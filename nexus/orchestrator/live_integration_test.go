package orchestrator

// Live integration probe (uncommitted, env-gated): does the orchestrator,
// wired to the REAL sovereign ledger work-graph, read a ready work-item and
// dispatch it? Proves the drain loop end-to-end against the live ledger with
// the package's fakeDispatcher standing in for the k8s Runner.
//
//   ORCH_E2E_LEDGER=ledger.cwb.svc.cluster.local:8081 \
//   WORKGRAPH_TLS_CERT=/etc/cw-app-tls/tls.crt \
//   WORKGRAPH_TLS_KEY=/etc/cw-app-tls/tls.key \
//   WORKGRAPH_TLS_CA=/etc/cw-app-tls/ca.crt \
//   go test ./nexus/orchestrator/ -run TestLiveOrchestratorDrain -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"google.golang.org/grpc"
)

func TestLiveOrchestratorDrain(t *testing.T) {
	addr := os.Getenv("ORCH_E2E_LEDGER")
	if addr == "" {
		t.Skip("set ORCH_E2E_LEDGER to run the live orchestrator drain probe")
	}

	creds, err := workgraph.DialCreds()
	if err != nil {
		t.Fatalf("DialCreds: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const role = "builder"
	gc := workgraph.New(conn, "carriedworld", "orch-live-probe", "NET")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := gc.EnsureProject(ctx); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed a ready work-item assigned to the role.
	id, err := gc.CreateWorkItem(ctx, workgraph.WorkItem{
		Role:               role,
		TaskSpec:           "live orchestrator probe: echo hello",
		AcceptanceCriteria: []string{"prints hello"},
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	t.Logf("seeded ready work-item %s for role %q", id, role)

	// Real orchestrator over the live graph; fakeDispatcher captures the
	// dispatch DECISION (we assert the drain reached dispatch, not a k8s Job).
	disp := &fakeDispatcher{}
	o := &Orchestrator{
		Graph:        gc,
		Dispatcher:   disp,
		WorkerStatus: &fakeWorkerStatus{},
		Roles:        []string{role},
	}

	report, err := o.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	var dispatched []string
	for _, c := range disp.calls {
		dispatched = append(dispatched, c.WorkItemID)
	}
	t.Logf("drain report: dispatched=%v skipped=%v reaped=%v held=%v errors=%v",
		dispatched, report.Skipped, report.Reaped, report.Held, report.Errors)

	found := false
	for _, d := range dispatched {
		if d == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("orchestrator did not dispatch the seeded item %s (dispatched=%v report=%+v)", id, dispatched, report)
	}
	t.Logf("PASS: orchestrator drained the live sovereign ledger and dispatched %s to role %q", id, role)
}
