package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
)

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

func NewWsPoster(ctx context.Context, sender ChatSender) Poster {
	return wsPoster{ctx: ctx, sender: sender}
}

func (p wsPoster) Post(thread, text string) error {
	_, err := p.sender.SendChat(p.ctx, text, 0, thread)
	return err
}

type Controller struct {
	K8s     *K8s
	Cfg     JobConfig
	MaxConc int
	Poster  Poster
	NewID   func() string

	mu      sync.Mutex
	active  map[string]string
	agentOf map[string]string // ticket -> agent, for per-agent serialization (NEX-464)
	queue   []Brief
	acked   map[string]bool
	seq     int
}

func (c *Controller) Init(ctx context.Context) error {
	c.mu.Lock()
	if c.MaxConc <= 0 {
		c.MaxConc = 4
	}
	if c.active == nil {
		c.active = map[string]string{}
	}
	if c.agentOf == nil {
		c.agentOf = map[string]string{}
	}
	if c.acked == nil {
		c.acked = map[string]bool{}
	}
	c.mu.Unlock()

	active, err := c.K8s.ListActiveJobs(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for ticket, aj := range active {
		c.active[ticket] = aj.Name
		c.agentOf[ticket] = aj.Agent
	}
	return nil
}

func (c *Controller) WatchLoop(ctx context.Context) error {
	return c.K8s.WatchJobs(ctx, c.OnJobDone)
}

func (c *Controller) OnJobDone(ticket, thread string, ok bool) {
	c.onJobDone(context.Background(), ticket, thread, ok)
}

func (c *Controller) nextID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	c.seq++
	return strconv.Itoa(c.seq)
}

func (c *Controller) post(thread, text string) {
	if c.Poster != nil {
		_ = c.Poster.Post(thread, text)
	}
}

func (c *Controller) HandleMessage(ctx context.Context, body []byte) {
	b, err := ParseBrief(body)
	if err != nil {
		return
	}
	if err := c.handle(ctx, b); err != nil {
		c.post(b.Thread, "dispatch failed: "+err.Error())
	}
}

func (c *Controller) handle(ctx context.Context, b Brief) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = map[string]string{}
	}
	if c.agentOf == nil {
		c.agentOf = map[string]string{}
	}
	if c.acked == nil {
		c.acked = map[string]bool{}
	}
	if c.MaxConc <= 0 {
		c.MaxConc = 4
	}
	if !c.acked[b.Ticket] {
		c.acked[b.Ticket] = true
		c.post(b.Thread, "dispatch accepted for "+b.Agent+" on "+b.Ticket)
	}
	if _, live := c.active[b.Ticket]; live {
		return nil
	}
	// NEX-464: one builder per aspect at a time. The broker allows a single
	// session per aspect name, so a second concurrent job for the same agent
	// can't register — queue it until the agent's current builder finishes.
	if c.agentBusy(b.Agent) {
		c.queue = append(c.queue, b)
		c.post(b.Thread, "dispatch queued — "+b.Agent+" already has a live builder")
		return nil
	}
	if len(c.active) >= c.MaxConc {
		c.queue = append(c.queue, b)
		c.post(b.Thread, "dispatch queued (concurrency cap "+strconv.Itoa(c.MaxConc)+")")
		return nil
	}
	return c.spawn(ctx, b)
}

// spawn provisions and creates the Job. Caller holds c.mu.
func (c *Controller) spawn(ctx context.Context, b Brief) error {
	taskID := c.nextID()
	if err := c.Provision(ctx, b, taskID); err != nil {
		return err
	}
	provider := b.Provider
	if provider == "" {
		provider = "codex-cli"
	}
	job := BuildJob(b, c.Cfg, taskID, provider)
	created, err := c.K8s.CreateJob(ctx, job)
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	// NEX-461: make the Job own the brief ConfigMap so it GCs with the Job
	// (the Job itself TTL-deletes via TTLSecondsAfterFinished).
	if err := c.K8s.SetBriefOwner(ctx, taskID, created); err != nil {
		slog.Warn("dispatch: brief will not auto-GC; SetBriefOwner failed", "task", taskID, "err", err)
	}
	c.active[b.Ticket] = created.Name
	c.agentOf[b.Ticket] = b.Agent
	c.post(b.Thread, "builder spawned as "+b.Agent+" ("+created.Name+")")
	return nil
}

// agentBusy reports whether the agent already has a live builder. NEX-464.
// Caller holds c.mu.
func (c *Controller) agentBusy(agent string) bool {
	for _, a := range c.agentOf {
		if a == agent {
			return true
		}
	}
	return false
}

// drainQueue spawns queued briefs that can now run — under the concurrency cap
// and whose agent is free. Caller holds c.mu.
func (c *Controller) drainQueue(ctx context.Context) {
	for len(c.active) < c.MaxConc {
		idx := -1
		for i, q := range c.queue {
			if !c.agentBusy(q.Agent) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		next := c.queue[idx]
		c.queue = append(c.queue[:idx], c.queue[idx+1:]...)
		if err := c.spawn(ctx, next); err != nil {
			c.post(next.Thread, "dispatch failed: "+err.Error())
		}
	}
}

func (c *Controller) onJobDone(ctx context.Context, ticket, thread string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, ticket)
	delete(c.agentOf, ticket)
	if ok {
		c.post(thread, "builder completed: "+ticket)
	} else {
		c.post(thread, "builder FAILED: "+ticket+" - see Job logs; re-dispatch to retry")
	}
	c.drainQueue(ctx)
}
