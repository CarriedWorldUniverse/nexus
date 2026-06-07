package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
)

// ErrPoolExhausted is returned by Submit when all pool slots are in use
// and the concurrency cap is reached. Callers should queue and retry.
var ErrPoolExhausted = errors.New("dispatch: all builder pool slots in use")

// K8sIface is the subset of K8s used by Runner, extracted for testing.
type K8sIface interface {
	EnsureKeyfileSecret(ctx context.Context, aspect string) error
	PutBriefConfigMap(ctx context.Context, taskID, brief string) error
	CreateJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error)
	SetBriefOwner(ctx context.Context, taskID string, job *batchv1.Job) error
	ListActiveJobs(ctx context.Context) (map[string]ActiveJob, error)
	WatchJobs(ctx context.Context, onDone func(ticket, thread string, ok bool)) error
}

// Submitter is the interface the broker calls for !dispatch interception.
type Submitter interface {
	Submit(ctx context.Context, b Brief) (string, error)
}

// Runner is the broker-embedded dispatch engine.
// It replaces Controller: no per-agent serialization, just pool-slot tracking.
// Multiple runs for the same logical agent can proceed in parallel when
// free pool slots exist.
type Runner struct {
	K8sIface K8sIface
	Cfg      JobConfig
	Pool     []string // ordered list of pool-slot aspect names
	MaxConc  int
	Poster   Poster
	NewID    func() string

	mu        sync.Mutex
	poolInUse map[string]string // slot name → runID currently using it
	active    map[string]*Run   // runID → Run
	queue     []Brief
	acked     map[string]bool
	seq       int
}

// Init initializes Runner maps and optionally recovers active jobs from K8s.
func (r *Runner) Init(ctx context.Context) error {
	r.mu.Lock()
	if r.MaxConc <= 0 {
		r.MaxConc = len(r.Pool)
	}
	if r.poolInUse == nil {
		r.poolInUse = map[string]string{}
	}
	if r.active == nil {
		r.active = map[string]*Run{}
	}
	if r.acked == nil {
		r.acked = map[string]bool{}
	}
	r.mu.Unlock()

	if r.K8sIface == nil {
		return nil
	}
	active, err := r.K8sIface.ListActiveJobs(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ticket, aj := range active {
		// Recover: map ticket back to a placeholder Run.
		run := &Run{
			ID:       "recovered-" + ticket,
			Brief:    Brief{Ticket: ticket, Agent: aj.Agent},
			JobName:  aj.Name,
			PoolSlot: aj.Agent,
		}
		r.active[run.ID] = run
	}
	return nil
}

// WatchLoop calls K8sIface.WatchJobs to watch for job completions.
func (r *Runner) WatchLoop(ctx context.Context) error {
	return r.K8sIface.WatchJobs(ctx, r.OnJobDone)
}

// Submit provisions and launches a dispatch run. Returns a RunID.
// Returns ErrPoolExhausted if all slots are occupied; the brief is queued
// and will be spawned when a slot becomes free.
func (r *Runner) Submit(ctx context.Context, b Brief) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Idempotency: if a run for this ticket is already active, return its ID.
	for _, run := range r.active {
		if run.Brief.Ticket == b.Ticket {
			return run.ID, nil
		}
	}

	if !r.acked[b.Ticket] {
		r.acked[b.Ticket] = true
		r.post(b.Thread, "dispatch accepted for "+b.Agent+" on "+b.Ticket)
	}

	slot := r.pickFreeSlot()
	if slot == "" {
		r.queue = append(r.queue, b)
		r.post(b.Thread, "dispatch queued (all "+strconv.Itoa(len(r.Pool))+" builder slots in use)")
		return "", ErrPoolExhausted
	}

	return r.spawn(ctx, b, slot)
}

// SlotFree reports whether the named pool slot is available. Used in tests.
func (r *Runner) SlotFree(slot string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, inUse := r.poolInUse[slot]
	return !inUse
}

// OnJobDone releases the pool slot for the completed ticket and drains the queue.
func (r *Runner) OnJobDone(ticket, thread string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Find and release the run for this ticket.
	var doneID string
	for id, run := range r.active {
		if run.Brief.Ticket == ticket {
			doneID = id
			break
		}
	}
	if doneID == "" {
		return
	}
	run := r.active[doneID]
	delete(r.active, doneID)
	delete(r.poolInUse, run.PoolSlot)

	if ok {
		r.post(thread, "builder completed: "+ticket)
	} else {
		r.post(thread, "builder FAILED: "+ticket+" — see Job logs; re-dispatch to retry")
	}

	r.drainQueue(context.Background())
}

func (r *Runner) nextID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	r.seq++
	return fmt.Sprintf("run-%d", r.seq)
}

func (r *Runner) pickFreeSlot() string {
	for _, slot := range r.Pool {
		if _, inUse := r.poolInUse[slot]; !inUse {
			return slot
		}
	}
	return ""
}

// spawn provisions and creates the Job. Caller holds r.mu.
func (r *Runner) spawn(ctx context.Context, b Brief, slot string) (string, error) {
	runID := r.nextID()
	taskID := runID
	b.RunID = runID
	b.PoolSlot = slot

	if r.K8sIface != nil {
		if err := provisionRun(ctx, r.K8sIface, r.Cfg, b, taskID); err != nil {
			return "", err
		}
	}
	provider := b.Provider
	if provider == "" {
		provider = "claude"
	}
	job := BuildJob(b, r.Cfg, taskID, provider)
	if r.K8sIface != nil {
		created, err := r.K8sIface.CreateJob(ctx, job)
		if err != nil {
			return "", fmt.Errorf("runner: create job: %w", err)
		}
		if err := r.K8sIface.SetBriefOwner(ctx, taskID, created); err != nil {
			slog.Warn("runner: brief will not auto-GC", "task", taskID, "err", err)
		}
		job = created
	}

	run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, JobName: job.Name, PoolSlot: slot}
	r.active[runID] = run
	r.poolInUse[slot] = runID
	r.post(b.Thread, "builder spawned as "+slot+" ("+job.Name+")")
	return runID, nil
}

// drainQueue spawns queued briefs when slots free up. Caller holds r.mu.
func (r *Runner) drainQueue(ctx context.Context) {
	for len(r.queue) > 0 {
		slot := r.pickFreeSlot()
		if slot == "" {
			return
		}
		next := r.queue[0]
		r.queue = r.queue[1:]
		if _, err := r.spawn(ctx, next, slot); err != nil {
			r.post(next.Thread, "dispatch failed: "+err.Error())
		}
	}
}

func (r *Runner) post(thread, text string) {
	if r.Poster != nil {
		_ = r.Poster.Post(thread, text)
	}
}

// provisionRun provisions keyfile secret and brief ConfigMap for a run.
// Git credential issuance (matching provision.go) is included when GitCredName is set.
func provisionRun(ctx context.Context, k K8sIface, cfg JobConfig, b Brief, taskID string) error {
	if err := k.EnsureKeyfileSecret(ctx, b.PoolSlot); err != nil {
		return fmt.Errorf("ensure keyfile for %s: %w", b.PoolSlot, err)
	}
	if cfg.GitCredName != "" && b.Repo != "" {
		cmd := execCommandContext(ctx, "cw", "credential", "issue-git-permission",
			"--aspect", b.PoolSlot, "--name", cfg.GitCredName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("provision: git-cred grant: %w (%s)", err, out)
		}
	} else {
		slog.Info("dispatch: skipping git credential grant; git credential name not configured",
			"agent", b.PoolSlot, "repo", b.Repo)
	}
	return k.PutBriefConfigMap(ctx, taskID, b.Task)
}
