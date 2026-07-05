package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
)

var execCommandContext = exec.CommandContext

var lookupPRURL = lookupPRURLWithGH

// SetLookupPRURLForTest swaps PR lookup and returns a restore function.
func SetLookupPRURLForTest(fn func(repo, branch string) (string, error)) func() {
	old := lookupPRURL
	lookupPRURL = fn
	return func() { lookupPRURL = old }
}

// Poster sends a status line to a comms thread.
type Poster interface {
	Post(thread, text string) error
}

// ChatSender is the send-chat shape used by NewWsPoster.
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
	EnsureHomeRepo(ctx context.Context, agent string) error
	EnsureSharedReposPVC(ctx context.Context) error
	PutBriefConfigMap(ctx context.Context, taskID string, data map[string]string) error
	CreateJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error)
	SetBriefOwner(ctx context.Context, taskID string, job *batchv1.Job) error
	ListActiveJobs(ctx context.Context) (map[string]ActiveJob, error)
	WatchJobs(ctx context.Context, onDone func(JobDone)) error
	GetPodLogs(ctx context.Context, jobName string) (string, error)
}

// Submitter is the interface the broker calls for !dispatch interception.
type Submitter interface {
	Submit(ctx context.Context, b Brief) (string, error)
}

// Runner is the broker-embedded dispatch engine.
//
// Each dispatch runs AS the named agent (brief.Agent): the Job mounts
// aspect-keyfile-<agent>, so the worker validates as that agent → loads its
// persona (SOUL.md/nexus.md) and signs commits/reviews as the agent. The work
// is attributed to a real team member, not a faceless pool slot.
//
// Concurrency is per-agent: one run per agent name at a time (the broker
// enforces one session per name, NEX-464). Different agents run in parallel.
// A second task for a busy agent is queued and drains when that agent frees.
// MaxConc, when > 0, additionally caps total concurrent runs across agents.
type Runner struct {
	K8sIface K8sIface
	Cfg      JobConfig
	MaxConc  int // global cap on concurrent runs; 0 = unlimited
	Poster   Poster
	NewID    func() string
	Recorder RunsRecorder

	// SpawnMaxConcurrent caps live hands PER PARENT aspect (NEX-571);
	// 0 = defaultSpawnMaxConcurrent (4). The global MaxConc still
	// applies on top.
	SpawnMaxConcurrent int

	// PoolSize caps concurrent leases of the role-based worker pool (M1
	// Unit 4, pool.go, PHASE2-DESIGN §4); 0 = defaultPoolSize (3). This
	// is a DISTINCT cap dimension from SpawnMaxConcurrent — pool slots
	// never compete with (or borrow capacity from) a named aspect's hand
	// cap. The global MaxConc still applies on top.
	PoolSize int

	// Audit stores spawn audit posts AS a named sender, returning the
	// message id — used for the spawn audit-thread root (the !dispatch
	// post-as-thread-root pattern, but originated broker-side). nil =
	// no root post; hands thread under a synthetic topic.
	Audit AuditPoster

	// MintHandCredential mints the derived session credential injected
	// into a hand's Job env (broker.KeyfileValidator.MintDerivedCredential
	// in production). Required for SubmitSpawn — a Runner without it
	// rejects spawn briefs at launch.
	MintHandCredential func(ctx context.Context, parent, derived string) (string, error)

	// AspectHandNames overrides the built-in kindred-word hand-name
	// pools per parent aspect (the P2 naming amendment). Keys are base
	// aspect names; values are the lease order. Parents absent from the
	// map use aspects.HandNamePool's built-in defaults. cmd/nexus
	// populates this from NEXUS_ASPECT_HAND_NAMES when set.
	AspectHandNames map[string][]string

	// HandProvider resolves the provider a hand of parent should run —
	// so a hand inherits the PARENT's provider binding rather than
	// defaulting to claude. nil (or an empty return) → the hand inherits
	// nothing and the launch default applies. In production cmd/nexus
	// wires this to the aspects store's provider column for the parent.
	HandProvider func(ctx context.Context, parent string) string

	mu        sync.Mutex
	ctx       context.Context   // stored at Init for background callbacks (OnJobDone)
	agentBusy map[string]string // agent name → runID of its active run
	active    map[string]*Run   // runID → Run
	queue     []Brief
	acked     map[string]bool
}

