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
		Repo: "CarriedWorldUniverse/nexus",
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
	if wi.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("Repo = %q", wi.Repo)
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

	ready, err := c.ListReady(ctx, "builder", "")
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != a {
		t.Fatalf("expected only A ready, got %v", refIDs(ready))
	}

	if err := c.Transition(ctx, a, StatusDone); err != nil {
		t.Fatalf("transition A done: %v", err)
	}

	ready, err = c.ListReady(ctx, "builder", "")
	if err != nil {
		t.Fatalf("ListReady after A done: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != b {
		t.Fatalf("expected only B ready, got %v", refIDs(ready))
	}
}

func TestListReadyRequiresRole(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()
	_, _ = c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})

	if _, err := c.ListReady(ctx, "", ""); err == nil {
		t.Fatalf("expected an error querying ListReady with no role — ledger's ListReadyIssues is assignee_aspect-scoped")
	}
}

func TestListReadyScopedToAssigneeAspect(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()
	builderItem, _ := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"x"}})
	testerItem, _ := c.CreateWorkItem(ctx, WorkItem{Role: "tester", TaskSpec: "B", AcceptanceCriteria: []string{"x"}})

	ready, err := c.ListReady(ctx, "builder", "")
	if err != nil {
		t.Fatalf("ListReady as builder: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != builderItem {
		t.Fatalf("querying as builder must only surface builder-assigned items, got %v (tester item %s must not appear)", refIDs(ready), testerItem)
	}

	ready, err = c.ListReady(ctx, "tester", "")
	if err != nil {
		t.Fatalf("ListReady as tester: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != testerItem {
		t.Fatalf("querying as tester must only surface tester-assigned items, got %v", refIDs(ready))
	}
}

func TestListReadyExcludesUnassignedAndFailedDoR(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()

	// No Role -> no assignee_aspect -> never ready for anyone, even though
	// nothing blocks it and it otherwise passes DoR.
	unassigned, _ := c.CreateWorkItem(ctx, WorkItem{TaskSpec: "no role", AcceptanceCriteria: []string{"x"}})

	ready, err := c.ListReady(ctx, "builder", "")
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if containsIDTest(ready, unassigned) {
		t.Fatalf("unassigned item %s must never appear in ListReady", unassigned)
	}

	// Directly violate the fake's definition-of-ready gate (empty DoD) to
	// prove the fake enforces it, not just happens to pass because our own
	// CreateWorkItem always fills it in.
	f.mu.Lock()
	f.issues[unassigned].assigneeAspect = "builder"
	f.issues[unassigned].dod = ""
	f.mu.Unlock()

	ready, err = c.ListReady(ctx, "builder", "")
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if containsIDTest(ready, unassigned) {
		t.Fatalf("item with empty definition_of_done must fail DoR and never appear in ListReady")
	}
}

func containsIDTest(items []WorkItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
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

	ready, err := c.ListReady(ctx, "builder", "NET-2")
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

func TestTransitionDoneTicksDefinitionOfDone(t *testing.T) {
	c, f := newTestClient()
	ctx := context.Background()
	id, err := c.CreateWorkItem(ctx, WorkItem{Role: "builder", TaskSpec: "A", AcceptanceCriteria: []string{"builds", "tests pass"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := f.issues[id].dod; got != "- [ ] builds\n- [ ] tests pass" {
		t.Fatalf("dod after create = %q, want unticked checklist", got)
	}

	// The fake enforces the live ledger's DoD gate (see fake_test.go's
	// TransitionIssue): this would fail with an unticked DoD if Transition
	// didn't tick it first.
	if err := c.Transition(ctx, id, StatusDone); err != nil {
		t.Fatalf("Transition to done: %v", err)
	}
	if got := f.issues[id].dod; got != "- [x] builds\n- [x] tests pass" {
		t.Fatalf("dod after Done transition = %q, want fully ticked", got)
	}

	wi, err := c.GetWorkItem(ctx, id)
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if len(wi.AcceptanceCriteria) != 2 || wi.AcceptanceCriteria[0] != "builds" || wi.AcceptanceCriteria[1] != "tests pass" {
		t.Fatalf("AcceptanceCriteria after ticking = %v, want the plain (unmarked) strings back", wi.AcceptanceCriteria)
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
