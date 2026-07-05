package docregister

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Register is the docregister API surface (spec §"API"): CreateDoc/GetDoc/
// ListDocs/SubmitForApproval/Approve/ApproveWithChanges/Reject/Supersede.
// It composes a Store (lifecycle index) and a CairnContent (MD body
// storage) — see README.md for the split. Register itself holds no
// authorization logic: the workbench-vs-verdict access boundary is enforced
// by the broker layer (draft/read/revise endpoints are broker-authenticated;
// verdict endpoints are requireAdmin) — see nexus/broker/docregister_rest.go.
type Register struct {
	Store   Store
	Content CairnContent

	// Now is injectable for tests; defaults to time.Now when nil.
	Now func() time.Time
}

func (r *Register) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// newID mints a short hex document id. Sufficient for register-scoped
// uniqueness (Store.Create's PRIMARY KEY constraint is the real backstop).
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("docregister: id rand: %w", err)
	}
	return "doc-" + hex.EncodeToString(b[:]), nil
}

// CreateDoc files a new document: status=draft, writes mdContent to cairn
// (version 1), and indexes it in the store. Every document belongs to a
// work-item (workItemID) — orphans are impossible by construction, per
// PHASE2-DESIGN.md §9.
func (r *Register) CreateDoc(ctx context.Context, kind Kind, title, workItemID, mdContent string) (string, error) {
	if !validKind(kind) {
		return "", ErrInvalidKind
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	ref, err := r.Content.Commit(ctx, id, kind, mdContent)
	if err != nil {
		return "", fmt.Errorf("docregister.CreateDoc: content commit: %w", err)
	}
	now := r.now()
	doc := Document{
		ID:         id,
		Kind:       kind,
		Title:      title,
		Version:    1,
		Status:     StatusDraft,
		WorkItemID: workItemID,
		CairnRef:   ref,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := r.Store.Create(ctx, doc); err != nil {
		return "", fmt.Errorf("docregister.CreateDoc: %w", err)
	}
	return id, nil
}

// GetDoc fetches a document's current metadata + approvals (not its MD
// body — see GetContent).
func (r *Register) GetDoc(ctx context.Context, id string) (Document, error) {
	return r.Store.Get(ctx, id)
}

// GetContent fetches a document's current MD body from cairn.
func (r *Register) GetContent(ctx context.Context, id string) (string, error) {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("docregister.GetContent: %w", err)
	}
	content, err := r.Content.Fetch(ctx, doc.CairnRef)
	if err != nil {
		return "", fmt.Errorf("docregister.GetContent: %w", err)
	}
	return content, nil
}

// ListDocs returns documents matching filter (kind/status/stream).
func (r *Register) ListDocs(ctx context.Context, filter ListFilter) ([]Document, error) {
	return r.Store.List(ctx, filter)
}

// Revise commits an edited MD body as a new cairn version and bumps
// Document.Version, without touching status — the shared-workbench "shadow
// drafts/revises" path (spec: draft/revise endpoints are broker-authenticated,
// not operator-only). Valid from draft or awaiting_approval (revising while
// under review is normal collaborative editing prior to a verdict); invalid
// once a verdict has landed (approved/approved_with_changes/rejected/
// superseded) — supersede a decided doc instead of mutating it in place.
func (r *Register) Revise(ctx context.Context, id, editedMD string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.Revise: %w", err)
	}
	switch doc.Status {
	case StatusDraft, StatusAwaitingApproval:
	default:
		return fmt.Errorf("docregister.Revise: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	ref, err := r.Content.Commit(ctx, id, doc.Kind, editedMD)
	if err != nil {
		return fmt.Errorf("docregister.Revise: content commit: %w", err)
	}
	if err := r.Store.SetCairnRef(ctx, id, ref, nextVersion(doc.Version), r.now()); err != nil {
		return fmt.Errorf("docregister.Revise: %w", err)
	}
	return nil
}

// SubmitForApproval moves a document draft -> awaiting_approval. Only legal
// from draft.
func (r *Register) SubmitForApproval(ctx context.Context, id string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.SubmitForApproval: %w", err)
	}
	if doc.Status != StatusDraft {
		return fmt.Errorf("docregister.SubmitForApproval: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	if err := r.Store.UpdateStatus(ctx, id, StatusAwaitingApproval, r.now()); err != nil {
		return fmt.Errorf("docregister.SubmitForApproval: %w", err)
	}
	return nil
}

// Approve records an operator approve verdict and moves the document to
// approved. Only legal from awaiting_approval. Callers (the broker layer)
// are responsible for ensuring only the operator can reach this — Register
// itself performs no authorization.
func (r *Register) Approve(ctx context.Context, id, by, comments string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.Approve: %w", err)
	}
	if doc.Status != StatusAwaitingApproval {
		return fmt.Errorf("docregister.Approve: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	now := r.now()
	if err := r.Store.AddApproval(ctx, id, Approval{By: by, Verdict: VerdictApprove, Comments: comments, At: now}); err != nil {
		return fmt.Errorf("docregister.Approve: %w", err)
	}
	if err := r.Store.UpdateStatus(ctx, id, StatusApproved, now); err != nil {
		return fmt.Errorf("docregister.Approve: %w", err)
	}
	return nil
}

// ApproveWithChanges commits the operator's editedMD as a new cairn version,
// records an approve_with_changes verdict, and moves the document to
// approved_with_changes. Only legal from awaiting_approval.
func (r *Register) ApproveWithChanges(ctx context.Context, id, by, editedMD, comments string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.ApproveWithChanges: %w", err)
	}
	if doc.Status != StatusAwaitingApproval {
		return fmt.Errorf("docregister.ApproveWithChanges: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	ref, err := r.Content.Commit(ctx, id, doc.Kind, editedMD)
	if err != nil {
		return fmt.Errorf("docregister.ApproveWithChanges: content commit: %w", err)
	}
	now := r.now()
	if err := r.Store.SetCairnRef(ctx, id, ref, nextVersion(doc.Version), now); err != nil {
		return fmt.Errorf("docregister.ApproveWithChanges: %w", err)
	}
	if err := r.Store.AddApproval(ctx, id, Approval{By: by, Verdict: VerdictApproveWithChanges, Comments: comments, At: now}); err != nil {
		return fmt.Errorf("docregister.ApproveWithChanges: %w", err)
	}
	if err := r.Store.UpdateStatus(ctx, id, StatusApprovedWithEdits, now); err != nil {
		return fmt.Errorf("docregister.ApproveWithChanges: %w", err)
	}
	return nil
}

// Reject records an operator reject verdict (reasons joined into the
// approval's Comments) and moves the document to rejected. Only legal from
// awaiting_approval.
func (r *Register) Reject(ctx context.Context, id, by string, reasons []string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.Reject: %w", err)
	}
	if doc.Status != StatusAwaitingApproval {
		return fmt.Errorf("docregister.Reject: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	now := r.now()
	if err := r.Store.AddApproval(ctx, id, Approval{By: by, Verdict: VerdictReject, Comments: joinReasons(reasons), At: now}); err != nil {
		return fmt.Errorf("docregister.Reject: %w", err)
	}
	if err := r.Store.UpdateStatus(ctx, id, StatusRejected, now); err != nil {
		return fmt.Errorf("docregister.Reject: %w", err)
	}
	return nil
}

// Supersede marks a decided document (approved/approved_with_changes/
// rejected) as superseded — e.g. replaced by a newer doc. Not legal from
// draft/awaiting_approval (nothing to supersede yet) or from superseded
// itself (idempotency isn't the point here; a caller superseding twice is
// almost certainly a bug worth surfacing).
func (r *Register) Supersede(ctx context.Context, id string) error {
	doc, err := r.Store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("docregister.Supersede: %w", err)
	}
	switch doc.Status {
	case StatusApproved, StatusApprovedWithEdits, StatusRejected:
	default:
		return fmt.Errorf("docregister.Supersede: status %q: %w", doc.Status, ErrInvalidTransition)
	}
	if err := r.Store.UpdateStatus(ctx, id, StatusSuperseded, r.now()); err != nil {
		return fmt.Errorf("docregister.Supersede: %w", err)
	}
	return nil
}

func joinReasons(reasons []string) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}
