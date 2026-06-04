package main

import (
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

func TestParseMCPProfile(t *testing.T) {
	if cfg, err := parseMCPProfile(""); err != nil || cfg != nil {
		t.Fatalf("empty: cfg=%v err=%v, want nil,nil", cfg, err)
	}
	if cfg, err := parseMCPProfile(`{"mcpServers":{}}`); err != nil || cfg != nil {
		t.Fatalf("no servers: cfg=%v err=%v, want nil,nil", cfg, err)
	}

	cfg, err := parseMCPProfile(`{"mcpServers":{
		"nexus-jira":{"command":"/bin/jira","args":["-keyfile","/k.json"],"env":{"A":"1"}},
		"remote":{"url":"https://h/sse"}
	}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers=%d want 2", len(cfg.Servers))
	}
	// sorted by name: nexus-jira, remote
	j := cfg.Servers[0]
	if j.Name != "nexus-jira" || j.Transport != bridle.MCPTransportStdio {
		t.Fatalf("server0=%+v", j)
	}
	if len(j.Command) != 3 || j.Command[0] != "/bin/jira" || j.Command[1] != "-keyfile" {
		t.Fatalf("jira command=%v", j.Command)
	}
	if j.Env["A"] != "1" {
		t.Fatalf("jira env=%v", j.Env)
	}
	r := cfg.Servers[1]
	if r.Name != "remote" || r.Transport != bridle.MCPTransportHTTPSSE || r.URL != "https://h/sse" {
		t.Fatalf("server1=%+v", r)
	}

	if _, err := parseMCPProfile(`{bad`); err == nil {
		t.Fatal("malformed: want error")
	}
}
