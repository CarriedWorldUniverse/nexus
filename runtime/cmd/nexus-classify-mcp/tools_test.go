package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

func newTestServer(t *testing.T, step bridlefake.Step) *mcpserver.MCPServer {
	t.Helper()
	prov := bridlefake.NewProvider(step)
	classifier := &classification.PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	srv := mcpserver.NewMCPServer("test-classify", "0.0.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, classifier, nil)
	return srv
}

func callTool(t *testing.T, srv *mcpserver.MCPServer, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	tools := srv.ListTools()
	st, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	req := mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
	res, err := st.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler(%q): %v", name, err)
	}
	return res
}

func TestClassifyPRTriage_Trivial(t *testing.T) {
	srv := newTestServer(t, bridlefake.Step{
		Text: `{"class": "trivial", "reason": "typo fix"}`,
	})

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff": "--- a/README.md\n+++ b/README.md\n-- instalation\n+- installation",
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var verdict map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &verdict); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if verdict["class"] != "trivial" {
		t.Errorf("class = %q, want %q", verdict["class"], "trivial")
	}
	if verdict["reason"] != "typo fix" {
		t.Errorf("reason = %q, want %q", verdict["reason"], "typo fix")
	}
}

func TestClassifyPRTriage_NeedsReview(t *testing.T) {
	srv := newTestServer(t, bridlefake.Step{
		Text: `{"class": "needs-review", "reason": "new auth handler"}`,
	})

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff": "+func NewHandler() { ... }",
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var verdict map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &verdict); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if verdict["class"] != "needs-review" {
		t.Errorf("class = %q, want %q", verdict["class"], "needs-review")
	}
}

func TestClassifyPRTriage_Suspicious(t *testing.T) {
	srv := newTestServer(t, bridlefake.Step{
		Text: `{"class": "suspicious", "reason": "touches auth middleware"}`,
	})

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff": "diff --git a/auth/token.go b/auth/token.go\n+newSecret := os.Getenv(\"SECRET\")",
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var verdict map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &verdict); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if verdict["class"] != "suspicious" {
		t.Errorf("class = %q, want %q", verdict["class"], "suspicious")
	}
}

func TestClassifyPRTriage_EmptyDiff(t *testing.T) {
	srv := newTestServer(t, bridlefake.Step{
		Text: `{"class": "trivial", "reason": "empty"}`,
	})

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff": "",
	})

	if !res.IsError {
		t.Fatal("expected error for empty diff")
	}
	if !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "diff is required") {
		t.Errorf("error = %q, want 'diff is required'", res.Content[0].(mcpgo.TextContent).Text)
	}
}

func TestClassifyPRTriage_ModelOverride(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "trivial", "reason": "test"}`,
	})
	classifier := &classification.PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	srv := mcpserver.NewMCPServer("test-classify", "0.0.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, classifier, nil)

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff":           "test diff",
		"model_override": "gpt-4o-mini",
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var verdict map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &verdict); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if verdict["class"] != "trivial" {
		t.Errorf("class = %q, want %q", verdict["class"], "trivial")
	}
}

func TestClassifyPRTriage_FailOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: "garbage not json",
	})
	classifier := &classification.PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	srv := mcpserver.NewMCPServer("test-classify", "0.0.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, classifier, nil)

	res := callTool(t, srv, "classify.pr_triage", map[string]any{
		"diff": "some diff",
	})

	if res.IsError {
		t.Fatalf("expected fail-open success, got error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var verdict map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &verdict); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if verdict["class"] != "needs-review" {
		t.Errorf("fail-open class = %q, want %q", verdict["class"], "needs-review")
	}
}

func TestMCPServerIdentity(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "trivial", "reason": "test"}`,
	})
	classifier := &classification.PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}
	srv := mcpserver.NewMCPServer("nexus-classify", "0.1.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, classifier, nil)

	tools := srv.ListTools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	st, ok := tools["classify.pr_triage"]
	if !ok {
		t.Fatal("tool 'classify.pr_triage' not found in map")
	}
	if st.Tool.Name != "classify.pr_triage" {
		t.Errorf("tool name = %q, want %q", st.Tool.Name, "classify.pr_triage")
	}
}
