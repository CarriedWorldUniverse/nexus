// Package workgraph is a thin adapter that persists the orchestrator's work
// graph as ledger issues on the sovereign ledger (ledger.cwb.svc:8081). It
// wraps existing ledger gRPC RPCs (CreateIssue/GetIssue/UpdateIssue/
// TransitionIssue/AddLink/ListLinks/ListReadyIssues/ClaimIssue/CommentIssue) —
// it does not implement new tracker logic. See README.md for the
// work_item<->Issue field mapping.
//
// Runtime-only state (dispatched/running, pool lease) belongs in nexus/runs,
// not here — this package only ever writes the durable graph.
package workgraph

import "encoding/json"

// Status is the work_item's lifecycle status, per handoff.schema.json plus
// the graph-only statuses (ready/blocked/cancelled) that the ledger adapter
// tracks. See README.md for the full status<->ledger-workflow-state mapping.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusReady      Status = "ready"
	StatusDispatched Status = "dispatched"
	StatusRunning    Status = "running"
	StatusDone       Status = "done"
	StatusRejected   Status = "rejected"
	StatusBlocked    Status = "blocked"
	StatusCancelled  Status = "cancelled"
)

// Origin is the work_item's trigger source, per handoff.schema.json.
type Origin string

const (
	OriginOperator  Origin = "operator"
	OriginScheduled Origin = "scheduled"
	OriginEvent     Origin = "event"
	OriginRework    Origin = "rework"
)

// Verdict is a result's gate outcome, per handoff.schema.json.
type Verdict string

const (
	VerdictPass    Verdict = "pass"
	VerdictReject  Verdict = "reject"
	VerdictBlocked Verdict = "blocked"
	VerdictDone    Verdict = "done"
)

// Artifact is a reference to a prior hop's output (or a result's evidence),
// per handoff.schema.json #/$defs/artifact.
type Artifact struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

// Metrics is a result's optional bookkeeping, per handoff.schema.json.
type Metrics struct {
	Steps      int `json:"steps,omitempty"`
	ToolCalls  int `json:"tool_calls,omitempty"`
	DurationMs int `json:"duration_ms,omitempty"`
}

// Result is a role's outcome for a work_item, per handoff.schema.json
// #/$defs/result. Persisted as a JSON comment on the ledger issue, tagged
// commentTagResult (see README.md).
type Result struct {
	WorkItemID   string     `json:"work_item_id"`
	Role         string     `json:"role"`
	Agent        string     `json:"agent"`
	Verdict      Verdict    `json:"verdict"`
	Reasons      []string   `json:"reasons,omitempty"`
	Artifacts    []Artifact `json:"artifacts,omitempty"`
	Evidence     string     `json:"evidence,omitempty"`
	NextRoleHint string     `json:"next_role_hint,omitempty"`
	Metrics      *Metrics   `json:"metrics,omitempty"`
}

// WorkItem is the orchestrator's unit of work, per handoff.schema.json
// #/$defs/work_item, plus the graph-only fields (Status, StreamID, DependsOn)
// the ledger adapter needs to place it in the dependency graph. See
// README.md for the full field<->ledger mapping.
type WorkItem struct {
	ID                 string     `json:"id"`
	Role               string     `json:"role"`
	TaskSpec           string     `json:"task_spec"`
	AcceptanceCriteria []string   `json:"acceptance_criteria"`
	CairnLine          string     `json:"cairn_line,omitempty"`
	Artifacts          []Artifact `json:"artifacts,omitempty"`
	// Repo is the git repo (owner/name, or any form dispatch.Brief.Repo
	// accepts) this work item's builder should check out and branch off of
	// (Phase 4, "real REPO tickets"). Empty = respond-only work: no builder
	// home repo checkout, no branch, no PR gate — reproduces the pre-Phase-4
	// pool behavior exactly. NOT the same thing as CairnLine (this repo's
	// own internal VCS line for a knowledge/design artifact) — Repo is a
	// plain git remote a builder clones/branches/PRs against. Threaded to
	// dispatch.PoolItem.Repo -> Brief.Repo by the orchestrator's dispatchOne
	// (drain.go); the branch itself is never carried explicitly — it always
	// follows the existing builder/<ticket> convention (ticket ==
	// WorkItemID for pool dispatch), same as named dispatch's default.
	Repo          string   `json:"repo,omitempty"`
	BaseKnowledge []string `json:"base_knowledge,omitempty"`
	Personality   string   `json:"personality,omitempty"`
	PriorResults  []Result `json:"prior_results,omitempty"`
	Origin        Origin   `json:"origin,omitempty"`

	// Status is the current lifecycle status (graph-only; not part of the
	// handoff.schema.json work_item, which is the point-in-time payload
	// handed to a role — Status is the ledger adapter's read of the issue's
	// workflow state, see README.md).
	Status Status `json:"status,omitempty"`
	// StreamID is the parent epic key (ledger parent_key). A stream is an
	// epic subtree.
	StreamID string `json:"stream_id,omitempty"`
	// DependsOn lists work_item ids that must reach StatusDone before this
	// item is ready (ledger issue_links type=blocks, blocker -> this).
	DependsOn []string `json:"depends_on,omitempty"`
}

// handoffBlob is the JSON shape persisted in the cwb:handoff comment: the
// work_item fields with no first-class ledger column (origin, personality,
// base_knowledge — prior_results is folded separately from cwb:result
// comments so each result is its own timeline entry).
type handoffBlob struct {
	CairnLine     string     `json:"cairn_line,omitempty"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
	BaseKnowledge []string   `json:"base_knowledge,omitempty"`
	Personality   string     `json:"personality,omitempty"`
	Origin        Origin     `json:"origin,omitempty"`
	// Repo mirrors WorkItem.Repo — no first-class ledger column, same as
	// the other handoff-only fields above.
	Repo string `json:"repo,omitempty"`
}

func (h handoffBlob) marshal() (string, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
