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
// in a goroutine — same pattern as dispatch/turn/CWB. The connection
// identity is captured HERE, on the read loop: registeredAs is written
// by the register/deregister handlers on this same goroutine, so
// reading it inside the spawned goroutine would race with a concurrent
// re-register. c.auth is immutable after accept; captured alongside
// for the same single-snapshot discipline.
func (c *wsConn) handleSpawnRequestFrame(env frames.Envelope) {
	parent := c.registeredAs
	auth := c.auth
	// Comms-sidecar path (NEX-609): inside a pod aspect, agentfunnel
	// owns the one-session-per-name registration slot, so the aspect's
	// nexus-comms-mcp connects authenticated-but-unregistered
	// (-register=false). The connection's JWT-verified identity
	// (auth.AgentID = the session JWT's sub) vouches for the parent
	// exactly as a registration would — it is the same credential the
	// register path binds. Gated on !Admin: aspect session JWTs are the
	// only non-admin identities (tryVerifyAspectJWT), so the operator
	// JWT (AgentID "operator", Admin) and the legacy master/Frame token
	// (Admin) keep falling out at executeSpawn's parent=="" guard.
	if parent == "" && !auth.Admin && auth.AgentID != "" && auth.AgentID != "operator" {
		parent = auth.AgentID
	}
	go c.executeSpawn(env, parent, auth)
}

func (c *wsConn) executeSpawn(env frames.Envelope, parent string, auth TokenInfo) {
	// The parent identity is the connection's REGISTERED aspect, or —
	// for an authenticated-but-unregistered comms sidecar — the
	// connection's JWT-verified identity (resolved by the read-loop
	// handler above). Never payload-supplied, so a hand request can't
	// be forged on another aspect's behalf. Operator connections
	// resolve to neither, so they fall out here (spawn is an
	// aspect-path-only frame).
	if parent == "" {
		c.respondError(env, "spawn.request requires an aspect-authenticated connection")
		return
	}
	// Bind the registered name to the connection's AUTHENTICATED
	// identity: a connection whose bearer resolved to aspect X must not
	// spawn hands as a differently-registered aspect Y. Admin
	// connections (legacy master / Frame, operator bypass) are exempt —
	// the same carve-out the deregister and dispatch handlers apply.
	// An empty AgentID (no resolved identity on this connection) keeps
	// working, registration alone vouches for it.
	if !auth.Admin && auth.AgentID != "" && auth.AgentID != parent {
		c.respondError(env, "spawn identity mismatch: connection authenticated as "+
			auth.AgentID+" but registered as "+parent)
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
		out = append(out, frames.SpawnHandle{RunID: h.RunID, Name: h.Name, Error: h.Error})
	}
	resp, rerr := frames.NewResponse(frames.KindSpawnResult, env.ID, frames.SpawnResultPayload{Hands: out})
	if rerr != nil {
		c.log.Error("build spawn.result frame failed", "err", rerr)
		return
	}
	c.send(resp)
}
