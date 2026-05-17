// MCP tool registration + per-tool handler. Each handler is a thin
// adapter: parse arguments, call the matching jiraClient method,
// shape the result back into a JSON-stringified mcpgo.CallToolResult.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, c *jiraClient, native *nativeClient, log *slog.Logger) {
	type toolDef struct {
		name        string
		description string
		schema      mcpgo.ToolInputSchema
		handler     func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}

	tools := []toolDef{
		{
			name:        "jira.search",
			description: "Generic JQL search. Returns lightweight issue refs (key, summary, status, type, assignee).",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"jql":         map[string]any{"type": "string", "description": "JQL query, e.g. \"project = NEX AND status = 'To Do'\"."},
					"max_results": map[string]any{"type": "integer", "description": "Cap on results (1-100, default 50)."},
				},
				Required: []string{"jql"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				jql := req.GetString("jql", "")
				if jql == "" {
					return mcpErr("jql is required"), nil
				}
				max := int(req.GetFloat("max_results", 0))
				refs, err := c.Search(ctx, jql, max)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(refs), nil
			},
		},
		{
			name:        "jira.get",
			description: "Fetch a single issue by key. Returns full projection: summary, status, type, assignee, description (plain-text from ADF), components, labels, parent.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"key": map[string]any{"type": "string", "description": "Issue key, e.g. \"NEX-15\"."},
				},
				Required: []string{"key"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				key := req.GetString("key", "")
				if key == "" {
					return mcpErr("key is required"), nil
				}
				issue, err := c.Get(ctx, key)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(issue), nil
			},
		},
		{
			name:        "jira.list_my_issues",
			description: "List issues assigned to the authenticated aspect. Optionally filter by status name.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"status":      map[string]any{"type": "string", "description": "Optional status name, e.g. \"In Progress\"."},
					"max_results": map[string]any{"type": "integer", "description": "Cap on results (1-100, default 50)."},
				},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				status := req.GetString("status", "")
				jql := "assignee = currentUser()"
				if c.projectKey != "" {
					jql = "project = " + c.projectKey + " AND " + jql
				}
				if status != "" {
					jql += fmt.Sprintf(" AND status = %q", status)
				}
				jql += " ORDER BY updated DESC"
				max := int(req.GetFloat("max_results", 0))
				refs, err := c.Search(ctx, jql, max)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(refs), nil
			},
		},
		{
			name:        "jira.list_ready",
			description: "List Ready-or-Todo work the aspect could claim. Optionally filter by component or by-aspect (assignee accountId).",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"component":        map[string]any{"type": "string", "description": "Optional component name (e.g. \"nexus\", \"harness\")."},
					"include_assigned": map[string]any{"type": "boolean", "description": "If true, return issues already assigned (default false: unassigned only)."},
					"max_results":      map[string]any{"type": "integer", "description": "Cap on results (1-100, default 50)."},
				},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				comp := req.GetString("component", "")
				includeAssigned := req.GetBool("include_assigned", false)
				jql := "status = \"To Do\""
				if c.projectKey != "" {
					jql = "project = " + c.projectKey + " AND " + jql
				}
				if comp != "" {
					jql += fmt.Sprintf(" AND component = %q", comp)
				}
				if !includeAssigned {
					jql += " AND assignee is EMPTY"
				}
				jql += " ORDER BY priority DESC, created ASC"
				max := int(req.GetFloat("max_results", 0))
				refs, err := c.Search(ctx, jql, max)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(refs), nil
			},
		},
		{
			name:        "jira.claim",
			description: "Atomically claim an issue: set self as assignee and transition to In Progress. Idempotent if already claimed by self.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"key": map[string]any{"type": "string", "description": "Issue key to claim."},
				},
				Required: []string{"key"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				key := req.GetString("key", "")
				if key == "" {
					return mcpErr("key is required"), nil
				}
				accountID, err := c.MyAccountID(ctx)
				if err != nil {
					return mcpErr("auth lookup: " + err.Error()), nil
				}
				if err := c.Assign(ctx, key, accountID); err != nil {
					return mcpErr("assign: " + err.Error()), nil
				}
				if err := c.TransitionTo(ctx, key, "In Progress", ""); err != nil {
					// Already-In-Progress is acceptable; surface error
					// only if the issue isn't actually there.
					issue, gErr := c.Get(ctx, key)
					if gErr == nil && issue.Status == "In Progress" {
						if native != nil && native.enabled() {
							native.MirrorAssign(ctx, key, accountID, native.aspect)
							native.MirrorTransition(ctx, key, "In Progress", native.aspect)
						}
						return mcpJSON(map[string]any{"claimed": key, "assignee_account": accountID, "status": issue.Status, "note": "already In Progress"}), nil
					}
					return mcpErr("transition: " + err.Error()), nil
				}
				if native != nil && native.enabled() {
					native.MirrorAssign(ctx, key, accountID, native.aspect)
					native.MirrorTransition(ctx, key, "In Progress", native.aspect)
				}
				return mcpJSON(map[string]any{"claimed": key, "assignee_account": accountID, "status": "In Progress"}), nil
			},
		},
		{
			name:        "jira.comment",
			description: "Post a plain-text comment on an issue. Newlines split into paragraphs.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"key":  map[string]any{"type": "string"},
					"body": map[string]any{"type": "string", "description": "Comment body (plain text; newlines preserved as paragraph breaks)."},
				},
				Required: []string{"key", "body"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				key := req.GetString("key", "")
				body := req.GetString("body", "")
				if key == "" || body == "" {
					return mcpErr("key and body are required"), nil
				}
				if err := c.Comment(ctx, key, body); err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"commented": key}), nil
			},
		},
		{
			name:        "jira.update_status",
			description: "Transition an issue to a target status by name. Optionally include a comment posted before the transition fires.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"key":     map[string]any{"type": "string"},
					"status":  map[string]any{"type": "string", "description": "Target status name (e.g. \"To Do\", \"In Progress\", \"In Review\", \"Done\")."},
					"comment": map[string]any{"type": "string", "description": "Optional comment to post before transitioning."},
				},
				Required: []string{"key", "status"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				key := req.GetString("key", "")
				status := req.GetString("status", "")
				comment := req.GetString("comment", "")
				if key == "" || status == "" {
					return mcpErr("key and status are required"), nil
				}
				if err := c.TransitionTo(ctx, key, status, comment); err != nil {
					return mcpErr(err.Error()), nil
				}
				if native != nil && native.enabled() {
					native.MirrorTransition(ctx, key, status, native.aspect)
				}
				return mcpJSON(map[string]any{"transitioned": key, "status": status}), nil
			},
		},
		{
			name:        "jira.create",
			description: "Create a new issue (Epic / Story / Task / Subtask / Bug). When parent is set, the new issue is parented to it.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"summary":     map[string]any{"type": "string"},
					"description": map[string]any{"type": "string", "description": "Body markdown. Rendered as an ADF code block."},
					"issue_type":  map[string]any{"type": "string", "description": "One of: Epic, Story, Task, Subtask, Bug."},
					"parent":      map[string]any{"type": "string", "description": "Optional parent issue key."},
					"component":   map[string]any{"type": "string"},
					"labels":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				Required: []string{"summary", "issue_type"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				summary := req.GetString("summary", "")
				desc := req.GetString("description", "")
				issueType := req.GetString("issue_type", "")
				parent := req.GetString("parent", "")
				comp := req.GetString("component", "")
				labels := req.GetStringSlice("labels", nil)
				if summary == "" || issueType == "" {
					return mcpErr("summary and issue_type are required"), nil
				}
				key, err := c.CreateIssue(ctx, summary, desc, issueType, parent, comp, labels)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				if native != nil && native.enabled() {
					body := translateJiraCreate(c.projectKey, issueType, summary, desc, native.aspect, "")
					native.MirrorCreate(ctx, body)
				}
				return mcpJSON(map[string]any{"created": key}), nil
			},
		},
		{
			name:        "jira.complete",
			description: "Finish a claimed issue. Transitions to Done (or In Review when await_review=true), optionally with a final comment.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"key":          map[string]any{"type": "string"},
					"comment":      map[string]any{"type": "string"},
					"await_review": map[string]any{"type": "boolean", "description": "If true, transition to In Review instead of Done (default false)."},
				},
				Required: []string{"key"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				key := req.GetString("key", "")
				comment := req.GetString("comment", "")
				awaitReview := req.GetBool("await_review", false)
				if key == "" {
					return mcpErr("key is required"), nil
				}
				target := "Done"
				if awaitReview {
					target = "In Review"
				}
				if err := c.TransitionTo(ctx, key, target, comment); err != nil {
					return mcpErr(err.Error()), nil
				}
				if native != nil && native.enabled() {
					native.MirrorTransition(ctx, key, target, native.aspect)
				}
				return mcpJSON(map[string]any{"completed": key, "status": target}), nil
			},
		},
	}

	for _, t := range tools {
		tool := mcpgo.Tool{
			Name:        t.name,
			Description: t.description,
			InputSchema: t.schema,
		}
		srv.AddTool(tool, t.handler)
		log.Debug("registered tool", "name", t.name)
	}
}

// mcpJSON wraps a Go value into an MCP CallToolResult with a single
// JSON-serialised text content block. Matches the convention nexus-
// comms-mcp uses so consumers get a uniform shape across both servers.
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

// mcpErr returns an MCP error result with the given message. Tool-
// level errors (bad input, REST failure) come back this way; MCP-
// protocol-level errors return via the handler's Go error path, which
// we never use today (everything surfaces as text).
func mcpErr(msg string) *mcpgo.CallToolResult {
	return &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: msg},
		},
	}
}
