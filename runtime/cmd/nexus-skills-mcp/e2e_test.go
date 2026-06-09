package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestEndToEnd(t *testing.T) {
	srv := mcpserver.NewMCPServer("nexus-skills", "test", mcpserver.WithToolCapabilities(true))
	registerTools(srv, nil)

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

func TestStdioJSONRPCSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_skills","arguments":{"query":"ticket"}}}`,
	}, "\n") + "\n"

	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stdio smoke failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "development") {
		t.Fatalf("stdio smoke missing development: %s", out)
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