// Init initializes Runner maps and optionally recovers active jobs from K8s.
func (r *Runner) Init(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
	if r.agentBusy == nil {
		r.agentBusy = map[string]string{}
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
		// Recover: re-mark the agent busy so a new dispatch can't double-run
		// the same identity while the recovered Job is still going.
		run := &Run{
			ID:      "recovered-" + ticket,
			Brief:   Brief{Ticket: ticket, Agent: aj.Agent},
			JobName: aj.Name,
		}
		r.active[run.ID] = run
		if aj.Agent != "" {
			r.agentBusy[aj.Agent] = run.ID
		}
	}
	return nil
}

// InitWithRetry runs Init with a bounded retry-with-backoff (NEX-611).
// On a fresh broker pod the CNI may not be routable for the first few
// seconds, so the one-shot Init's ListActiveJobs call fails with "no
// route to host" and the caller used to give up — leaving the Runner
// nil and wake+spawn silently dead. attempts bounds the total tries;
// the delay doubles from baseDelay, capped at 30s (5 attempts at 4s ≈
// 58s of cover). sleepFn is the wait seam (tests inject a no-op); nil
// → real timer. Context cancellation aborts between attempts.
func (r *Runner) InitWithRetry(ctx context.Context, attempts int, baseDelay time.Duration, sleepFn func(time.Duration)) error {
	if attempts < 1 {
		attempts = 1
	}
	delay := baseDelay
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = r.Init(ctx); err == nil {
			if attempt > 1 {
				slog.Info("runner: init succeeded after retry", "attempt", attempt)
			}
			return nil
		}
		if attempt == attempts {
			break
		}
		slog.Warn("runner: init failed — retrying (in-cluster API may not be routable yet)",
			"attempt", attempt, "max_attempts", attempts, "delay", delay, "err", err)
		switch {
		case sleepFn != nil:
			sleepFn(delay)
		case ctx != nil:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		default:
			time.Sleep(delay)
		}
		if delay *= 2; delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
	return err
}

// WatchLoop calls K8sIface.WatchJobs to watch for job completions.
func (r *Runner) WatchLoop(ctx context.Context) error {
	return r.K8sIface.WatchJobs(ctx, r.OnJobDone)
}

// Submit launches a dispatch run as the named agent and returns its RunID.
// Returns ("", nil) when the brief is accepted but queued — the agent is busy
// or the global cap is reached — and will spawn when capacity frees.
//
// The mutex guards only in-memory bookkeeping. The slow path — provisioning
// (cw exec), CreateJob (k8s API), and status posts (WS send) — runs OUTSIDE
// the lock so concurrent dispatches and completion callbacks don't serialize
// behind one run's I/O.
func (r *Runner) Submit(ctx context.Context, b Brief) (string, error) {
	r.mu.Lock()

	slog.Info("runner: submit received", "agent", b.Agent, "ticket", b.Ticket, "repo", b.Repo)
	// Idempotency: a run for this ticket already active → return its ID.
	for _, run := range r.active {
		if run.Brief.Ticket == b.Ticket {
			id := run.ID
			r.mu.Unlock()
			slog.Info("runner: ticket already active — returning existing run, no new spawn", "ticket", b.Ticket, "run_id", id)
			return id, nil
		}
	}
	// Idempotency: a brief for this ticket already queued → no-op rather than
	// enqueue a duplicate that would double-spawn when capacity frees.
	for _, q := range r.queue {
		if q.Ticket == b.Ticket {
			r.mu.Unlock()
			slog.Info("runner: ticket already queued — no-op", "ticket", b.Ticket)
			return "", nil
		}
	}

	var ackMsg string
	if !r.acked[b.Ticket] {
		r.acked[b.Ticket] = true
		ackMsg = "dispatch submitted for " + b.Agent + " on " + b.Ticket
	}

	if !r.canRun(b.Agent) {
		r.queue = append(r.queue, b)
		thread := b.Thread
		slog.Info("runner: agent busy or at cap — queued", "agent", b.Agent, "ticket", b.Ticket, "active", len(r.active), "max_conc", r.MaxConc)
		r.mu.Unlock()
		if ackMsg != "" {
			r.post(thread, ackMsg)
		}
		r.post(thread, "dispatch queued ("+b.Agent+" busy)")
		return "", nil
	}

	run := r.reserve(b)
	r.mu.Unlock()
	slog.Info("runner: reserved — launching job", "agent", b.Agent, "ticket", b.Ticket, "run_id", run.ID)

	if ackMsg != "" {
		r.post(run.Brief.Thread, ackMsg)
	}
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
		r.post(run.Brief.Thread, "dispatch failed: "+err.Error())
		return "", err
	}
	return run.ID, nil
}

