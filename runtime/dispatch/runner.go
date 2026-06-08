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

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
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
	PutBriefConfigMap(ctx context.Context, taskID, brief string) error
	CreateJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error)
	SetBriefOwner(ctx context.Context, taskID string, job *batchv1.Job) error
	ListActiveJobs(ctx context.Context) (map[string]ActiveJob, error)
	WatchJobs(ctx context.Context, onDone func(JobDone)) error
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

	mu        sync.Mutex
	ctx       context.Context   // stored at Init for background callbacks (OnJobDone)
	agentBusy map[string]string // agent name → runID of its active run
	active    map[string]*Run   // runID → Run
	queue     []Brief
	acked     map[string]bool
	seq       int
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
		ackMsg = "dispatch accepted for " + b.Agent + " on " + b.Ticket
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
	return true
}

// reserve stamps a RunID, records the run, and marks the agent busy. Caller holds r.mu.
func (r *Runner) reserve(b Brief) *Run {
	runID := r.nextID()
	b.RunID = runID
	run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, Started: time.Now()}
	r.active[runID] = run
	r.agentBusy[b.Agent] = runID
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
	r.post(done.Thread, r.completionSummary(run, done))

	r.launchPending(r.ctx, pending)
}

func (r *Runner) completionSummary(run *Run, done JobDone) string {
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
	r.seq++
	return fmt.Sprintf("run-%d", r.seq)
}

// reserveQueued pulls queued briefs whose agent is now free (and within the
// global cap) and reserves them, preserving order for the rest. Caller holds
// r.mu. Returns the reserved runs for the caller to launch via launchPending.
func (r *Runner) reserveQueued() []*Run {
	var runs []*Run
	kept := make([]Brief, 0, len(r.queue))
	for _, b := range r.queue {
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
	r.post(run.Brief.Thread, "builder spawned as "+run.Brief.Agent+" ("+job.Name+")")
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
	if err := k.EnsureKeyfileSecret(ctx, b.Agent); err != nil {
		return fmt.Errorf("ensure keyfile for %s: %w", b.Agent, err)
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
	return k.PutBriefConfigMap(ctx, taskID, b.Task)
}
