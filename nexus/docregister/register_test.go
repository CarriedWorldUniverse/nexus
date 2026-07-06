package docregister

import (
	"context"
	"errors"
	"testing"
)

func TestCreateDocDraft(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindSpec, "Unit 2 spec", "wi-1", "# hello")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	doc, err := reg.GetDoc(ctx, id)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if doc.Status != StatusDraft {
		t.Fatalf("status = %q, want draft", doc.Status)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if doc.WorkItemID != "wi-1" {
		t.Fatalf("work_item_id = %q, want wi-1", doc.WorkItemID)
	}
	if doc.CairnRef == "" {
		t.Fatal("cairn_ref is empty")
	}
	content, err := reg.GetContent(ctx, id)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if content != "# hello" {
		t.Fatalf("content = %q, want %q", content, "# hello")
	}
}

func TestCreateDocInvalidKind(t *testing.T) {
	reg, _ := newTestRegister(t)
	if _, err := reg.CreateDoc(context.Background(), Kind("bogus"), "t", "wi-1", "x"); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("err = %v, want ErrInvalidKind", err)
	}
}

func TestFullLifecycle_Approve(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindPlan, "plan", "wi-2", "body v1")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	doc, _ := reg.GetDoc(ctx, id)
	if doc.Status != StatusAwaitingApproval {
		t.Fatalf("status after submit = %q, want awaiting_approval", doc.Status)
	}

	if err := reg.Approve(ctx, id, "operator", "looks good"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	doc, _ = reg.GetDoc(ctx, id)
	if doc.Status != StatusApproved {
		t.Fatalf("status after approve = %q, want approved", doc.Status)
	}
	if len(doc.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(doc.Approvals))
	}
	a := doc.Approvals[0]
	if a.By != "operator" || a.Verdict != VerdictApprove || a.Comments != "looks good" {
		t.Fatalf("approval = %+v, unexpected", a)
	}

	if err := reg.Supersede(ctx, id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	doc, _ = reg.GetDoc(ctx, id)
	if doc.Status != StatusSuperseded {
		t.Fatalf("status after supersede = %q, want superseded", doc.Status)
	}
}

func TestApproveWithChanges_CommitsNewCairnVersion(t *testing.T) {
	reg, content := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindDesign, "design", "wi-3", "body v1")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	before, _ := reg.GetDoc(ctx, id)

	if err := reg.ApproveWithChanges(ctx, id, "operator", "body v2 (edited)", "tightened scope"); err != nil {
		t.Fatalf("ApproveWithChanges: %v", err)
	}
	after, err := reg.GetDoc(ctx, id)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if after.Status != StatusApprovedWithEdits {
		t.Fatalf("status = %q, want approved_with_changes", after.Status)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("version = %d, want %d", after.Version, before.Version+1)
	}
	if after.CairnRef == before.CairnRef {
		t.Fatal("cairn_ref did not change — ApproveWithChanges must commit a new version")
	}
	if len(after.Approvals) != 1 || after.Approvals[0].Verdict != VerdictApproveWithChanges {
		t.Fatalf("approvals = %+v, want one approve_with_changes", after.Approvals)
	}
	got, err := reg.GetContent(ctx, id)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if got != "body v2 (edited)" {
		t.Fatalf("content = %q, want edited body", got)
	}
	// The old version is still retrievable in cairn (versioned, diffable) —
	// exercise the fake directly since GetContent only reads current HEAD.
	oldContent, err := content.Fetch(ctx, before.CairnRef)
	if err != nil {
		t.Fatalf("Fetch old ref: %v", err)
	}
	if oldContent != "body v1" {
		t.Fatalf("old ref content = %q, want %q", oldContent, "body v1")
	}
}

func TestReject(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindSpec, "spec", "wi-4", "body")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	if err := reg.Reject(ctx, id, "operator", []string{"missing acceptance criteria", "unclear scope"}); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	doc, _ := reg.GetDoc(ctx, id)
	if doc.Status != StatusRejected {
		t.Fatalf("status = %q, want rejected", doc.Status)
	}
	if len(doc.Approvals) != 1 || doc.Approvals[0].Verdict != VerdictReject {
		t.Fatalf("approvals = %+v, want one reject", doc.Approvals)
	}
	want := "missing acceptance criteria; unclear scope"
	if doc.Approvals[0].Comments != want {
		t.Fatalf("reasons = %q, want %q", doc.Approvals[0].Comments, want)
	}

	// Rejected is a decided state — Supersede is legal from here too.
	if err := reg.Supersede(ctx, id); err != nil {
		t.Fatalf("Supersede rejected doc: %v", err)
	}
}