// canRun reports whether a run for agent may start now. Caller holds r.mu.
func (r *Runner) canRun(agent string) bool {
	if _, busy := r.agentBusy[agent]; busy {
		return false
	}
	if r.MaxConc > 0 && len(r.active) >= r.MaxConc {
		return false
	}
	// Per-parent hand cap (NEX-571): a derived identity only starts
	// while its base aspect has spare hand slots. Applies on submit AND
	// on queue drain, so queued hands wait for a sibling to finish. The
	// pool base (M1 Unit 4, pool.go) uses its OWN cap dimension
	// (poolSize, default 3) here — a distinct dimension from the
	// per-aspect hand cap (spawnMaxConcurrent, default 4) so growing one
	// never silently loosens or tightens the other.
	if base := aspects.BaseName(agent); base != agent && r.liveHands(base) >= r.capForBase(base) {
		return false
	}
	return true
}

// capForBase returns the live-hand cap that applies to base — the pool
// cap (poolSize) for the pool identity, the generic per-aspect hand cap
// (spawnMaxConcurrent) for everything else. Caller holds r.mu.
func (r *Runner) capForBase(base string) int {
	if base == poolParentName {
		return r.poolSize()
	}
	return r.spawnMaxConcurrent()
}

// liveHands counts base's busy derived identities. Caller holds r.mu.
func (r *Runner) liveHands(base string) int {
	n := 0
	for name := range r.agentBusy {
		if aspects.IsDerivedName(name) && aspects.BaseName(name) == base {
			n++
		}
	}
	return n
}

func (r *Runner) spawnMaxConcurrent() int {
	if r.SpawnMaxConcurrent > 0 {
		return r.SpawnMaxConcurrent
	}
	return defaultSpawnMaxConcurrent
}

// reserve stamps a RunID, records the run, and marks the agent busy. Caller holds r.mu.
func (r *Runner) reserve(b Brief) *Run {
	runID := r.nextID()
	b.RunID = runID
	run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, Started: time.Now()}
	r.active[runID] = run
	r.agentBusy[b.Agent] = runID
	if r.Recorder != nil {
		ctx := r.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		r.Recorder.RecordRunStart(ctx, runID, b.Ticket, b.Agent, b.Thread, b.Repo, b.Task, b.ParentRunID, b.DispatchMsgID)
	}
	return run
}

// AgentBusy reports whether the named agent has an active run. Used in tests.
func (r *Runner) AgentBusy(agent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, busy := r.agentBusy[agent]
	return busy
}

