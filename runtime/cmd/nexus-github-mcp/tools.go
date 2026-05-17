// MCP tool registration. Each tool is a thin adapter that maps the
// MCP CallToolRequest arguments to a client method call and returns
// the result as JSON text.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, c *client, log *slog.Logger) {
	srv.AddTool(mcpgo.NewTool("github.pr_create",
		mcpgo.WithDescription("Open a new pull request."),
		mcpgo.WithString("repo", mcpgo.Required(), mcpgo.Description("Repository as \"owner/name\".")),
		mcpgo.WithString("title", mcpgo.Required()),
		mcpgo.WithString("body"),
		mcpgo.WithString("head", mcpgo.Required(), mcpgo.Description("Source branch.")),
		mcpgo.WithString("base", mcpgo.Required(), mcpgo.Description("Target branch (e.g. \"main\").")),
		mcpgo.WithBoolean("draft", mcpgo.Description("Open as draft.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		in := PRCreateInput{
			Owner: owner, Repo: repo,
			Title: req.GetString("title", ""),
			Body:  req.GetString("body", ""),
			Head:  req.GetString("head", ""),
			Base:  req.GetString("base", ""),
			Draft: req.GetBool("draft", false),
		}
		out, err := c.CreatePR(ctx, in)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("github.pr_view",
		mcpgo.WithDescription("Fetch a pull request by number."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("number", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		n := int(req.GetFloat("number", 0))
		if n <= 0 {
			return mcpErr("number must be positive"), nil
		}
		pr, err := c.GetPR(ctx, owner, repo, n)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(pr), nil
	})

	srv.AddTool(mcpgo.NewTool("github.pr_list",
		mcpgo.WithDescription("List pull requests."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithString("state", mcpgo.Description("open | closed | all (default: open)")),
		mcpgo.WithString("head"),
		mcpgo.WithString("base"),
		mcpgo.WithNumber("limit", mcpgo.Description("Cap on results (1-100, default 30).")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		in := ListPRsInput{
			Owner: owner, Repo: repo,
			State: req.GetString("state", ""),
			Head:  req.GetString("head", ""),
			Base:  req.GetString("base", ""),
			Limit: int(req.GetFloat("limit", 0)),
		}
		prs, err := c.ListPRs(ctx, in)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(prs), nil
	})

	srv.AddTool(mcpgo.NewTool("github.pr_merge",
		mcpgo.WithDescription("Merge a pull request (squash by default)."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("number", mcpgo.Required()),
		mcpgo.WithString("merge_method", mcpgo.Description("merge | squash | rebase (default squash)")),
		mcpgo.WithString("commit_title"),
		mcpgo.WithString("commit_message"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		n := int(req.GetFloat("number", 0))
		if n <= 0 {
			return mcpErr("number must be positive"), nil
		}
		raw, err := c.MergePR(ctx, MergePRInput{
			Owner: owner, Repo: repo, Number: n,
			MergeMethod:   req.GetString("merge_method", ""),
			CommitTitle:   req.GetString("commit_title", ""),
			CommitMessage: req.GetString("commit_message", ""),
		})
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(raw)), nil
	})

	srv.AddTool(mcpgo.NewTool("github.pr_checks",
		mcpgo.WithDescription("List check-run results for a PR's head commit."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("number", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		n := int(req.GetFloat("number", 0))
		raw, err := c.PRChecks(ctx, owner, repo, n)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(raw)), nil
	})

	srv.AddTool(mcpgo.NewTool("github.pr_diff",
		mcpgo.WithDescription("Fetch the unified diff of a PR."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("number", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		n := int(req.GetFloat("number", 0))
		diff, err := c.PRDiff(ctx, owner, repo, n)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(diff), nil
	})

	srv.AddTool(mcpgo.NewTool("github.issue_create",
		mcpgo.WithDescription("Open a new issue."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithString("title", mcpgo.Required()),
		mcpgo.WithString("body"),
		mcpgo.WithString("labels", mcpgo.Description("Comma-separated label names.")),
		mcpgo.WithString("assignees", mcpgo.Description("Comma-separated GitHub usernames.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		in := IssueCreateInput{
			Owner: owner, Repo: repo,
			Title:     req.GetString("title", ""),
			Body:      req.GetString("body", ""),
			Labels:    splitCSV(req.GetString("labels", "")),
			Assignees: splitCSV(req.GetString("assignees", "")),
		}
		out, err := c.CreateIssueOnRepo(ctx, in)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("github.issue_view",
		mcpgo.WithDescription("Fetch an issue by number."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("number", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		n := int(req.GetFloat("number", 0))
		i, err := c.GetIssue(ctx, owner, repo, n)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(i), nil
	})

	srv.AddTool(mcpgo.NewTool("github.issue_list",
		mcpgo.WithDescription("List issues in a repo (excludes PRs)."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithString("state", mcpgo.Description("open | closed | all")),
		mcpgo.WithString("labels", mcpgo.Description("Comma-separated label names.")),
		mcpgo.WithNumber("limit"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		in := ListIssuesInput{
			Owner: owner, Repo: repo,
			State:  req.GetString("state", ""),
			Labels: req.GetString("labels", ""),
			Limit:  int(req.GetFloat("limit", 0)),
		}
		issues, err := c.ListIssues(ctx, in)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(issues), nil
	})

	srv.AddTool(mcpgo.NewTool("github.run_view",
		mcpgo.WithDescription("Fetch a GitHub Actions workflow run by ID."),
		mcpgo.WithString("repo", mcpgo.Required()),
		mcpgo.WithNumber("run_id", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		owner, repo, err := splitRepo(req.GetString("repo", ""))
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		runID := int64(req.GetFloat("run_id", 0))
		if runID <= 0 {
			return mcpErr("run_id must be positive"), nil
		}
		raw, err := c.GetWorkflowRun(ctx, owner, repo, runID)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(raw)), nil
	})

	srv.AddTool(mcpgo.NewTool("github.whoami",
		mcpgo.WithDescription("Return the GitHub identity this MCP is authenticated as."),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		u, err := c.WhoAmI(ctx)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(u), nil
	})

	srv.AddTool(mcpgo.NewTool("github.api",
		mcpgo.WithDescription("Generic GitHub REST call. method + path + optional body."),
		mcpgo.WithString("method", mcpgo.Required(), mcpgo.Description("GET | POST | PUT | PATCH | DELETE")),
		mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Path under api.github.com, e.g. \"/repos/owner/name/releases\".")),
		mcpgo.WithString("body", mcpgo.Description("Optional JSON body (string).")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		method := strings.ToUpper(req.GetString("method", ""))
		path := req.GetString("path", "")
		bodyStr := req.GetString("body", "")
		if method == "" || path == "" {
			return mcpErr("method and path required"), nil
		}
		var body any
		if bodyStr != "" {
			var parsed any
			if err := json.Unmarshal([]byte(bodyStr), &parsed); err != nil {
				return mcpErr("body must be valid JSON: " + err.Error()), nil
			}
			body = parsed
		}
		raw, err := c.do(ctx, method, path, body, nil)
		if err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(raw)), nil
	})
}

func splitRepo(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("repo required (expected \"owner/name\")")
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repo %q malformed (expected \"owner/name\")", s)
	}
	return parts[0], parts[1], nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(msg)
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpErr("marshal result: " + err.Error())
	}
	return mcpgo.NewToolResultText(string(b))
}
