package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// DrainOnce is one stateless graph-drain pass (PHASE2-DESIGN §2 "Body"):
//
//  1. ReapStale — requeue any work item whose worker's heartbeat has gone
//     stale (runs every pass, see README.md).
//  2. PreflightAuth (AuthProbe) — if configured and it fails, HOLD: dispatch
//     nothing, alert, return (Held=true, nil error). See README.md
//     "Auth-hold".
//  3. For each role in o.Roles: workgraph.ListReady(role, o.Stream) → for
//     each ready item, workgraph.Claim it (the idempotent-dispatch guard —
//     an item another pass already claimed is skipped, never
//     double-dispatched), resolve the role overlay (o.Resolver, optional),
//     dispatch.SubmitPoolItem, then workgraph.Transition(id, dispatched) on
//     a successful submit.
//
// DrainOnce reads all state fresh from Graph/WorkerStatus every call — see
// package doc "Stateless". A per-item error is recorded in
// DrainReport.Errors and does not abort the rest of the pass; only a
// ListReady failure for a role (an infrastructure-level failure, not a
// per-item one) returns early with an error.
func (o *Orchestrator) DrainOnce(ctx context.Context) (DrainReport, error) {
	report := DrainReport{}

	reaped, err := o.ReapStale(ctx)
	report.Reaped = reaped
	if err != nil {
		report.Errors = append(report.Errors, errf("reap: %v", err))
	}

	if o.AuthProbe != nil {
		if probeErr := o.AuthProbe(ctx); probeErr != nil {
			o.alert(ctx, "orchestrator-auth-preflight-failed",
				fmt.Sprintf("drain held, nothing dispatched: %v", probeErr))
			report.Held = true
			report.HoldReason = probeErr.Error()
			return report, nil
		}
	}

	for _, role := range o.Roles {
		items, err := o.Graph.ListReady(ctx, role, o.Stream)
		if err != nil {
			return report, fmt.Errorf("orchestrator: DrainOnce: list ready for role %q: %w", role, err)
		}
		for _, wi := range items {
			o.dispatchOne(ctx, role, wi, &report)
		}
	}
	return report, nil
}

// dispatchOne claims + dispatches a single ready item, recording the
// outcome on report. Claim's atomicity is the idempotent-dispatch guard:
// ErrAlreadyClaimed means an earlier pass (or a concurrent one) already has
// this item, so it is skipped rather than double-dispatched.
func (o *Orchestrator) dispatchOne(ctx context.Context, role string, wi workgraph.WorkItem, report *DrainReport) {
	// Idempotent-dispatch guard = the item's status, NOT a Claim. In the
	// sovereign ledger, assigning an item to a role (CreateWorkItem sets
	// assignee_aspect=role) IS the claim — the orchestrator, a different
	// identity, can never Claim it (confirmed live: ClaimIssue returns
	// Aborted "already claimed by another agent" because assignee != actor).
	// So dispatch only items still queued (ledger "To Do"); an item already
	// dispatched/running (ledger "In Progress") is skipped — a second drain
	// pass, or a worker mid-run, must not be double-dispatched.
	if wi.Status != workgraph.StatusQueued {
		report.Skipped = append(report.Skipped, wi.ID)
		return
	}

	rolePrompt, skills, policy, brainProvider, brainModel := o.resolve(role)
	item := dispatch.PoolItem{
		Role:               role,
		Task:               wi.TaskSpec,
		WorkItemID:         wi.ID,
		RolePrompt:         rolePrompt,
		SkillAllowlist:     skills,
		PolicyFragment:     policy,
		AcceptanceCriteria: formatAcceptanceCriteria(wi.AcceptanceCriteria),
		// Provider/Model (role-tier-brains, 2026-07-06): the role's
		// configured brain, from o.Resolver (RoleBrainResolver in
		// production — see rolebrain.go). Empty when no Resolver is wired
		// or the role has no override, so PoolItem/Brief's Provider/Model
		// stay "" exactly as before this existed, and
		// dispatch.Runner.resolveProvider's personality-row inheritance +
		// launch's default apply unchanged (see runtime/dispatch/pool.go
		// resolveProvider / SubmitPoolItem's precedence comment).
		Provider: brainProvider,
		Model:    brainModel,
		// Repo (Phase 4, "real REPO tickets"): threading wi.Repo straight
		// through to PoolItem.Repo -> Brief.Repo is the whole gap this
		// closes — SubmitPoolItem/BuildJob already do everything else
		// (git-credential grant, -repo/-branch args, PR gate) once Brief.Repo
		// is non-empty; only the work-item -> PoolItem leg was missing this
		// field (see runtime/dispatch/README.md "The pool model" and
		// nexus/workgraph/README.md's repo mapping note). Empty wi.Repo
		// reproduces every pre-Phase-4 pool dispatch exactly.
		Repo: wi.Repo,
		// Personality: threading wi.Personality straight through to
		// PoolItem.Personality -> Brief.RequestedPersonality is the whole
		// gap per-personality routing closes — SubmitPoolItem/reserveQueued
		// already do everything else (lease targeting, provider
		// inheritance) once it's non-empty. Empty wi.Personality reproduces
		// "any free personality" exactly, same as every pre-routing pool
		// dispatch.
		Personality: wi.Personality,
	}
	if _, err := o.Dispatcher.SubmitPoolItem(ctx, item); err != nil {
		report.Errors = append(report.Errors, errf("submit %s: %v", wi.ID, err))
		return
	}

	if err := o.Graph.Transition(ctx, wi.ID, workgraph.StatusDispatched); err != nil {
		report.Errors = append(report.Errors, errf("transition %s to dispatched: %v", wi.ID, err))
		return
	}
	report.Dispatched = append(report.Dispatched, wi.ID)
}

// formatAcceptanceCriteria renders a work item's AcceptanceCriteria list as
// plain bullet text for dispatch.PoolItem.AcceptanceCriteria — Unit B
// (verified task_done, NET-22/23/24). Deliberately a plain "- " bullet
// rather than workgraph's dodTicked/dodUnticked checklist markers: this
// text is read by the agentfunnel task_done verifier (a cheap-judge
// prompt), not re-parsed back into a checklist, so there is nothing to tick.
// Empty/nil criteria formats to "" — the caller (agentfunnel) treats an
// empty AcceptanceCriteria as "no DoD captured" and honors task_done
// unconditionally, exactly like today.
func formatAcceptanceCriteria(criteria []string) string {
	if len(criteria) == 0 {
		return ""
	}
	lines := make([]string, len(criteria))
	for i, c := range criteria {
		lines[i] = "- " + c
	}
	return strings.Join(lines, "\n")
}
