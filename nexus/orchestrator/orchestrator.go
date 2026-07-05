// Package orchestrator is the M1 Unit 6 standing orchestrator MECHANISM
// (PHASE2-DESIGN §2, §2.1, §5, §6): a stateless drain pass that reads the
// work graph + worker heartbeats, dispatches ready items to the pool,
// reaps dead workers, and holds on auth failure.
//
// Scope boundary: this package is the CODE MECHANISM, not the LLM
// decomposition logic. "What work to create next" is the orchestrator's
// runtime drain PROMPT (a later concern, out of scope here) — everything
// in this package is deterministic and testable with fakes, no live LLM.
//
// See README.md for the drain-pass lifecycle, wake/cadence/poke triggers,
// reap+strike policy, auth-hold behavior, and the live-verify path.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// WorkGraph is the subset of nexus/workgraph.Client's API this package
// needs — narrowed to an interface so tests can supply an in-memory fake
// instead of a live ledger connection. workgraph.Client satisfies this
// structurally; no adapter/wrapper required.
type WorkGraph interface {
	ListReady(ctx context.Context, role, stream string) ([]workgraph.WorkItem, error)
	GetWorkItem(ctx context.Context, id string) (workgraph.WorkItem, error)
	Transition(ctx context.Context, id string, status workgraph.Status) error
	RecordResult(ctx context.Context, id string, result workgraph.Result) error
	Rework(ctx context.Context, rejectedID string, newSpec workgraph.WorkItem) (string, error)
	Claim(ctx context.Context, id, agent string) error
	Cancel(ctx context.Context, id string, requeue bool, reason string) error
}

// Dispatcher is the subset of runtime/dispatch.Runner's pool-lease API this
// package needs. dispatch.Runner satisfies this structurally via
// SubmitPoolItem (added alongside this unit — see runtime/dispatch/pool.go).
type Dispatcher interface {
	SubmitPoolItem(ctx context.Context, item dispatch.PoolItem) (string, error)
}

// WorkerStatusStore is the subset of nexus/workerstatus.Store this package
// needs (a read-only List) — workerstatus.SQLStore satisfies this
// structurally.
type WorkerStatusStore interface {
	List(ctx context.Context) ([]workerstatus.Status, error)
}

// Alerter is the fail-loud sink: PreflightAuth failure and a stale-worker
// second strike page through here. There is no push API into
// loki-alert-bridge in this codebase today (see README.md "Alerting") —
// the default LogAlerter emits a structured slog line an Alertmanager rule
// can key on; a Poster-backed Alerter (dispatch.Poster) can additionally
// post straight to a chat thread for immediate visibility.
type Alerter interface {
	Alert(ctx context.Context, subject, detail string) error
}

// LogAlerter is the default Alerter: a structured slog.Error line, tagged
// so a Loki/Alertmanager rule can match on it and forward through
// loki-alert-bridge (see README.md). Never returns an error itself.
type LogAlerter struct{ Log *slog.Logger }

func (a LogAlerter) Alert(_ context.Context, subject, detail string) error {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	log.Error("orchestrator: ALERT", "subject", subject, "detail", detail)
	return nil
}

// RoleResolver resolves a role LABEL (e.g. "builder") to the role-at-spawn
// overlay (M1 Unit 3, PHASE2-DESIGN §3) a dispatch should carry: the
// resolved role system-prompt text, the skill allowlist, and a tool-policy
// fragment. Optional — a nil Orchestrator.Resolver dispatches with the role
// label alone (RolePrompt="", SkillAllowlist=nil, PolicyFragment=nil),
// reproducing SubmitPool's original behavior exactly. See README.md
// "Role resolution (out of scope, by design)" for why this unit ships the
// seam but not a docs/network/roles/*.yaml-backed implementation.
type RoleResolver interface {
	Resolve(role string) (rolePrompt string, skillAllowlist []string, policy *funnel.ToolPolicy)
}

