package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/CarriedWorldUniverse/nexus/skills"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, log *slog.Logger) {
	srv.AddTool(mcpgo.NewTool("search_skills",
		mcpgo.WithDescription("Search the nexus dev-lifecycle skill library. Returns matching skills as [{name, description}]. Call get_skill with a name to load the full skill."),
		mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Topic or phase, e.g. 'review', 'security', 'merge'. Empty lists all.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		hits := skills.Search(req.GetString("query", ""))
		out := make([]map[string]string, 0, len(hits))
		for _, s := range hits {
			out = append(out, map[string]string{"name": s.Name, "description": s.Description})
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("get_skill",
		mcpgo.WithDescription("Load the full SKILL.md body for a skill by exact name (from search_skills)."),
		mcpgo.WithString("name", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		name := req.GetString("name", "")
		body, ok := skills.Get(name)
		if !ok {
			return mcpErr("no such skill: " + name), nil
		}
		return mcpgo.NewToolResultText(body), nil
	})
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(msg)
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, _ := json.Marshal(v)
	return mcpgo.NewToolResultText(string(b))
}
