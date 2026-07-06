package main

import (
	"context"
	"encoding/json"
	"log/slog"

	agentskills "github.com/CarriedWorldUniverse/nexus"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// registerTools wires search_skills/get_skill. allow is the role-at-spawn
// SkillAllowlist (M1 Unit 3) scoping both tools to this spawn's role: an
// empty allow list is the back-compat no-op (every skill served, today's
// ungated behavior); a non-empty one filters search_skills' hits and
// denies get_skill for any name outside it — the skill-gating primitive,
// enforced at this MCP surface (agentskills.FilterAllowlist/AllowedName).
func registerTools(srv *mcpserver.MCPServer, log *slog.Logger, allow []string) {
	srv.AddTool(mcpgo.NewTool("search_skills",
		mcpgo.WithDescription("Search the nexus dev-lifecycle skill library. Returns matching skills as [{name, description}]. Call get_skill with a name to load the full skill."),
		mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Topic or phase, e.g. 'review', 'security', 'merge'. Empty lists all.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		hits := agentskills.FilterAllowlist(agentskills.Search(req.GetString("query", "")), allow)
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
		if !agentskills.AllowedName(name, allow) {
			return mcpErr("skill not permitted for this role: " + name), nil
		}
		body, ok := agentskills.Get(name)
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