// OnJobDone frees the completed run's agent and drains any queued briefs whose
// agent is now free. Bookkeeping happens under the lock; status posts and the
// slow launch of drained runs happen outside.
func (r *Runner) OnJobDone(done JobDone) {
	r.mu.Lock()

	var doneID string
	for id, run := range r.active {
		if run.Brief.Ticket == done.Ticket {
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
	delete(r.agentBusy, run.Brief.Agent)
	delete(r.acked, done.Ticket)
	pending := r.reserveQueued()
	r.mu.Unlock()

	if done.Thread == "" {
		done.Thread = run.Brief.Thread
	}
	if done.Agent == "" {
		done.Agent = run.Brief.Agent
	}
	if done.StartedAt.IsZero() {
		done.StartedAt = run.Started
	}
	if done.CompletedAt.IsZero() {
		done.CompletedAt = time.Now()
	}
	if r.Recorder != nil {
		ctx := r.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if r.K8sIface != nil && run.JobName != "" {
			logs, err := r.K8sIface.GetPodLogs(ctx, run.JobName)
			if err != nil {
				slog.Warn("dispatch: capture builder logs failed", "run_id", run.ID, "job", run.JobName, "err", err)
			} else {
				r.Recorder.RecordRunLogs(ctx, run.ID, logs)
			}
		}
		dur := int(done.CompletedAt.Sub(done.StartedAt).Seconds())
		r.Recorder.RecordRunDone(ctx, run.ID, statusFor(done.OK), done.CompletedAt, prURLForRun(run), dur)
	}
	r.post(done.Thread, r.completionSummary(run, done))

	r.launchPending(r.ctx, pending)
}

func (r *Runner) completionSummary(run *Run, done JobDone) string {
	// Pool-lease completion (M1 Unit 4): accountability is the slot
	// identity + role + work item, not the builder branch/PR block or
	// the hand-of-parent lineage line.
	if run.Brief.SpawnParent == poolParentName {
		status := "done"
		if !done.OK {
			status = "failed"
		}
		return strings.Join([]string{
			"pool " + status + ": slot=" + run.Brief.Agent + " role=" + run.Brief.Role + " work_item=" + run.Brief.WorkItemID,
			"duration: " + formatDuration(done.StartedAt, done.CompletedAt),
			"turns: " + formatCount(r.countActivityTurns(done.Agent, done.StartedAt, done.CompletedAt)),
		}, "\n")
	}

	// Hand completion (NEX-571): carry the lineage instead of the
	// builder branch/PR block — hands do fan-out work in their parent's
	// thread, not single-ticket PR runs.
	if run.Brief.SpawnParent != "" {
		status := "done"
		if !done.OK {
			status = "failed"
		}
		return strings.Join([]string{
			"hand " + status + ": " + run.Brief.Agent + " (hand of " + run.Brief.SpawnParent + ")",
			"duration: " + formatDuration(done.StartedAt, done.CompletedAt),
			"turns: " + formatCount(r.countActivityTurns(done.Agent, done.StartedAt, done.CompletedAt)),
		}, "\n")
	}

	branch := run.Brief.Branch
	if branch == "" {
		branch = "builder/" + run.Brief.Ticket
	}
	prURL, prErr := lookupPRURL(run.Brief.Repo, branch)
	duration := formatDuration(done.StartedAt, done.CompletedAt)
	turns := r.countActivityTurns(done.Agent, done.StartedAt, done.CompletedAt)

	status := "done"
	if !done.OK {
		status = "failed"
	}
	lines := []string{
		"builder " + status + ": " + run.Brief.Ticket,
		"branch: " + branch,
		"duration: " + duration,
		"turns: " + formatCount(turns),
	}
	if prURL != "" {
		lines = append(lines, "PR: "+prURL)
	} else if prErr != nil {
		lines = append(lines, "PR: not resolved ("+prErr.Error()+")")
	} else {
		lines = append(lines, "PR: not found")
	}
	return strings.Join(lines, "\n")
}

func formatDuration(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "unknown"
	}
	return end.Sub(start).Round(time.Second).String()
}

func formatCount(n int) string {
	if n < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", n)
}

func lookupPRURLWithGH(repo, branch string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("repo not set")
	}
	if branch == "" {
		return "", fmt.Errorf("branch not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, "gh", "pr", "list",
		"--repo", repo,
		"--head", branch,
		"--state", "open",
		"--json", "url",
		"-q", ".[0].url",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Runner) countActivityTurns(aspect string, start, end time.Time) int {
	if r.Cfg.ActivityDir == "" || aspect == "" || start.IsZero() || end.IsZero() {
		return -1
	}
	total := 0
	for day := dayStart(start); !day.After(end); day = day.AddDate(0, 0, 1) {
		n, err := countTurnFrames(filepath.Join(r.Cfg.ActivityDir, aspect, day.Format("2006-01-02")+".jsonl"), start, end)
		if err != nil {
			slog.Warn("dispatch: activity turn count unavailable", "aspect", aspect, "path", filepath.Join(r.Cfg.ActivityDir, aspect, day.Format("2006-01-02")+".jsonl"), "err", err)
			return -1
		}
		total += n
	}
	return total
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func countTurnFrames(path string, start, end time.Time) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var count int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var frame observability.Frame
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
			return 0, err
		}
		if frame.Kind != observability.FrameTurn || frame.TS.Before(start) || frame.TS.After(end) {
			continue
		}
		var turn observability.TurnFrame
		if err := json.Unmarshal(frame.Payload, &turn); err != nil {
			return 0, err
		}
		if turn.Status != observability.TurnComplete && turn.Status != observability.TurnErrored {
			continue
		}
		if turn.Label != "" && turn.Label != "main" {
			continue
		}
		count++
	}
	return count, sc.Err()
}

func (r *Runner) nextID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	return "run-" + uuid.NewString()
}

// reserveQueued pulls queued briefs whose agent is now free (and within the
// global cap) and reserves them, preserving order for the rest. Caller holds
// r.mu. Returns the reserved runs for the caller to launch via launchPending.
func (r *Runner) reserveQueued() []*Run {
	var runs []*Run
	kept := make([]Brief, 0, len(r.queue))
	for _, b := range r.queue {
		// A queued pool work-item (pool.go) carries no Agent yet — any
		// free slot will do, so it can't be checked via canRun(agent)
		// like a fixed-identity brief. Lease whichever slot is free now.
		if b.SpawnParent == poolParentName && b.Agent == "" {
			if name := r.tryLeasePoolSlot(); name != "" {
				b.Agent = name
				runs = append(runs, r.reserve(b))
			} else {
				kept = append(kept, b)
			}
			continue
		}
		if r.canRun(b.Agent) {
			runs = append(runs, r.reserve(b))
		} else {
			kept = append(kept, b)
		}
	}
	r.queue = kept
	return runs
}