func TestInvalidTransitions(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindSpec, "spec", "wi-5", "body")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}

	// Can't approve/reject/approve-with-changes a draft (must submit first).
	if err := reg.Approve(ctx, id, "operator", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Approve on draft: err = %v, want ErrInvalidTransition", err)
	}
	if err := reg.Reject(ctx, id, "operator", nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Reject on draft: err = %v, want ErrInvalidTransition", err)
	}
	if err := reg.ApproveWithChanges(ctx, id, "operator", "edited", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ApproveWithChanges on draft: err = %v, want ErrInvalidTransition", err)
	}
	// Can't supersede a draft (nothing decided yet).
	if err := reg.Supersede(ctx, id); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Supersede on draft: err = %v, want ErrInvalidTransition", err)
	}

	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	// Can't submit twice.
	if err := reg.SubmitForApproval(ctx, id); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double SubmitForApproval: err = %v, want ErrInvalidTransition", err)
	}

	if err := reg.Approve(ctx, id, "operator", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Can't approve twice, and can't reject an already-approved doc.
	if err := reg.Approve(ctx, id, "operator", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double Approve: err = %v, want ErrInvalidTransition", err)
	}
	if err := reg.Reject(ctx, id, "operator", nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Reject on approved: err = %v, want ErrInvalidTransition", err)
	}
	// Double-supersede is rejected too.
	if err := reg.Supersede(ctx, id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if err := reg.Supersede(ctx, id); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double Supersede: err = %v, want ErrInvalidTransition", err)
	}
}

func TestRevise(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	id, err := reg.CreateDoc(ctx, KindSpec, "spec", "wi-6", "v1")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if err := reg.Revise(ctx, id, "v2"); err != nil {
		t.Fatalf("Revise (draft): %v", err)
	}
	doc, _ := reg.GetDoc(ctx, id)
	if doc.Status != StatusDraft {
		t.Fatalf("Revise should not change status, got %q", doc.Status)
	}
	if doc.Version != 2 {
		t.Fatalf("version = %d, want 2", doc.Version)
	}
	content, _ := reg.GetContent(ctx, id)
	if content != "v2" {
		t.Fatalf("content = %q, want v2", content)
	}

	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	if err := reg.Revise(ctx, id, "v3 while under review"); err != nil {
		t.Fatalf("Revise (awaiting_approval): %v", err)
	}

	if err := reg.Approve(ctx, id, "operator", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := reg.Revise(ctx, id, "v4"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Revise after approve: err = %v, want ErrInvalidTransition", err)
	}
}

func TestListDocsFilters(t *testing.T) {
	reg, _ := newTestRegister(t)
	ctx := context.Background()

	spec1, err := reg.CreateDoc(ctx, KindSpec, "s1", "wi-a", "x")
	if err != nil {
		t.Fatal(err)
	}
	spec2, err := reg.CreateDoc(ctx, KindSpec, "s2", "wi-b", "x")
	if err != nil {
		t.Fatal(err)
	}
	plan1, err := reg.CreateDoc(ctx, KindPlan, "p1", "wi-a", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SubmitForApproval(ctx, spec1); err != nil {
		t.Fatal(err)
	}

	byKindSpec, err := reg.ListDocs(ctx, ListFilter{Kind: KindSpec})
	if err != nil {
		t.Fatalf("ListDocs kind=spec: %v", err)
	}
	if !containsID(byKindSpec, spec1) || !containsID(byKindSpec, spec2) || containsID(byKindSpec, plan1) {
		t.Fatalf("ListDocs kind=spec = %v, want spec1+spec2 only", ids(byKindSpec))
	}

	byStatusDraft, err := reg.ListDocs(ctx, ListFilter{Status: StatusDraft})
	if err != nil {
		t.Fatalf("ListDocs status=draft: %v", err)
	}
	if containsID(byStatusDraft, spec1) || !containsID(byStatusDraft, spec2) || !containsID(byStatusDraft, plan1) {
		t.Fatalf("ListDocs status=draft = %v, want spec2+plan1 only (spec1 moved on)", ids(byStatusDraft))
	}

	byStream, err := reg.ListDocs(ctx, ListFilter{Stream: "wi-a"})
	if err != nil {
		t.Fatalf("ListDocs stream=wi-a: %v", err)
	}
	if !containsID(byStream, spec1) || !containsID(byStream, plan1) || containsID(byStream, spec2) {
		t.Fatalf("ListDocs stream=wi-a = %v, want spec1+plan1 only", ids(byStream))
	}

	byKindAndStatus, err := reg.ListDocs(ctx, ListFilter{Kind: KindSpec, Status: StatusAwaitingApproval})
	if err != nil {
		t.Fatalf("ListDocs kind=spec,status=awaiting_approval: %v", err)
	}
	if len(byKindAndStatus) != 1 || byKindAndStatus[0].ID != spec1 {
		t.Fatalf("ListDocs kind+status combo = %v, want [spec1]", ids(byKindAndStatus))
	}
}

func TestGetDocNotFound(t *testing.T) {
	reg, _ := newTestRegister(t)
	if _, err := reg.GetDoc(context.Background(), "doc-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func containsID(docs []Document, id string) bool {
	for _, d := range docs {
		if d.ID == id {
			return true
		}
	}
	return false
}

func ids(docs []Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out
}
