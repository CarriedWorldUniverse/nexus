package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
)

var execCommandContext = exec.CommandContext

// ErrPoolExhausted is returned by Submit when all pool slots are in use
// and the concurrency cap is reached. Callers should queue and retry.
var ErrPoolExhausted = errors.New("dispatch: all builder pool slots in use")

// Poster sends a status line to a comms thread.
type Poster interface {
	Post(thread, text string) error
}

// ChatSender is the wsasp send-chat shape used by NewWsPoster.
type ChatSender interface {
	SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error)
}

type wsPoster struct {
	ctx    context.Context
	sender ChatSender
}

// NewWsPoster creates a Poster that sends messages to a thread via ChatSender.
func NewWsPoster(ctx context.Context, sender ChatSender) Poster {
	return wsPoster{ctx: ctx, sender: sender}
}

func (p wsPoster) Post(thread, text string) error {
	_, err := p.sender.SendChat(p.ctx, text, 0, thread)
	return err
}

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
	ctx       context.Context   // stored at Init for background callbacks (OnJobDone)
	poolInUse map[string]string // slot name → runID currently using it
	active    map[string]*Run   // runID → Run
	queue     []Brief
	acked     map[string]bool
	seq       int
}

// Init initializes Runner maps and optionally recovers active jobs from K8s.
func (r *Runner) Init(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
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
		// Recover: map ticket back to a placeholder Run and re-mark its
		// pool slot in use so a new dispatch can't be handed the same
		// builder identity while the recovered Job is still running.
		slot := aj.Slot
		if slot == "" {
			// Pre-pool-slot Job (or unlabelled): fall back to the agent
			// name, which is what older jobs used as the keyfile aspect.
			slot = aj.Agent
		}
		run := &Run{
			ID:       "recovered-" + ticket,
			Brief:    Brief{Ticket: ticket, Agent: aj.Agent, PoolSlot: slot},
			JobName:  aj.Name,
			PoolSlot: slot,
		}
		r.active[run.ID] = run
		if slot != "" {
			r.poolInUse[slot] = run.ID
		}
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
//
// The runner mutex guards only in-memory bookkeeping (slot reservation +
// maps). The slow path — provisioning (cw exec), CreateJob (k8s API), and
// status posts (WS send) — runs OUTSIDE the lock so concurrent dispatches
// and job-completion callbacks don't serialize behind one run's I/O.
func (r *Runner) Submit(ctx context.Context, b Brief) (string, error) {
	r.mu.Lock()

	// Idempotency: a run for this ticket already active → return its ID.
	for _, run := range r.active {
		if run.Brief.Ticket == b.Ticket {
			id := run.ID
			r.mu.Unlock()
			return id, nil
		}
	}
	// Idempotency: a brief for this ticket already queued (slots were full
	// on a prior submit) → no-op rather than enqueue a duplicate that would
	// double-spawn when a slot frees.
	for _, q := range r.queue {
		if q.Ticket == b.Ticket {
			r.mu.Unlock()
			return "", nil
		}
	}

	var ackMsg string
	if !r.acked[b.Ticket] {
		r.acked[b.Ticket] = true
		ackMsg = "dispatch accepted for " + b.Agent + " on " + b.Ticket
	}

	slot := r.pickFreeSlot()
	if slot == "" {
		r.queue = append(r.queue, b)
		thread := b.Thread
		poolLen := len(r.Pool)
		r.mu.Unlock()
		if ackMsg != "" {
			r.post(thread, ackMsg)
		}
		r.post(thread, "dispatch queued (all "+strconv.Itoa(poolLen)+" builder slots in use)")
		return "", ErrPoolExhausted
	}

	run := r.reserve(b, slot)
	r.mu.Unlock()

	if ackMsg != "" {
		r.post(run.Brief.Thread, ackMsg)
	}
	if err := r.launch(ctx, run); err != nil {
		r.mu.Lock()
		delete(r.active, run.ID)
		delete(r.poolInUse, run.PoolSlot)
		r.mu.Unlock()
		r.post(run.Brief.Thread, "dispatch failed: "+err.Error())
		return "", err
	}
	return run.ID, nil
}

// reserve assigns brief b to slot: stamps RunID/PoolSlot, creates the run
// placeholder, and marks the slot in use. Caller holds r.mu.
func (r *Runner) reserve(b Brief, slot string) *Run {
	runID := r.nextID()
	b.RunID = runID
	b.PoolSlot = slot
	run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, PoolSlot: slot}
	r.active[runID] = run
	r.poolInUse[slot] = runID
	return run
}

