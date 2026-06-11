// NEX-609: the comms MCP server must never reach the bridle tool loop
// (the native comms surface already carries those tool names → bridle
// ErrToolNameCollision aborts every turn), and the spawn tool def is
// appended for parent identities only.

package main

import (
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

func TestDropCommsMCPServers(t *testing.T) {
	cfg := &bridle.MCPClientConfig{Servers: []bridle.MCPServerSpec{
		{Name: "nexus-jira", Transport: bridle.MCPTransportStdio, Command: []string{"/usr/local/bin/nexus-jira-mcp"}},
		{Name: "nexus-comms-mcp", Transport: bridle.MCPTransportStdio, Command: []string{"/usr/local/bin/nexus-comms-mcp"}},
		{Name: "nexus-comms", Transport: bridle.MCPTransportStdio, Command: []string{"/usr/local/bin/nexus-comms-mcp"}},
	}}
	got := dropCommsMCPServers(cfg, nil)
	if len(got.Servers) != 1 || got.Servers[0].Name != "nexus-jira" {
		t.Fatalf("servers = %+v, want only nexus-jira", got.Servers)
	}

	// nil / empty pass through.
	if dropCommsMCPServers(nil, nil) != nil {
		t.Fatal("nil config must stay nil")
	}
	empty := &bridle.MCPClientConfig{}
	if got := dropCommsMCPServers(empty, nil); len(got.Servers) != 0 {
		t.Fatalf("empty config grew servers: %+v", got.Servers)
	}
}

func TestToolsForProviderAgentSpawnGating(t *testing.T) {
	hasSpawn := func(defs []bridle.ToolDef) bool {
		for _, d := range defs {
			if d.Name == funnel.ToolNameSpawn {
				return true
			}
		}
		return false
	}

	if !hasSpawn(toolsForProviderAgent("ollama", "harrow")) {
		t.Error("parent aspect on a native provider must get the spawn tool")
	}
	if hasSpawn(toolsForProviderAgent("ollama", "harrow.tine")) {
		t.Error("derived (hand) identity must not get the spawn tool (no sub-of-sub)")
	}
	if defs := toolsForProviderAgent("claude-code", "harrow"); defs != nil {
		t.Errorf("claude-code keeps the empty explicit tool surface, got %d defs", len(defs))
	}
}
