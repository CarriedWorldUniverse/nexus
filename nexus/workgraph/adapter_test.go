package workgraph

import (
	"context"
	"errors"
	"testing"
)

func TestEnsureProject(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()

	if err := c.EnsureProject(ctx); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if !f.orgs[DefaultOrg] {
		t.Fatalf("org %q not created", DefaultOrg)
	}
	if !f.projects[DefaultProject] {
		t.Fatalf("project %q not created", DefaultProject)
	}

	// Idempotent: calling again must not error or duplicate.
	if err := c.EnsureProject(ctx); err != nil {
		t.Fatalf("EnsureProject (2nd call): %v", err)
	}
}

func TestCreateWorkItem(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()
	_ = c.EnsureProject(ctx)

	id, err := c.CreateWorkItem(ctx, WorkItem{
		Role:               "builder",
		TaskSpec:           "do the thing",
		AcceptanceCriteria: []string{"builds", "tests pass"},
		Origin:             OriginOperator,
		Personality:        "terse",
		BaseKnowledge:      []string{"topic-a"},
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	if id == "" {
		t.Fatalf("expected non-empty id")
	}
	if len(f.comments[id]) != 1 {
		t.Fatalf("expected 1 handoff comment, got %d", len(f.comments[id]))
	}
}

func TestCreateWorkItemLinksDependsOn(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()

	a, err := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "B", AcceptanceCriteria: []string{"x"}, DependsOn: []string{a}})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	wi, err := c.GetWorkItem(ctx, b)
	if err != nil {
		t.Fatalf("GetWorkItem B: %v", err)
	}
	if len(wi.DependsOn) != 1 || wi.DependsOn[0] != a {
		t.Fatalf("expected B depends_on [%s], got %v", a, wi.DependsOn)
	}
}

func TestGetWorkItemFoldsFields(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()

	id, err := c.CreateWorkItem(ctx, WorkItem{
		Role: "tester", TaskSpec: "verify X", AcceptanceCriteria: []string{"a", "b"},
		StreamID: "NET-EPIC", Origin: OriginScheduled, Personality: "meticulous",
		BaseKnowledge: []string{"k1", "k2"}, CairnLine: "builder/foo",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	wi, err := c.GetWorkItem(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wi.Role != "tester" {
		t.Errorf("Role = %q, want tester", wi.Role)
	}
	if wi.TaskSpec != "verify X" {
		t.Errorf("TaskSpec = %q", wi.TaskSpec)
	}
	if len(wi.AcceptanceCriteria) != 2 {
		t.Errorf("AcceptanceCriteria = %v", wi.AcceptanceCriteria)
	}
	if wi.StreamID != "NET-EPIC" {
		t.Errorf("StreamID = %q", wi.StreamID)
	}
	if wi.Origin != OriginScheduled {
		t.Errorf("Origin = %q", wi.Origin)
	}
	if wi.Personality != "meticulous" {
		t.Errorf("Personality = %q", wi.Personality)
	}
	if wi.CairnLine != "builder/foo" {
		t.Errorf("CairnLine = %q", wi.CairnLine)
	}
	if wi.Status != StatusQueued {
		t.Errorf("Status = %q, want queued", wi.Status)
	}
}

func TestListReady(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()

	a, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})
	b, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "B", AcceptanceCriteria: []string{"x"}, DependsOn: []string{a}})

	ready, err := c.ListReady(ctx, "")
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != a {
		t.Fatalf("expected only A ready, got %v", refIDs(ready))
	}

	if err := c.Transition(ctx, a, StatusDone); err != nil {
		t.Fatalf("transition A done: %v", err)
	}

	ready, err = c.ListReady(ctx, "")
	if err != nil {
		t.Fatalf("ListReady after A done: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != b {
		t.Fatalf("expected only B ready, got %v", refIDs(ready))
	}
}

func refIDs(items []WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestListReadyFiltersByStream(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()

	_, _ = c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}, StreamID: "NET-1"})
	b, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "B", AcceptanceCriteria: []string{"x"}, StreamID: "NET-2"})

	ready, err := c.ListReady(ctx, "NET-2")
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != b {
		t.Fatalf("expected only B (stream NET-2), got %v", refIDs(ready))
	}
}

func TestTransitionRejectedHasNoMapping(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()
	id, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})

	err := c.Transition(ctx, id, StatusRejected)
	if !errors.Is(err, ErrNoLedgerStatus) {
		t.Fatalf("expected ErrNoLedgerStatus, got %v", err)
	}
}

