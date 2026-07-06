// Pool leasing + cap — M1 Unit 4 (PHASE2-DESIGN §4).
//
// A pool is a fixed set of N interchangeable derived-identity slots
// (`pool.sub-1..N`) leased per-dispatch to role-based work items — the
// second dispatch mode alongside `!dispatch <named-agent>` (Submit,
// per-agent-name serialization) and aspect-owned hand fan-out
// (SubmitSpawn, per-parent hand cap). Pool leasing reuses the SAME
// derived-identity machinery (aspects.DerivedName/IsDerivedName,
// MintHandCredential, the queue + OnJobDone drain) under a synthetic
// pool parent identity, capped by its OWN dimension (poolSize) instead
// of the per-aspect hand cap or the per-name serialization guarantee.
//
// Slot identity + role + work_item are stamped on every pool run
// (Brief.Agent/Role/WorkItemID) for accountability, mirroring how named
// dispatch and hand fan-out record identity.

package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// poolParentName is the in-memory sentinel a QUEUED (not-yet-leased) pool
// work item carries as its SpawnParent — the discriminator reserveQueued
// uses to re-lease by role. It is never minted or looked up as an aspect:
// leasing resolves it to a real personality (`<personality>-<role>`, with
// SpawnParent = the personality) before any credential mint. See the
// worker-identity grammar in aspects/lineage.go.
const poolParentName = "pool"

// personalities returns the pool worker roster (Runner.Personalities, or
// aspects.WorkerPersonalities when unset). Lease order = roster order.
func (r *Runner) personalities() []string {
	if len(r.Personalities) > 0 {
		return r.Personalities
	}
	return aspects.WorkerPersonalities
}

// tryLeaseWorkerSlot leases the first FREE personality for role and returns
// (personality, `<personality>-<role>`), or ("","") if none is free (or the
// global MaxConc leaves no room). A personality is free when it has no live
// worker hand — one job per personality, so "the first agent available,
// regardless of name, is spawned with a personality and the role"
// (PHASE2-DESIGN §4). Caller holds r.mu.
func (r *Runner) tryLeaseWorkerSlot(role string) (personality, name string) {
	if r.MaxConc > 0 && len(r.active) >= r.MaxConc {
		return "", ""
	}
	for _, p := range r.personalities() {
		if r.liveWorkers(p) > 0 { // personality already running a pool job
			continue
		}
		return p, aspects.WorkerName(p, role)
	}
	return "", ""
}

// PoolItem is SubmitPoolItem's payload: SubmitPool's basic (role, task,
// workItemID, thread) shape, plus the role-at-spawn overlay fields (M1 Unit
// 3, PHASE2-DESIGN §3) a caller may have already resolved for role —
// carried straight into the leased Brief. Added for M1 Unit 6 (the
// orchestrator's graph-drain, PHASE2-DESIGN §2): SubmitPool's original
// 4-string signature has no way to carry RolePrompt/SkillAllowlist/
// PolicyFragment through to the spawned worker, so this is the superset a
// role-aware dispatcher uses instead. All the overlay fields are optional —
// a zero-value PoolItem{Role, Task, WorkItemID, Thread} dispatches exactly
// like SubmitPool.
type PoolItem struct {
	Role       string
	Task       string
	WorkItemID string
	Thread     string

	// RolePrompt/SkillAllowlist/PolicyFragment mirror Brief's role-at-spawn
	// fields (M1 Unit 3) — see brief.go. Empty/nil reproduces SubmitPool's
	// exact behavior (no role overlay, all skills, static -policy file
	// only).
	RolePrompt     string
	SkillAllowlist []string
	PolicyFragment *funnel.ToolPolicy

	// AcceptanceCriteria mirrors Brief.AcceptanceCriteria (Unit B — verified
	// task_done, NET-22/23/24): the ledger work item's DoD checklist,
	// pre-formatted as text by the orchestrator (drain.go), carried straight
	// into the leased Brief. Empty = no criteria captured, same back-compat
	// story as the other overlay fields.
	AcceptanceCriteria string

	// Repo mirrors Brief.Repo (Phase 4, "real REPO tickets"): the git repo
	// (workgraph.WorkItem.Repo, threaded by the orchestrator's dispatchOne)
	// a builder should check out and PR against. Empty = respond-only work
	// — reproduces every pre-Phase-4 pool dispatch exactly (no builder home
	// repo checkout, no git-credential grant, no PR gate; see runner.go's
	// provisionRun and jobspec.go's builderArgs, both already gated on
	// Brief.Repo != ""). The branch is NOT a PoolItem field: it always
	// follows the existing builder/<ticket> convention (ticket ==
	// WorkItemID for pool dispatch) that Brief.Branch's empty-string
	// default and agentfunnel's builderBranch already apply uniformly
	// across every dispatch mode.
	Repo string
}

