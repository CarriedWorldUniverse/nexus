package main

import (
	"context"
	"encoding/json"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, c *client, log *slog.Logger) {
	srv.AddTool(mcpgo.NewTool("issue.create",
		mcpgo.WithDescription("Create an issue. Required: project, type, summary, definition_of_done, reporter."),
		mcpgo.WithString("project", mcpgo.Required()),
		mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("Epic|Story|Task|Subtask|Bug")),
		mcpgo.WithString("summary", mcpgo.Required()),
		mcpgo.WithString("definition_of_done", mcpgo.Required(), mcpgo.Description("Markdown checklist; at least one item.")),
		mcpgo.WithString("reporter", mcpgo.Required()),
		mcpgo.WithString("description"),
		mcpgo.WithString("priority"),
		mcpgo.WithString("parent_key"),
		mcpgo.WithString("assignee_aspect"),
		mcpgo.WithString("assignee_team"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		body := map[string]any{
			"project":            req.GetString("project", ""),
			"type":               req.GetString("type", ""),
			"summary":            req.GetString("summary", ""),
			"definition_of_done": req.GetString("definition_of_done", ""),
			"reporter":           req.GetString("reporter", ""),
		}
		for _, k := range []string{"description", "priority", "parent_key", "assignee_aspect", "assignee_team"} {
			if v := req.GetString(k, ""); v != "" {
				body[k] = v
			}
		}
		var out map[string]any
		if err := c.post(ctx, "/api/issues", body, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.get",
		mcpgo.WithDescription("Get an issue by key (resolves aliases)."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		var out map[string]any
		if err := c.get(ctx, "/api/issues/"+key, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.search",
		mcpgo.WithDescription("Structured filter search."),
		mcpgo.WithObject("filter", mcpgo.Required(), mcpgo.Description("SearchFilter shape; see spec.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		f := req.GetArguments()["filter"]
		var out []any
		if err := c.post(ctx, "/api/issues/search", f, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.comment",
		mcpgo.WithDescription("Append an immutable comment."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
		mcpgo.WithString("body", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		body := map[string]any{
			"actor": req.GetString("actor", ""),
			"body":  req.GetString("body", ""),
		}
		if err := c.post(ctx, "/api/issues/"+req.GetString("key", "")+"/comments", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(msg)
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, _ := json.Marshal(v)
	return mcpgo.NewToolResultText(string(b))
}
