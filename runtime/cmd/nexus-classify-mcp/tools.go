package main

import (
	"context"
	"encoding/json"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

func registerTools(srv *mcpserver.MCPServer, c *classification.PRTriage, log *slog.Logger) {
	tool := mcpgo.Tool{
		Name:        "classify.pr_triage",
		Description: "Classify a git diff as trivial, needs-review, or suspicious. Fails open (returns needs-review) on any error.",
		InputSchema: mcpgo.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"diff": map[string]any{
					"type":        "string",
					"description": "Unified git diff text (truncated to 4000 chars if longer)",
				},
				"model_override": map[string]any{
					"type":        "string",
					"description": "Per-call model override; empty = use NEXUS_PR_TRIAGE_MODEL env var or default (deepseek-chat)",
				},
			},
			Required: []string{"diff"},
		},
	}

	srv.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		diff := req.GetString("diff", "")
		if diff == "" {
			return mcpErr("diff is required"), nil
		}

		verdict, err := c.Classify(ctx, classification.PRTriageInput{
			Diff:          diff,
			ModelOverride: req.GetString("model_override", ""),
		})
		if err != nil {
			return mcpErr(err.Error()), nil
		}

		return mcpJSON(map[string]string{
			"class":  string(verdict.Class),
			"reason": verdict.Reason,
		}), nil
	})

	if log != nil {
		log.Debug("registered tool", "name", "classify.pr_triage")
	}
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcpErr("internal: encode result: " + err.Error())
	}
	return &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: string(buf)},
		},
	}
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: msg},
		},
	}
}
