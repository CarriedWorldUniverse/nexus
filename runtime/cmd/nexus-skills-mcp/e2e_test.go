package main

import (
	"context"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestEndToEnd(t *testing.T) {
	srv := mcpserver.NewMCPServer("nexus-skills", "test", mcpserver.WithToolCapabilities(true))
	registerTools(srv, nil, nil)

	c, err := mcpclient.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "1.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "search_skills", Arguments: map[string]any{"query": "ticket"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("search_skills: err=%v res=%+v", err, res)
	}
	if txt := toolText(res); !strings.Contains(txt, "development") {
		t.Fatalf("search result missing development: %s", txt)
	}

	res, err = c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "get_skill", Arguments: map[string]any{"name": "development"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_skill: err=%v res=%+v", err, res)
	}
	if txt := toolText(res); !strings.Contains(txt, "# development") {
		t.Fatalf("get_skill body wrong: %s", txt)
	}
}

// TestEndToEndSkillGating verifies the role-at-spawn skill-gating
// primitive (M1 Unit 3) at the actual MCP surface: a non-empty allow list
// scopes search_skills' hits and denies get_skill for anything outside it.
func TestEndToEndSkillGating(t *testing.T) {
	srv := mcpserver.NewMCPServer("nexus-skills", "test", mcpserver.WithToolCapabilities(true))
	registerTools(srv, nil, []string{"development"})

	c, err := mcpclient.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "1.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// search_skills is scoped to the allowlist — "security" must not appear.
	res, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "search_skills", Arguments: map[string]any{"query": ""}},
	})
	if err != nil || res.IsError {
		t.Fatalf("search_skills: err=%v res=%+v", err, res)
	}
	txt := toolText(res)
	if !strings.Contains(txt, "development") {
		t.Fatalf("search_skills should still return the allowed skill: %s", txt)
	}
	if strings.Contains(txt, "security") {
		t.Fatalf("search_skills leaked a non-allowlisted skill: %s", txt)
	}

	// get_skill for the allowed name succeeds.
	res, err = c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "get_skill", Arguments: map[string]any{"name": "development"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_skill(development) should succeed under the allowlist: err=%v res=%+v", err, res)
	}

	// get_skill for a real but non-allowlisted name is denied.
	res, err = c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "get_skill", Arguments: map[string]any{"name": "security"}},
	})
	if err != nil {
		t.Fatalf("get_skill(security) transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("get_skill(security) should be denied outside the allowlist, got %+v", res)
	}
}

// TestSkillAllowlistFrom is a table test of the flag/env precedence for
// the skill-gating allow list.
func TestSkillAllowlistFrom(t *testing.T) {
	tests := []struct {
		name    string
		flagVal string
		envVal  string
		want    []string
	}{
		{name: "both empty yields nil (all skills)", flagVal: "", envVal: "", want: nil},
		{name: "flag wins over env", flagVal: "development,review", envVal: "security", want: []string{"development", "review"}},
		{name: "env used when flag empty", flagVal: "", envVal: "security,bash", want: []string{"security", "bash"}},
		{name: "blank entries dropped", flagVal: "development, ,review", envVal: "", want: []string{"development", "review"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := skillAllowlistFrom(tc.flagVal, tc.envVal)
			if len(got) != len(tc.want) {
				t.Fatalf("skillAllowlistFrom(%q, %q) = %v, want %v", tc.flagVal, tc.envVal, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("skillAllowlistFrom(%q, %q)[%d] = %q, want %q", tc.flagVal, tc.envVal, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func toolText(r *mcpgo.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
