// Package docregister implements the M1 Unit 2 document register: specs,
// plans, designs, and reports as first-class, lifecycle-managed documents
// (PHASE2-DESIGN.md §9). A doc that isn't attached to a work-item with a
// status doesn't exist — no folder of loose markdown files.
//
// Split (README.md has the full rationale): the MD *content* is stored in
// cairn (versioned, diffable, via CairnContent); the *lifecycle* — index,
// status, approvals — lives in a dedicated sqlite-backed Store, mirroring
// nexus/runs's idiom rather than overloading ledger's issue model with
// document-approval semantics.
package docregister

import (
	"errors"
	"time"
)

// Kind is the document type.
type Kind string

const (
	KindSpec   Kind = "spec"
	KindPlan   Kind = "plan"
	KindDesign Kind = "design"
	KindReport Kind = "report"
)

func validKind(k Kind) bool {
	switch k {
	case KindSpec, KindPlan, KindDesign, KindReport:
		return true
	}
	return false
}

// Status is the document's lifecycle state (PHASE2-DESIGN.md §9).
type Status string

const (
	StatusDraft             Status = "draft"
	StatusAwaitingApproval  Status = "awaiting_approval"
	StatusApproved          Status = "approved"
	StatusApprovedWithEdits Status = "approved_with_changes"
	StatusRejected          Status = "rejected"
	StatusSuperseded        Status = "superseded"
)

// Verdict is the operator's decision on an approval work-item, recorded
// alongside the document's status transition.
type Verdict string

const (
	VerdictApprove            Verdict = "approve"
	VerdictApproveWithChanges Verdict = "approve_with_changes"
	VerdictReject             Verdict = "reject"
)

// Approval is one recorded verdict against a document — the shape from
// PHASE2-DESIGN.md §9: {by, verdict, comments, at}.
type Approval struct {
	By       string    `json:"by"`
	Verdict  Verdict   `json:"verdict"`
	Comments string    `json:"comments,omitempty"`
	At       time.Time `json:"at"`
}

// Document is the register's unit: {id, kind, title, version, status,
// work_item_id, cairn_ref, approvals[]} per PHASE2-DESIGN.md §9. CairnRef
// points at the current MD content version (see CairnContent); Version
// increments each time ApproveWithChanges commits an edited body.
type Document struct {
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	Title      string     `json:"title"`
	Version    int        `json:"version"`
	Status     Status     `json:"status"`
	WorkItemID string     `json:"work_item_id"`
	CairnRef   string     `json:"cairn_ref"`
	Approvals  []Approval `json:"approvals,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ListFilter narrows ListDocs. Zero values mean "no filter on that field".
type ListFilter struct {
	Kind   Kind
	Status Status
	Stream string // work_item_id prefix / stream scoping, store-defined
}

var (
	// ErrNotFound is returned by Get/lifecycle calls for an unknown doc id.
	ErrNotFound = errors.New("docregister: document not found")
	// ErrInvalidKind is returned when CreateDoc is given an unrecognized Kind.
	ErrInvalidKind = errors.New("docregister: invalid kind")
	// ErrInvalidTransition is returned when a lifecycle call isn't legal
	// from the document's current status (see README.md's transition table).
	ErrInvalidTransition = errors.New("docregister: invalid status transition")
)
