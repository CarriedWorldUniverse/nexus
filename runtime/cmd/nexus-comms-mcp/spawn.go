// spawn — the aspect-facing MCP tool that fans a unit of work to a
// fresh-context hand of the SAME aspect (roundtable P2 / NEX-571,
// NEX-601). It is the last piece making hands agent-triggerable: the
// broker has accepted spawn.request on an aspect's authenticated WS
// since feat/aspect-hands, but until now no MCP tool emitted it, so an
// aspect's claude-code had no way to fan out.
//
// The mapping mirrors send_chat exactly: an MCP tool call → a frame on
// THIS process's WS connection. Because the connection is the aspect's
// authenticated identity (keyfile/JWT-bound), the spawn inherits that
// identity — the broker binds the derived hand identity to the
// connection's registered aspect and never trusts a payload-supplied
// parent. So the tool carries no "from": there is nothing to forge.
//
// Gating decision (NEX-601):
//   - The broker is the authority. It rejects spawn from operator
//     connections, enforces no-sub-of-sub (a derived hand cannot
//     spawn), binds identity to connection auth, and caps Count by
//     SpawnMaxPerRequest. The tool passes Count straight through for
//     the broker to enforce the ceiling.
//   - At THIS (MCP) layer we apply one matching gate: the spawn tool
//     is only materialised for a NON-derived aspect identity. A hand's
//     comms-mcp is bound to a derived name, so it simply never sees a
//     spawn tool (no-sub-of-sub at the surface), and the operator
//     transport (defaultFrom == "operator", never registers) doesn't
//     either. This is defence-in-depth, not the sole gate.
//
// There is no per-aspect spawn allow-list: the broker opens spawn to
// every registered (non-derived) aspect, so the tool surface matches
// that — present for all parent aspects, absent for hands/operator.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// spawnToolAvailable reports whether the spawn tool should be
// materialised for a given bound identity. Spawn is for PARENT aspects
// only: derived hand identities are excluded (no-sub-of-sub) and the
// operator transport (which never registers an aspect) is excluded too
// — operator connections can't spawn at the broker either.
func spawnToolAvailable(boundIdentity string) bool {
	id := strings.TrimSpace(boundIdentity)
	if id == "" || id == "operator" {
		return false
	}
	return !aspects.IsDerivedName(id)
}

// spawnTool is the MCP tool definition. Schema per NEX-601: brief
// (required), count (optional, default 1, capped by the broker's
// SpawnMaxPerRequest), thread (optional audit thread).
func spawnTool() mcpgo.Tool {
	return mcpgo.NewTool("spawn",
		mcpgo.WithDescription("Fan a unit of work to a fresh-context hand carrying your own persona under a derived identity. The hand boots clean (no current conversation), does the work described in brief, and reports back into the audit thread — you keep running, never blocked. Use for parallel background work you'd otherwise have to do inline (research sweeps, multi-file edits, independent sub-tasks). The hand inherits your identity and scope; it cannot itself spawn (no sub-of-sub). Returns one handle (name + run id) per hand once accepted."),
		mcpgo.WithString("brief",
			mcpgo.Required(),
			mcpgo.Description("The work/persona instruction for the hand — a self-contained statement of what to do. The hand has no access to your current context, so include everything it needs."),
		),
		mcpgo.WithNumber("count",
			mcpgo.Description("How many hands to fan this brief to. Default 1. Capped by the broker's SpawnMaxPerRequest; asking for more is rejected."),
		),
		mcpgo.WithString("thread",
			mcpgo.Description("Audit thread (topic) the hands report their briefs and results into. Defaults to a fresh thread rooted by the broker under your identity."),
		),
	)
}

// spawnRequestFromArgs maps a spawn tool call to a SpawnRequestPayload.
// Count defaults to 1 when omitted (count==0); any explicit value is
// passed through untouched so the broker enforces the ceiling (a single
// authority for the cap, no client/broker drift). Returns an error only
// for the one client-side invariant worth catching early: an empty
// brief, which the broker would reject anyway.
func spawnRequestFromArgs(req mcpgo.CallToolRequest) (frames.SpawnRequestPayload, error) {
	brief := strings.TrimSpace(req.GetString("brief", ""))
	if brief == "" {
		return frames.SpawnRequestPayload{}, fmt.Errorf("brief is required and must be non-empty")
	}
	count := req.GetInt("count", 0)
	if count == 0 {
		count = 1
	}
	thread := strings.TrimSpace(req.GetString("thread", ""))
	return frames.SpawnRequestPayload{
		Brief:  brief,
		Count:  count,
		Thread: thread,
	}, nil
}

// handleSpawn translates an MCP spawn tool call into a spawn.request WS
// frame on this connection, mirroring send_chat's emit path so the
// spawn inherits the aspect's authenticated identity. Unlike send_chat
// (fire-and-forget), spawn is a correlated Request: the broker answers
// spawn.result with one handle per hand, or spawn.request.error for a
// whole-request rejection (bad count, no-sub-of-sub, no runner, …).
func (b *wsBridge) handleSpawn(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	payload, err := spawnRequestFromArgs(req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	if r, down := b.brokerDown(); down {
		return r, nil
	}

	env, err := frames.NewRequest(frames.KindSpawnRequest, payload)
	if err != nil {
		b.log.Warn("spawn: encode failed", "err", err)
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := b.ws.Request(reqCtx, env)
	if err != nil {
		b.log.Warn("spawn: request failed", "err", err, "count", payload.Count)
		return mcpgo.NewToolResultError(fmt.Sprintf("spawn.request: %v", err)), nil
	}

	// Whole-request rejection comes back as spawn.request.error with
	// {"error": "..."}. Surface to the model as a tool error so it can
	// adjust (lower count, drop the attempt if it's a hand, …).
	if string(resp.Kind) == string(frames.KindSpawnRequest)+".error" {
		var errPayload map[string]string
		_ = json.Unmarshal(resp.Payload, &errPayload)
		return mcpgo.NewToolResultError(fmt.Sprintf("spawn.request: %s", errPayload["error"])), nil
	}

	var result frames.SpawnResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("decode result: %v", err)), nil
	}
	b.log.Debug("spawn: accepted", "count", payload.Count, "hands", len(result.Hands), "thread", payload.Thread)
	return mcpgo.NewToolResultText(formatSpawnResult(result)), nil
}

// formatSpawnResult renders the per-hand handles one per line. Each
// line carries the hand's derived name, its run id (empty == queued for
// capacity), and any per-hand launch error — so the model can report
// what it fanned out and follow up in the audit thread.
func formatSpawnResult(result frames.SpawnResultPayload) string {
	if len(result.Hands) == 0 {
		return "spawn accepted but no hands returned"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "spawned %d hand(s):\n", len(result.Hands))
	for _, h := range result.Hands {
		switch {
		case h.Error != "":
			fmt.Fprintf(&sb, "- %s: failed to launch — %s\n", h.Name, h.Error)
		case h.RunID == "":
			fmt.Fprintf(&sb, "- %s: queued (waiting on capacity)\n", h.Name)
		default:
			fmt.Fprintf(&sb, "- %s: running (run %s)\n", h.Name, h.RunID)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