// SlotFree reports whether the named pool slot is available. Used in tests.
func (r *Runner) SlotFree(slot string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, inUse := r.poolInUse[slot]
	return !inUse
}

// OnJobDone releases the pool slot for the completed ticket and drains the
// queue onto the freed slot. Slot release + queue reservation happen under
// the lock; status posts and the slow launch of any drained run run outside.
func (r *Runner) OnJobDone(ticket, thread string, ok bool) {
	r.mu.Lock()

	// Find and release the run for this ticket.
	var doneID string
	for id, run := range r.active {
		if run.Brief.Ticket == ticket {
			doneID = id
			break
		}
	}
	if doneID == "" {
		r.mu.Unlock()
		return
	}
	run := r.active[doneID]
	delete(r.active, doneID)
	delete(r.poolInUse, run.PoolSlot)
	delete(r.acked, ticket)
	pending := r.reserveQueued()
	r.mu.Unlock()

	if ok {
		r.post(thread, "builder completed: "+ticket)
	} else {
		r.post(thread, "builder FAILED: "+ticket+" — see Job logs; re-dispatch to retry")
	}

	r.launchPending(r.ctx, pending)
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

// reserveQueued assigns queued briefs to free slots, reserving each slot and
// creating its run placeholder. Caller holds r.mu. Returns the reserved runs
// for the caller to launch outside the lock via launchPending.
func (r *Runner) reserveQueued() []*Run {
	var runs []*Run
	for len(r.queue) > 0 {
		slot := r.pickFreeSlot()
		if slot == "" {
			break
		}
		b := r.queue[0]
		r.queue = r.queue[1:]
		runs = append(runs, r.reserve(b, slot))
	}
	return runs
}

// launchPending launches reserved runs outside the lock. On launch error the
// run's reservation is rolled back and its brief re-enqueued at the front, so
// a transient K8s failure does not silently discard queued work; draining
// stops at the first failure and retries on the next slot-free.
func (r *Runner) launchPending(ctx context.Context, runs []*Run) {
	for _, run := range runs {
		if err := r.launch(ctx, run); err != nil {
			r.mu.Lock()
			delete(r.active, run.ID)
			delete(r.poolInUse, run.PoolSlot)
			r.queue = append([]Brief{run.Brief}, r.queue...)
			r.mu.Unlock()
			r.post(run.Brief.Thread, "dispatch spawn failed, will retry on next slot free: "+err.Error())
			return
		}
	}
}

// launch provisions and creates the Job for an already-reserved run. Runs
// OUTSIDE r.mu: provisionRun execs the cw CLI and CreateJob hits the k8s API,
// neither of which should block other dispatches. Sets run.JobName under the
// lock once known.
func (r *Runner) launch(ctx context.Context, run *Run) error {
	taskID := run.ID
	if r.K8sIface != nil {
		if err := provisionRun(ctx, r.K8sIface, r.Cfg, run.Brief, taskID); err != nil {
			return err
		}
	}
	provider := run.Brief.Provider
	if provider == "" {
		provider = "claude"
	}
	job := BuildJob(run.Brief, r.Cfg, taskID, provider)
	if r.K8sIface != nil {
		created, err := r.K8sIface.CreateJob(ctx, job)
		if err != nil {
			return fmt.Errorf("runner: create job: %w", err)
		}
		if err := r.K8sIface.SetBriefOwner(ctx, taskID, created); err != nil {
			slog.Warn("runner: brief will not auto-GC", "task", taskID, "err", err)
		}
		job = created
	}

	r.mu.Lock()
	run.JobName = job.Name
	r.mu.Unlock()
	r.post(run.Brief.Thread, "builder spawned as "+run.PoolSlot+" ("+job.Name+")")
	return nil
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
