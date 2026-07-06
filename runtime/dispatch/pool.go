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

// tryLeaseWorkerSlot leases a personality for role and returns (personality,
// `<personality>-<role>`), or ("","") if none is free (or the global MaxConc
// leaves no room). Caller holds r.mu.
//
// requested == "": leases the first FREE personality in roster order — "the
// first agent available, regardless of name, is spawned with a personality
// and the role" (PHASE2-DESIGN §4).
//
// requested != "" (per-personality routing, ROLE-MODEL.md "routing a work
// item to a personality"): leases ONLY that personality when it is free and
// a member of the roster (personalities()); otherwise ("","") — busy or
// unknown never falls back to a different personality, since the request is
// about the BRAIN behind that name (its aspects row's provider/model), and
// substitution would defeat it. The caller (SubmitPoolItem/reserveQueued)
// queues on a miss, same as any other lease failure.
//
// A personality is free when it has no live worker hand — one job per
// personality.
func (r *Runner) tryLeaseWorkerSlot(role, requested string) (personality, name string) {
	if r.MaxConc > 0 && len(r.active) >= r.MaxConc {
		return "", ""
	}
	if requested != "" {
		if !containsString(r.personalities(), requested) {
			return "", ""
		}
		if r.liveWorkers(requested) > 0 {
			return "", ""
		}
		return requested, aspects.WorkerName(requested, role)
	}
	for _, p := range r.personalities() {
		if r.liveWorkers(p) > 0 { // personality already running a pool job
			continue
		}
		return p, aspects.WorkerName(p, role)
	}
	return "", ""
}

// containsString reports whether list contains s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// resolveProvider stamps b.Provider from r.HandProvider(personality) when
// wired and b.Provider is not already set — the same provider-inheritance
// this Runner already does for hand spawns (spawn.go's handProvider), now
// also applied to a pool lease: the leased personality IS the identity
// (Brief.SpawnParent) whose aspects row's provider/model a claude-code (or
// any non-default) personality routing depends on (ROLE-MODEL.md). Without
// this, every pool-leased Job ran with launch's "claude" default regardless
// of the personality's aspects row — silently defeating per-personality
// routing to a stronger brain. No-op when r.HandProvider is nil or
// b.Provider is already set (never overrides an explicit caller choice).
func (r *Runner) resolveProvider(ctx context.Context, b *Brief, personality string) {
	if b.Provider != "" || r.HandProvider == nil || personality == "" {
		return
	}
	b.Provider = r.HandProvider(ctx, personality)
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

	// Personality mirrors Brief.RequestedPersonality (per-personality
	// routing, ROLE-MODEL.md): requests a specific pool personality for
	// this item's lease (workgraph.WorkItem.Personality, threaded by the
	// orchestrator's dispatchOne). Empty = "any free personality" — every
	// pre-routing pool dispatch's exact behavior. Non-empty is honored
	// strictly by tryLeaseWorkerSlot: free -> leased to exactly that
	// personality, busy -> queues (never substituted).
	Personality string

	// Provider/Model mirror Brief.Provider/Brief.Model (role-tier-brains,
	// 2026-07-06): the ROLE's configured brain — e.g. "builder-complex"
	// routed to a heavier provider/model than plain "builder" — threaded by
	// the orchestrator's dispatchOne from its RoleResolver (RoleBrainResolver
	// in production, see nexus/orchestrator/rolebrain.go). Precedence (see
	// SubmitPoolItem/resolveProvider): a non-empty Provider here is stamped
	// straight onto the leased Brief BEFORE resolveProvider runs, so it wins
	// over the leased personality's own aspects-row provider, which in turn
	// wins over launch's "claude" default. Empty = no role-brain override —
	// every pre-role-tier-brains pool dispatch's exact behavior (personality
	// row, then launch default, same as before this field existed).
	Provider string
	Model    string

	// Effort mirrors Brief.Effort (reasoning-EFFORT knob, 2026-07-06): the
	// role's configured reasoning effort — low|medium|high — threaded by
	// the orchestrator's dispatchOne from its RoleResolver's Effort return
	// (RoleBrainResolver in production, see nexus/orchestrator/rolebrain.go).
	// Empty = no override — agentfunnel leaves the claude-api provider's
	// extended-thinking budget unset (provider default), exactly like every
	// pre-reasoning-EFFORT pool dispatch.
	Effort string
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
		SpawnParent:          poolParentName,
		Role:                 role,
		WorkItemID:           workItemID,
		Ticket:               workItemID,
		Thread:               thread,
		Task:                 task,
		RolePrompt:           item.RolePrompt,
		SkillAllowlist:       item.SkillAllowlist,
		PolicyFragment:       item.PolicyFragment,
		AcceptanceCriteria:   item.AcceptanceCriteria,
		Repo:                 item.Repo,
		RequestedPersonality: item.Personality,
		// Provider/Model: the role's brain override, stamped onto the Brief
		// up front (role-tier-brains). Setting Brief.Provider here — BEFORE
		// resolveProvider runs below — is what gives the role brain
		// precedence over the leased personality's own aspects-row provider:
		// resolveProvider is a no-op once b.Provider is already non-empty.
		// Empty item.Provider/item.Model reproduces every pre-role-tier-
		// brains pool dispatch exactly (personality-row inheritance, then
		// launch's default).
		Provider: item.Provider,
		Model:    item.Model,
		// Effort: the role's reasoning-effort override (reasoning-EFFORT
		// knob), stamped onto the Brief unconditionally — unlike Provider,
		// there is no personality-row inheritance to precede for Effort, so
		// it just carries straight through. Empty item.Effort reproduces
		// every pre-reasoning-EFFORT pool dispatch exactly.
		Effort: item.Effort,
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

	personality, name := r.tryLeaseWorkerSlot(role, item.Personality)
	if name == "" {
		r.queue = append(r.queue, b)
		r.mu.Unlock()
		if item.Personality != "" {
			r.post(thread, fmt.Sprintf("pool dispatch queued (role %s, requested personality %s busy/unavailable)", role, item.Personality))
		} else {
			r.post(thread, fmt.Sprintf("pool dispatch queued (role %s, all %d personalit(ies) busy)", role, len(r.personalities())))
		}
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

	// Provider inheritance (per-personality routing): stamp the leased
	// personality's aspects-row provider onto the Brief, mirroring
	// spawn.go's hand provider inheritance (resolved outside r.mu, same
	// reasoning — see resolveProvider). Without this a claude-code-routed
	// personality's Job never got CLAUDE_CODE_OAUTH_TOKEN injected
	// (jobspec.go gates that on provider=="claude-code"), silently falling
	// back to launch's "claude" default for every pool-leased worker
	// regardless of personality.
	r.resolveProvider(ctx, &run.Brief, personality)

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
