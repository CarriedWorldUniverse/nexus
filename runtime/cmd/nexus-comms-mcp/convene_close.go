// convene_close — the facilitator-facing MCP tool that closes a
// roundtable convene (roundtable P3 / NEX-609 follow-through). The
// broker has accepted convene.close on an aspect's authenticated WS
// since P3 landed, but no MCP tool emitted it — so a claude-code
// facilitator could judge convergence and post the CONSENSUS: summary
// yet had no way to transition the convene record off status=open.
//
// The mapping mirrors spawn exactly: an MCP tool call → a correlated
// frame on THIS process's WS connection. The broker authorizes the
// close against the connection's identity (only the convene's
// facilitator, or an operator, may close), so the tool carries no
// "caller" — there is nothing to forge.
//
// Availability mirrors spawn's gate: facilitators are parent aspects
// (a hand never facilitates), and the operator transport closes via
// the operator frame path instead.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// conveneCloseTool is the MCP tool definition. Status is constrained to
// the two terminal states; summary_msg_id is the CONSENSUS: post's chat
// msg id so the record points at the verdict.
func conveneCloseTool() mcpgo.Tool {
	return mcpgo.NewTool("convene_close",
		mcpgo.WithDescription("Close a roundtable convene you are FACILITATING. Call this after judging the participants' lens turns and posting your 'CONSENSUS: …' summary to the convene thread. status=converged when the participants reached agreement (or you synthesised a verdict), status=abandoned when the roundtable cannot conclude. Only the convene's facilitator may close it."),
		mcpgo.WithString("convene_id",
			mcpgo.Required(),
			mcpgo.Description("The convene id from your facilitator brief (cv-…)."),
		),
		mcpgo.WithString("status",
			mcpgo.Required(),
			mcpgo.Description("Terminal status: converged | abandoned."),
		),
		mcpgo.WithNumber("summary_msg_id",
			mcpgo.Description("Chat msg id of your CONSENSUS: summary post, so the record links to the verdict. Optional but strongly preferred for converged closes."),
		),
	)
}

// conveneClosePayloadFromArgs maps the tool call to the wire payload,
// catching the client-side invariants the broker would reject anyway.
func conveneClosePayloadFromArgs(req mcpgo.CallToolRequest) (frames.ConveneClosePayload, error) {
	id := strings.TrimSpace(req.GetString("convene_id", ""))
	if id == "" {
		return frames.ConveneClosePayload{}, fmt.Errorf("convene_id is required")
	}
	status := strings.TrimSpace(req.GetString("status", ""))
	if status != "converged" && status != "abandoned" {
		return frames.ConveneClosePayload{}, fmt.Errorf("status must be converged or abandoned")
	}
	return frames.ConveneClosePayload{
		ConveneID:    id,
		Status:       status,
		SummaryMsgID: int64(req.GetInt("summary_msg_id", 0)),
	}, nil
}

// handleConveneClose translates the MCP call into a convene.close WS
// frame on this connection — correlated, like spawn: the broker answers
// convene.close.result with {ok,status} or a rejection message.
func (b *wsBridge) handleConveneClose(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	payload, err := conveneClosePayloadFromArgs(req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	if r, down := b.brokerDown(); down {
		return r, nil
	}

	env, err := frames.NewRequest(frames.KindConveneClose, payload)
	if err != nil {
		b.log.Warn("convene_close: encode failed", "err", err)
		return mcpgo.NewToolResultError(fmt.Sprintf("encode frame: %v", err)), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := b.ws.Request(reqCtx, env)
	if err != nil {
		b.log.Warn("convene_close: request failed", "err", err, "convene_id", payload.ConveneID)
		return mcpgo.NewToolResultError(fmt.Sprintf("convene.close: %v", err)), nil
	}

	var result frames.ConveneCloseResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("decode result: %v", err)), nil
	}
	if !result.OK {
		return mcpgo.NewToolResultError("convene.close rejected: " + result.Message), nil
	}
	b.log.Debug("convene_close: accepted", "convene_id", payload.ConveneID, "status", result.Status)
	return mcpgo.NewToolResultText(fmt.Sprintf("convene %s closed: %s", payload.ConveneID, result.Status)), nil
}