// launchPending launches reserved runs outside the lock. On launch error the
// run's reservation is rolled back and its brief re-enqueued at the front, so
// a transient K8s failure doesn't silently discard queued work; draining stops
// at the first failure and retries on the next agent-free.
func (r *Runner) launchPending(ctx context.Context, runs []*Run) {
	for _, run := range runs {
		if err := r.launch(ctx, run); err != nil {
			r.mu.Lock()
			delete(r.active, run.ID)
			delete(r.agentBusy, run.Brief.Agent)
			r.queue = append([]Brief{run.Brief}, r.queue...)
			r.mu.Unlock()
			if r.Recorder != nil {
				doneCtx := ctx
				if doneCtx == nil {
					doneCtx = context.Background()
				}
				r.Recorder.RecordRunDone(doneCtx, run.ID, "failed", time.Now(), "", 0)
			}
			r.post(run.Brief.Thread, "dispatch spawn failed, will retry on next agent-free: "+err.Error())
			return
		}
	}
}

// launch provisions and creates the Job for an already-reserved run. Runs
// OUTSIDE r.mu: provisionRun execs the cw CLI and CreateJob hits the k8s API.
// Sets run.JobName under the lock once known.
func (r *Runner) launch(ctx context.Context, run *Run) error {
	taskID := run.ID
	if r.K8sIface != nil {
		if err := provisionRun(ctx, r.K8sIface, r.Cfg, run.Brief, taskID); err != nil {
			return err
		}
	}
	// Hand briefs: mint the derived credential at launch time (not at
	// enqueue) so a queued hand boots with a fresh TTL.
	if run.Brief.SpawnParent != "" {
		if r.MintHandCredential == nil {
			return fmt.Errorf("spawn: no hand-credential minter configured")
		}
		tok, err := r.MintHandCredential(ctx, run.Brief.SpawnParent, run.Brief.Agent)
		if err != nil {
			return fmt.Errorf("spawn: mint credential for %s: %w", run.Brief.Agent, err)
		}
		run.Brief.SessionJWT = tok
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
	slog.Info("runner: builder job created", "agent", run.Brief.Agent, "ticket", run.Brief.Ticket, "job", job.Name)
	kind := "builder"
	if run.Brief.SpawnParent != "" {
		kind = "hand"
	}
	r.post(run.Brief.Thread, kind+" spawned as "+run.Brief.Agent+" ("+job.Name+")")
	return nil
}

func (r *Runner) post(thread, text string) {
	if r.Poster != nil {
		_ = r.Poster.Post(thread, text)
	}
}

// provisionRun ensures the agent's keyfile secret exists, optionally issues a
// scoped git credential, and writes the brief ConfigMap. The worker runs AS
// the named agent, so the keyfile + cred are keyed on b.Agent.
func provisionRun(ctx context.Context, k K8sIface, cfg JobConfig, b Brief, taskID string) error {
	// Hand briefs have no keyfile secret — their identity is the
	// broker-minted session JWT injected as Job env (NEX-571 Task B);
	// the mint happens in launch, beside this seam.
	if b.SpawnParent == "" {
		if err := k.EnsureKeyfileSecret(ctx, b.Agent); err != nil {
			return fmt.Errorf("ensure keyfile for %s: %w", b.Agent, err)
		}
	}
	if err := k.EnsureHomeRepo(ctx, b.Agent); err != nil {
		return fmt.Errorf("ensure home repo for %s: %w", b.Agent, err)
	}
	if err := k.EnsureSharedReposPVC(ctx); err != nil {
		return fmt.Errorf("ensure shared repos PVC: %w", err)
	}
	if cfg.GitCredName != "" && b.Repo != "" {
		cmd := execCommandContext(ctx, "cw", "credential", "issue-git-permission",
			"--aspect", b.Agent, "--name", cfg.GitCredName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("provision: git-cred grant: %w (%s)", err, out)
		}
	} else {
		slog.Info("dispatch: skipping git credential grant; git credential name not configured",
			"agent", b.Agent, "repo", b.Repo)
	}
	data, err := briefConfigMapData(b)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	return k.PutBriefConfigMap(ctx, taskID, data)
}