// DrainReport is DrainOnce's (and RecordJobResult's re-drain's) result —
// every field is a plain read of what happened this pass, nothing carried
// forward (DrainOnce is stateless; see package doc).
type DrainReport struct {
	// Dispatched lists work-item ids newly claimed + submitted to the pool
	// this pass.
	Dispatched []string
	// Skipped lists ready work-item ids NOT dispatched because they were
	// already claimed (by an earlier pass, or another concurrent drain) —
	// the idempotent-dispatch guard, see README.md.
	Skipped []string
	// Reaped lists work-item ids ReapStale requeued this pass (run at the
	// top of every DrainOnce).
	Reaped []string
	// Held is true when PreflightAuth failed: DrainOnce dispatched
	// nothing this pass, by design (see README.md "Auth-hold").
	Held bool
	// HoldReason is PreflightAuth's error text when Held is true.
	HoldReason string
	// Errors collects non-fatal per-item errors encountered mid-pass (a
	// single bad item never aborts the rest of the drain).
	Errors []string
}

// Orchestrator holds the DrainOnce/RecordJobResult/ReapStale/PreflightAuth
// machinery's collaborators. Every field except the reap strike-counter is
// read fresh from the stores on every call — see package doc "Stateless".
type Orchestrator struct {
	Graph        WorkGraph
	Dispatcher   Dispatcher
	WorkerStatus WorkerStatusStore
	Alerter      Alerter

	// AuthProbe, when set, is called at the top of every DrainOnce
	// (PreflightAuth) before any dispatch. A non-nil error HOLDS the
	// drain (nothing dispatched, items stay queued) and alerts — see
	// README.md "Auth-hold". nil disables the gate (no preflight check).
	AuthProbe func(ctx context.Context) error

	// Resolver optionally resolves a role label to its RolePrompt/
	// SkillAllowlist/PolicyFragment overlay before dispatch. nil = role
	// label only (see RoleResolver).
	Resolver RoleResolver

	// Roles is the set of role labels this orchestrator's pool serves —
	// DrainOnce calls workgraph.ListReady once per role, per stream.
	Roles []string
	// Stream optionally scopes every ListReady call to one epic subtree.
	// Empty = no stream filter (every ready item for each role).
	Stream string

	// ClaimAgent is the actor name Claim uses to atomically stake a ready
	// item before dispatch (the idempotent-dispatch guard). Defaults to
	// defaultClaimAgent.
	ClaimAgent string

	// StaleAfter is ReapStale's heartbeat-staleness threshold. Defaults to
	// defaultStaleAfter (5 minutes) when zero.
	StaleAfter time.Duration

	// Now, when set, overrides time.Now for ReapStale (tests). nil = real
	// clock.
	Now func() time.Time

	// mu guards strikes, the one piece of in-process state this package
	// keeps (see package doc "Stateless" / README.md "Reap + strike
	// policy") — a work item that reaps clean on its next heartbeat check
	// clears its strike count; a work item that's STILL stale on a later
	// pass escalates on the second strike.
	mu      sync.Mutex
	strikes map[string]int
}

const (
	defaultClaimAgent = "orchestrator-drain"
	defaultStaleAfter = 5 * time.Minute
)

func (o *Orchestrator) claimAgent() string {
	if o.ClaimAgent != "" {
		return o.ClaimAgent
	}
	return defaultClaimAgent
}

func (o *Orchestrator) staleAfter() time.Duration {
	if o.StaleAfter > 0 {
		return o.StaleAfter
	}
	return defaultStaleAfter
}

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Orchestrator) alert(ctx context.Context, subject, detail string) {
	if o.Alerter == nil {
		return
	}
	if err := o.Alerter.Alert(ctx, subject, detail); err != nil {
		slog.Warn("orchestrator: alert delivery failed", "subject", subject, "err", err)
	}
}

func (o *Orchestrator) resolve(role string) (string, []string, *funnel.ToolPolicy) {
	if o.Resolver == nil {
		return "", nil, nil
	}
	return o.Resolver.Resolve(role)
}

// errf is a tiny helper so per-item errors read consistently in
// DrainReport.Errors.
func errf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
