package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// mcpServersDoc is the on-the-wire {"mcpServers": {...}} shape stored in an
// aspect's mcp_profile and returned (credential-substituted) by the
// validate endpoint.
type mcpServersDoc struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

type mcpServerEntry struct {
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Transport string            `json:"transport,omitempty"` // optional; inferred from url when absent
}

// parseMCPProfile turns the validate response's mcp_profile blob into a
// bridle MCPClientConfig so NON-claude-code providers (openai, codex, …)
// receive the servers in their TurnRequest.MCP — the funnel only ever set
// an empty config, leaving those providers with no MCP tools. claude-code
// ignores this and discovers its servers from the materialised .mcp.json
// (NEX-170) instead.
//
// Returns (nil, nil) for an empty/serverless profile so the caller can keep
// MCP non-nil-but-empty (which is what claude-code's .mcp.json path wants).
// Server order is sorted by name for deterministic provider args.
func parseMCPProfile(profile string) (*bridle.MCPClientConfig, error) {
	if strings.TrimSpace(profile) == "" {
		return nil, nil
	}
	var doc mcpServersDoc
	if err := json.Unmarshal([]byte(profile), &doc); err != nil {
		return nil, fmt.Errorf("parse mcp_profile: %w", err)
	}
	if len(doc.MCPServers) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(doc.MCPServers))
	for n := range doc.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)

	cfg := &bridle.MCPClientConfig{}
	for _, name := range names {
		e := doc.MCPServers[name]
		spec := bridle.MCPServerSpec{Name: name, Env: e.Env, URL: e.URL}
		if e.URL != "" || e.Transport == string(bridle.MCPTransportHTTPSSE) {
			spec.Transport = bridle.MCPTransportHTTPSSE
		} else {
			spec.Transport = bridle.MCPTransportStdio
			spec.Command = append([]string{e.Command}, e.Args...)
		}
		cfg.Servers = append(cfg.Servers, spec)
	}
	return cfg, nil
}