func TestClaim(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()
	id, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})

	if err := c.Claim(ctx, id, "agent-01"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := c.Claim(ctx, id, "agent-02"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("expected ErrAlreadyClaimed, got %v", err)
	}
}

func TestRecordResultVerdictTransitions(t *testing.T) {
	tests := []struct {
		name       string
		verdict    Verdict
		wantStatus string
	}{
		{"done transitions to Done", VerdictDone, "Done"},
		{"blocked transitions to Blocked", VerdictBlocked, "Blocked"},
		{"pass leaves status untouched", VerdictPass, "To Do"},
		{"reject leaves status untouched", VerdictReject, "To Do"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTestClient()
			ctx := context.Background()
			id, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})

			if err := c.RecordResult(ctx, id, Result{WorkItemID: id, Agent: "agent-01", Verdict: tt.verdict}); err != nil {
				t.Fatalf("RecordResult: %v", err)
			}
			if got := f.issues[id].status; got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}

			wi, err := c.GetWorkItem(ctx, id)
			if err != nil {
				t.Fatalf("GetWorkItem: %v", err)
			}
			if len(wi.PriorResults) != 1 || wi.PriorResults[0].Verdict != tt.verdict {
				t.Errorf("PriorResults = %v, want one result with verdict %q", wi.PriorResults, tt.verdict)
			}
		})
	}
}

func TestCancelRequeue(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()
	id, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})
	if err := c.Transition(ctx, id, StatusDispatched); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if err := c.Cancel(ctx, id, true, "operator changed priorities"); err != nil {
		t.Fatalf("Cancel requeue: %v", err)
	}

	if got := f.issues[id].status; got != "To Do" {
		t.Fatalf("status = %q, want To Do (requeued)", got)
	}

	wi, err := c.GetWorkItem(ctx, id)
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if len(wi.PriorResults) != 1 {
		t.Fatalf("expected 1 prior_result carrying the requeue reason, got %d", len(wi.PriorResults))
	}
	if len(wi.PriorResults[0].Reasons) != 1 || wi.PriorResults[0].Reasons[0] != "operator changed priorities" {
		t.Fatalf("PriorResults[0].Reasons = %v", wi.PriorResults[0].Reasons)
	}
}

func TestCancelNoRequeueBlocksDependents(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()
	a, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})
	b, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "B", AcceptanceCriteria: []string{"x"}, DependsOn: []string{a}})

	if err := c.Cancel(ctx, a, false, "scope cut"); err != nil {
		t.Fatalf("Cancel no-requeue: %v", err)
	}

	if got := f.issues[a].status; got != "Cancelled" {
		t.Fatalf("A status = %q, want Cancelled", got)
	}
	if got := f.issues[b].status; got != "Blocked" {
		t.Fatalf("B status = %q, want Blocked", got)
	}
}

func TestRework(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()
	id, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A v1", AcceptanceCriteria: []string{"x"}, StreamID: "NET-1"})

	rejectResult := Result{WorkItemID: id, Agent: "reviewer-01", Verdict: VerdictReject, Reasons: []string{"missing tests"}}
	if err := c.RecordResult(ctx, id, rejectResult); err != nil {
		t.Fatalf("RecordResult reject: %v", err)
	}

	newID, err := c.Rework(ctx, id, WorkItem{TaskSpec: "A v2 — add tests", AcceptanceCriteria: []string{"x", "tests"}})
	if err != nil {
		t.Fatalf("Rework: %v", err)
	}
	if newID == id {
		t.Fatalf("Rework should create a new item, got same id")
	}

	newItem, err := c.GetWorkItem(ctx, newID)
	if err != nil {
		t.Fatalf("GetWorkItem new: %v", err)
	}
	if newItem.Role != "builder" {
		t.Errorf("Role not inherited: %q", newItem.Role)
	}
	if newItem.StreamID != "NET-1" {
		t.Errorf("StreamID not inherited: %q", newItem.StreamID)
	}
	if newItem.Origin != OriginRework {
		t.Errorf("Origin = %q, want rework", newItem.Origin)
	}
	if len(newItem.PriorResults) != 1 || newItem.PriorResults[0].Verdict != VerdictReject {
		t.Fatalf("expected the rejecting result carried forward, got %v", newItem.PriorResults)
	}

	// Back-edge link: new item relates-to the rejected one.
	found := false
	for _, l := range f.links {
		if l.from == newID && l.to == id && l.typ == "relates-to" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected relates-to back-edge %s -> %s, links = %v", newID, id, f.links)
	}
}
