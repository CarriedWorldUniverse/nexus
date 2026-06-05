package dispatch

import (
	"context"
	"fmt"
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

	mu     sync.Mutex
	active map[string]string
	queue  []Brief
	acked  map[string]bool
	seq    int
}

func (c *Controller) Init(ctx context.Context) error {
	c.mu.Lock()
	if c.MaxConc <= 0 {
		c.MaxConc = 4
	}
	if c.active == nil {
		c.active = map[string]string{}
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
	for ticket, job := range active {
		c.active[ticket] = job
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
	job := BuildJob(b, c.Cfg, taskID)
	if err := c.K8s.CreateJob(ctx, job); err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	c.active[b.Ticket] = job.Name
	c.post(b.Thread, "builder spawned as "+b.Agent+" ("+job.Name+")")
	return nil
}

func (c *Controller) onJobDone(ctx context.Context, ticket, thread string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, ticket)
	if ok {
		c.post(thread, "builder completed: "+ticket)
	} else {
		c.post(thread, "builder FAILED: "+ticket+" - see Job logs; re-dispatch to retry")
	}
	if len(c.queue) > 0 && len(c.active) < c.MaxConc {
		next := c.queue[0]
		c.queue = c.queue[1:]
		_ = c.spawn(ctx, next)
	}
}
