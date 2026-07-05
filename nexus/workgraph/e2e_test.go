package workgraph

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// TestLiveWorkGraph exercises the adapter against a real sovereign ledger:
// create A and B (B depends_on A), assert ListReady returns only A,
// transition A done, assert B becomes ready, cancel B with requeue, assert B
// is back to queued. Mirrors the discipline of the custodian/herald live
// tests (nexus/cwb/custodian/custodian_live_test.go): skip cleanly unless
// explicitly opted in.
//
// Gated on WORKGRAPH_E2E_LEDGER (the ledger gRPC address, e.g.
// ledger.cwb.svc:8081). mTLS material comes from WORKGRAPH_TLS_CERT/_KEY/_CA
// (or WORKGRAPH_DEV_INSECURE=1 for a local dev ledger without mTLS) — see
// DialCreds. Org/project default to DefaultOrg/DefaultProject; override via
// WORKGRAPH_E2E_ORG/WORKGRAPH_E2E_PROJECT if the sovereign ledger already has
// state at those defaults you don't want to touch.
//
// NOTE: this test creates real issues on whatever ledger it points at and
// does not delete them afterward (the ledger proto has no DeleteIssue) — do
// not point it at a ledger you care about keeping clean.
func TestLiveWorkGraph(t *testing.T) {
	addr := os.Getenv("WORKGRAPH_E2E_LEDGER")
	if addr == "" {
		t.Skip("set WORKGRAPH_E2E_LEDGER (and WORKGRAPH_TLS_CERT/_KEY/_CA or WORKGRAPH_DEV_INSECURE=1) to run the live work-graph e2e")
	}

	org := os.Getenv("WORKGRAPH_E2E_ORG")
	if org == "" {
		org = DefaultOrg
	}
	project := os.Getenv("WORKGRAPH_E2E_PROJECT")
	if project == "" {
		project = DefaultProject
	}

	creds, err := DialCreds()
	if err != nil {
		t.Fatalf("DialCreds: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	c := New(conn, org, "workgraph-e2e", project)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := c.EnsureProject(ctx); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	a, err := c.CreateWorkItem(ctx, WorkItem{
		Role: "builder", TaskSpec: "workgraph e2e: item A", AcceptanceCriteria: []string{"n/a — e2e fixture"},
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := c.CreateWorkItem(ctx, WorkItem{
		Role: "builder", TaskSpec: "workgraph e2e: item B", AcceptanceCriteria: []string{"n/a — e2e fixture"},
		DependsOn: []string{a},
	})
	if err != nil {
		t.Fatalf("create B (depends_on A): %v", err)
	}

	ready, err := c.ListReady(ctx, "")
	if err != nil {
		t.Fatalf("ListReady (before A done): %v", err)
	}
	if !containsID(ready, a) || containsID(ready, b) {
		t.Fatalf("expected ListReady to contain only A (%s), got %v", a, refIDsE2E(ready))
	}

	if err := c.Transition(ctx, a, StatusDone); err != nil {
		t.Fatalf("transition A done: %v", err)
	}

	ready, err = c.ListReady(ctx, "")
	if err != nil {
		t.Fatalf("ListReady (after A done): %v", err)
	}
	if !containsID(ready, b) {
		t.Fatalf("expected ListReady to contain B (%s) once A is done, got %v", b, refIDsE2E(ready))
	}

	if err := c.Cancel(ctx, b, true, "workgraph e2e: requeue check"); err != nil {
		t.Fatalf("Cancel B requeue: %v", err)
	}
	wi, err := c.GetWorkItem(ctx, b)
	if err != nil {
		t.Fatalf("GetWorkItem B after requeue: %v", err)
	}
	if wi.Status != StatusQueued {
		t.Fatalf("B status after cancel-requeue = %q, want queued", wi.Status)
	}
}

func containsID(items []WorkItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func refIDsE2E(items []WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
