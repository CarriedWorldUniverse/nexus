// spawn.request — aspect-owned fan-out ("hands", roundtable P2 /
// NEX-571).
//
// A registered aspect sends spawn.request on its own authenticated WS
// to fan work out to fresh-context instances of ITSELF. This is the
// sibling shape to the !dispatch ticket-builder path (dispatch_intercept
// .go): same Runner, same Job machinery, same audit-thread pattern —
// but requested by an aspect mid-turn via RPC rather than parsed out of
// chat, and the worker boots with the requester's OWN persona under a
// derived identity (`<parent>.sub-N`) instead of another named agent's.
//
// The handler is deliberately thin: identity + shape validation here,
// everything stateful (derived-name minting, caps, queueing, audit
// posts) in the Runner's SubmitSpawn.

package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// defaultSpawnMaxPerRequest caps Count on a single spawn.request when
// Config.SpawnMaxPerRequest is unset.
const defaultSpawnMaxPerRequest = 4

// SpawnSubmitter is the Runner shape the spawn.request handler calls.
// *dispatch.Runner satisfies it; the broker type-asserts its configured
// Runner so brokers wired with a non-spawning Submitter (tests, legacy)
// degrade to a clean "spawn not available" error.
type SpawnSubmitter interface {
	SubmitSpawn(ctx context.Context, parent, brief string, count int, thread string) ([]dispatch.SpawnHandle, error)
}

// The production Runner must keep satisfying the spawn seam.
var _ SpawnSubmitter = (*dispatch.Runner)(nil)

// spawnMaxPerRequest is the effective per-request hand cap.
func (b *Broker) spawnMaxPerRequest() int {
	if b.cfg.SpawnMaxPerRequest > 0 {
		return b.cfg.SpawnMaxPerRequest
	}
	return defaultSpawnMaxPerRequest
}

// handleSpawnRequestFrame routes a spawn.request off the read loop.
// SubmitSpawn posts audit messages and talks to the k8s API, so it runs
// in a goroutine — same pattern as dispatch/turn/CWB.
func (c *wsConn) handleSpawnRequestFrame(env frames.Envelope) {
	go c.executeSpawn(env)
}

func (c *wsConn) executeSpawn(env frames.Envelope) {
	// The parent identity is the connection's REGISTERED aspect — never
	// payload-supplied, so a hand request can't be forged on another
	// aspect's behalf. Operator connections never register, so they
	// fall out here too (spawn is an aspect-path-only frame).
	parent := c.registeredAs
	if parent == "" {
		c.respondError(env, "spawn.request requires a registered aspect connection")
		return
	}
	// No sub-of-sub in v1: a hand (derived name) cannot spawn hands.
	if aspects.IsDerivedName(parent) {
		c.respondError(env, "spawn.request: derived identity "+parent+" cannot spawn (no sub-of-sub)")
		return
	}

	var p frames.SpawnRequestPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.respondError(env, "spawn.request payload malformed: "+err.Error())
		return
	}
	if strings.TrimSpace(p.Brief) == "" {
		c.respondError(env, "spawn.request: brief required")
		return
	}
	count := p.Count
	if count == 0 {
		count = 1
	}
	if max := c.broker.spawnMaxPerRequest(); count < 1 || count > max {
		c.respondError(env, fmt.Sprintf("spawn.request: count %d out of range 1..%d", count, max))
		return
	}

	sub, ok := c.broker.runner.(SpawnSubmitter)
	if c.broker.runner == nil || !ok {
		c.respondError(env, "spawn not available: broker has no spawn-capable runner")
		return
	}

	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	handles, err := sub.SubmitSpawn(ctx, parent, p.Brief, count, p.Thread)
	if err != nil {
		c.respondError(env, "spawn failed: "+err.Error())
		return
	}
	out := make([]frames.SpawnHandle, 0, len(handles))
	for _, h := range handles {
		out = append(out, frames.SpawnHandle{RunID: h.RunID, Name: h.Name})
	}
	resp, rerr := frames.NewResponse(frames.KindSpawnResult, env.ID, frames.SpawnResultPayload{Hands: out})
	if rerr != nil {
		c.log.Error("build spawn.result frame failed", "err", rerr)
		return
	}
	c.send(resp)
}