// SubmitPool dispatches a role-based work item onto the shared pool
// instead of a named agent. It leases the first free pool slot
// (pool.sub-1..N) as a derived identity of the synthetic pool parent —
// same Job/credential machinery as a hand spawn (SubmitSpawn), but
// keyed to the fixed pool cap (poolSize, default 3) rather than a
// per-aspect hand cap, and without SubmitSpawn's fan-out/audit-root
// shape (one work item leases exactly one slot).
//
// Returns ("", nil) when every slot is busy — the item queues and
// launches on the next OnJobDone-driven drain that frees a slot,
// mirroring Submit's and SubmitSpawn's queue semantics.
//
// workItemID doubles as Brief.Ticket, the idempotency key: resubmitting
// the same work item while it is active or queued is a no-op / returns
// the existing run, exactly like Submit's ticket dedupe.
//
// SubmitPool is PoolItem-free sugar over SubmitPoolItem for callers that
// have no role-at-spawn overlay to carry (e.g. today's !dispatch-adjacent
// callers) — see SubmitPoolItem for the role-prompt/skill-allowlist/
// policy-fragment superset the M1 Unit 6 orchestrator uses.
func (r *Runner) SubmitPool(ctx context.Context, role, task, workItemID, thread string) (string, error) {
	return r.SubmitPoolItem(ctx, PoolItem{Role: role, Task: task, WorkItemID: workItemID, Thread: thread})
}

// SubmitPoolItem is SubmitPool's superset: identical lease/queue/
// idempotency mechanics, but item additionally carries the role-at-spawn
// overlay (RolePrompt/SkillAllowlist/PolicyFragment) into the leased
// Brief — see PoolItem.
func (r *Runner) SubmitPoolItem(ctx context.Context, item PoolItem) (string, error) {
	role, task, workItemID, thread := item.Role, item.Task, item.WorkItemID, item.Thread
	if strings.TrimSpace(role) == "" {
		return "", fmt.Errorf("pool: role required")
	}
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("pool: task required")
	}
	if strings.TrimSpace(workItemID) == "" {
		return "", fmt.Errorf("pool: work_item_id required (idempotency key)")
	}
	if r.MintHandCredential == nil {
		return "", fmt.Errorf("pool: no hand-credential minter configured")
	}
	if thread == "" {
		thread = "pool-" + workItemID
	}

	b := Brief{
		SpawnParent:        poolParentName,
		Role:               role,
		WorkItemID:         workItemID,
		Ticket:             workItemID,
		Thread:             thread,
		Task:               task,
		RolePrompt:         item.RolePrompt,
		SkillAllowlist:     item.SkillAllowlist,
		PolicyFragment:     item.PolicyFragment,
		AcceptanceCriteria: item.AcceptanceCriteria,
		Repo:               item.Repo,
	}

	r.mu.Lock()
	// Idempotency: a run for this work item already active → return its ID.
	for _, run := range r.active {
		if run.Brief.Ticket == b.Ticket {
			id := run.ID
			r.mu.Unlock()
			return id, nil
		}
	}
	// Idempotency: already queued → no-op rather than double-enqueue.
	for _, q := range r.queue {
		if q.Ticket == b.Ticket {
			r.mu.Unlock()
			return "", nil
		}
	}

	personality, name := r.tryLeaseWorkerSlot(role)
	if name == "" {
		r.queue = append(r.queue, b)
		r.mu.Unlock()
		r.post(thread, fmt.Sprintf("pool dispatch queued (role %s, all %d personalit(ies) busy)", role, len(r.personalities())))
		return "", nil
	}
	b.SpawnParent = personality
	b.Agent = name
	// M1 Unit 5 gap: Personality was never stamped on the leased Brief —
	// only SpawnParent/Agent — so jobspec.go's `if b.Personality != ""`
	// guard never fired for pool dispatches: CW_PERSONALITY was never
	// injected into the Job env, and the worker.status heartbeat's
	// `personality` field was permanently empty for every pool-leased
	// worker.
	b.Personality = personality
	run := r.reserve(b)
	r.mu.Unlock()

	if err := r.launch(ctx, run); err != nil {
		r.mu.Lock()
		delete(r.active, run.ID)
		delete(r.agentBusy, run.Brief.Agent)
		r.mu.Unlock()
		if r.Recorder != nil {
			doneCtx := ctx
			if doneCtx == nil {
				doneCtx = context.Background()
			}
			r.Recorder.RecordRunDone(doneCtx, run.ID, "failed", time.Now(), "", 0)
		}
		r.post(thread, "pool dispatch failed: "+err.Error())
		return "", err
	}
	return run.ID, nil
}
