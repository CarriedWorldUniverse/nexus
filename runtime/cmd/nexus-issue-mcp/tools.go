package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

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
		mcpgo.WithDescription("Get issue as a markdown document (aspect-facing)."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		body, err := c.getText(ctx, "/api/issues/"+url.PathEscape(key))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(body), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.get_raw",
		mcpgo.WithDescription("Get structured JSON (dashboard/sync use)."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		var out map[string]any
		if err := c.get(ctx, "/api/issues/"+url.PathEscape(key)+"?format=raw", &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.update",
		mcpgo.WithDescription("Patch issue fields. actor required; pass only the fields to change."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
		mcpgo.WithString("summary"),
		mcpgo.WithString("description"),
		mcpgo.WithString("definition_of_done"),
		mcpgo.WithString("priority"),
		mcpgo.WithString("parent_key"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		actor := req.GetString("actor", "")
		if key == "" || actor == "" {
			return mcpErr("key and actor required"), nil
		}
		body := map[string]any{"actor": actor}
		for _, k := range []string{"summary", "description", "definition_of_done", "priority", "parent_key"} {
			if v, ok := req.GetArguments()[k]; ok {
				body[k] = v
			}
		}
		if err := c.patch(ctx, "/api/issues/"+url.PathEscape(key), body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.transition",
		mcpgo.WithDescription("Move issue to a new status."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("status", mcpgo.Required(), mcpgo.Description("Target status, e.g. In Progress, Done.")),
		mcpgo.WithString("actor", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		body := map[string]any{
			"status": req.GetString("status", ""),
			"actor":  req.GetString("actor", ""),
		}
		if err := c.post(ctx, "/api/issues/"+url.PathEscape(key)+"/transition", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.assign",
		mcpgo.WithDescription("Assign issue to an aspect or team. Pass empty strings to unassign."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
		mcpgo.WithString("aspect"),
		mcpgo.WithString("team"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		body := map[string]any{
			"aspect": req.GetString("aspect", ""),
			"team":   req.GetString("team", ""),
			"actor":  req.GetString("actor", ""),
		}
		if err := c.post(ctx, "/api/issues/"+url.PathEscape(key)+"/assign", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.find_by_text",
		mcpgo.WithDescription("Full-text search over issue summary/description/DoD AND comment bodies. Returns issues ranked by FTS5 bm25. Use this when you want to find issues *mentioning* a term — not when you know the exact field to filter on (use issue.search for that). Query syntax: bare words, \"quoted phrases\", AND/OR/NOT operators, prefix-with-* for stems."),
		mcpgo.WithString("q", mcpgo.Required(), mcpgo.Description("FTS5 query string.")),
		mcpgo.WithNumber("limit", mcpgo.Description("Max results, default 50, cap 200.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		q := req.GetString("q", "")
		if q == "" {
			return mcpErr("q required"), nil
		}
		v := url.Values{}
		v.Set("q", q)
		if limit := req.GetInt("limit", 0); limit > 0 {
			v.Set("limit", fmt.Sprintf("%d", limit))
		}
		var out []any
		if err := c.get(ctx, "/api/issues/search/text?"+v.Encode(), &out); err != nil {
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
		if err := c.post(ctx, "/api/issues/"+url.PathEscape(req.GetString("key", ""))+"/comments", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.watch",
		mcpgo.WithDescription("Watch an issue. Idempotent."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("aspect", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		body := map[string]any{
			"aspect": req.GetString("aspect", ""),
			"actor":  req.GetString("actor", ""),
		}
		if err := c.post(ctx, "/api/issues/"+url.PathEscape(req.GetString("key", ""))+"/watchers", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.unwatch",
		mcpgo.WithDescription("Unwatch an issue. No-op if not watching."),
		mcpgo.WithString("key", mcpgo.Required()),
		mcpgo.WithString("aspect", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		body := map[string]any{
			"aspect": req.GetString("aspect", ""),
			"actor":  req.GetString("actor", ""),
		}
		if err := c.del(ctx, "/api/issues/"+url.PathEscape(req.GetString("key", ""))+"/watchers", body, nil); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(map[string]any{"ok": true}), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.list_watchers",
		mcpgo.WithDescription("List aspects watching an issue."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var out []string
		if err := c.get(ctx, "/api/issues/"+url.PathEscape(req.GetString("key", ""))+"/watchers", &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.list_my_updates",
		mcpgo.WithDescription("Pull-mode catch-up: returns events on issues assigned to or watched by the aspect, since an optional ISO 8601 timestamp. LIMIT 200."),
		mcpgo.WithString("aspect", mcpgo.Required()),
		mcpgo.WithString("since", mcpgo.Description("ISO 8601 timestamp; events after this")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		aspect := req.GetString("aspect", "")
		if aspect == "" {
			return mcpErr("aspect required"), nil
		}
		q := url.Values{}
		q.Set("aspect", aspect)
		if since := req.GetString("since", ""); since != "" {
			q.Set("since", since)
		}
		path := "/api/issues/updates?" + q.Encode()
		var out []any
		if err := c.get(ctx, path, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.link",
		mcpgo.WithDescription("Create a typed link from one issue to another. type='blocks' means from_key blocks to_key (the from-side cannot reach Done until to_key is terminal); type='relates-to' is editorial cross-reference only. Idempotent — re-linking the same edge is a no-op."),
		mcpgo.WithString("from_key", mcpgo.Required()),
		mcpgo.WithString("to_key", mcpgo.Required()),
		mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("blocks | relates-to")),
		mcpgo.WithString("actor", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		from := req.GetString("from_key", "")
		if from == "" {
			return mcpErr("from_key required"), nil
		}
		body := map[string]any{
			"to_key": req.GetString("to_key", ""),
			"type":   req.GetString("type", ""),
			"actor":  req.GetString("actor", ""),
		}
		var out map[string]any
		if err := c.post(ctx, "/api/issues/"+url.PathEscape(from)+"/links", body, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.unlink",
		mcpgo.WithDescription("Remove a typed link. Idempotent — removing a non-existent edge returns success."),
		mcpgo.WithString("from_key", mcpgo.Required()),
		mcpgo.WithString("to_key", mcpgo.Required()),
		mcpgo.WithString("type", mcpgo.Required()),
		mcpgo.WithString("actor", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		from := req.GetString("from_key", "")
		if from == "" {
			return mcpErr("from_key required"), nil
		}
		body := map[string]any{
			"to_key": req.GetString("to_key", ""),
			"type":   req.GetString("type", ""),
			"actor":  req.GetString("actor", ""),
		}
		var out map[string]any
		if err := c.del(ctx, "/api/issues/"+url.PathEscape(from)+"/links", body, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.list_links",
		mcpgo.WithDescription("List every link touching an issue, in both directions. Each entry carries from_key, to_key, type, direction ('outgoing'|'incoming'), created_at, created_by."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		var out map[string]any
		if err := c.get(ctx, "/api/issues/"+url.PathEscape(key)+"/links", &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.list_projects",
		mcpgo.WithDescription("List projects (the keyspace aspects can create issues against — NEX, WAKE, OSS, ...). Org-scoped by the calling token; archived projects excluded unless include_archived=true."),
		mcpgo.WithBoolean("include_archived"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		path := "/api/projects"
		if req.GetBool("include_archived", false) {
			path += "?include_archived=true"
		}
		var out []any
		if err := c.get(ctx, path, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(msg)
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, _ := json.Marshal(v)
	return mcpgo.NewToolResultText(string(b))
}
