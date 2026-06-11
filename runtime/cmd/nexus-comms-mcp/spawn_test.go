// spawn tool tests (NEX-601). These are unit-level — they exercise the
// tool→frame mapping, the count defaulting, and the materialisation
// gate without standing up a live broker WS (the broker's own
// spawn_test.go covers the wire round-trip + ceiling enforcement).

package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// newSpawnCall builds an MCP tool call request with the given args,
// matching the shape claude-code would send.
func newSpawnCall(args map[string]any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Name = "spawn"
	req.Params.Arguments = args
	return req
}

// TestSpawnRequestFromArgs_MapsFields asserts brief/count/thread map
// straight onto SpawnRequestPayload.
func TestSpawnRequestFromArgs_MapsFields(t *testing.T) {
	p, err := spawnRequestFromArgs(newSpawnCall(map[string]any{
		"brief":  "survey the auth packages",
		"count":  float64(3), // JSON numbers decode as float64
		"thread": "NEX-601",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Brief != "survey the auth packages" {
		t.Errorf("Brief = %q, want %q", p.Brief, "survey the auth packages")
	}
	if p.Count != 3 {
		t.Errorf("Count = %d, want 3", p.Count)
	}
	if p.Thread != "NEX-601" {
		t.Errorf("Thread = %q, want %q", p.Thread, "NEX-601")
	}
}

// TestSpawnRequestFromArgs_CountDefaultsToOne asserts an omitted count
// becomes 1 (the broker's own default, mirrored client-side so the
// emitted frame is explicit).
func TestSpawnRequestFromArgs_CountDefaultsToOne(t *testing.T) {
	p, err := spawnRequestFromArgs(newSpawnCall(map[string]any{
		"brief": "do one thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Count != 1 {
		t.Errorf("Count = %d, want 1 (default)", p.Count)
	}
	if p.Thread != "" {
		t.Errorf("Thread = %q, want empty (fresh thread)", p.Thread)
	}
}

// TestSpawnRequestFromArgs_CountPassedThrough asserts an over-cap count
// is NOT clamped client-side — it's passed through so the broker is the
// single authority on the ceiling (no client/broker drift).
func TestSpawnRequestFromArgs_CountPassedThrough(t *testing.T) {
	p, err := spawnRequestFromArgs(newSpawnCall(map[string]any{
		"brief": "fan wide",
		"count": float64(99),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Count != 99 {
		t.Errorf("Count = %d, want 99 (pass-through for broker to reject)", p.Count)
	}
}

// TestSpawnRequestFromArgs_EmptyBriefRejected asserts the one
// client-side invariant: a blank brief errors before any WS write.
func TestSpawnRequestFromArgs_EmptyBriefRejected(t *testing.T) {
	for _, brief := range []string{"", "   ", "\t\n"} {
		if _, err := spawnRequestFromArgs(newSpawnCall(map[string]any{"brief": brief})); err == nil {
			t.Errorf("brief %q: expected error, got nil", brief)
		}
	}
}

// TestSpawnRequestPayload_FrameEncoding asserts the payload encodes to a
// spawn.request frame with the expected JSON field names — the wire
// contract the broker decodes against.
func TestSpawnRequestPayload_FrameEncoding(t *testing.T) {
	p, err := spawnRequestFromArgs(newSpawnCall(map[string]any{
		"brief":  "encode me",
		"count":  float64(2),
		"thread": "T-1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env, err := frames.NewRequest(frames.KindSpawnRequest, p)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if env.Kind != frames.KindSpawnRequest {
		t.Errorf("Kind = %q, want %q", env.Kind, frames.KindSpawnRequest)
	}
	if env.ID == "" {
		t.Error("spawn.request must be a correlated Request (non-empty ID)")
	}
	var got map[string]any
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["brief"] != "encode me" {
		t.Errorf("payload brief = %v, want %q", got["brief"], "encode me")
	}
	if got["count"] != float64(2) {
		t.Errorf("payload count = %v, want 2", got["count"])
	}
	if got["thread"] != "T-1" {
		t.Errorf("payload thread = %v, want %q", got["thread"], "T-1")
	}
}

// TestSpawnToolAvailable_GatesDerivedAndOperator pins the
// materialisation gate: parent aspects get the tool; hands (derived
// names) and the operator transport do not (no-sub-of-sub at the MCP
// surface).
func TestSpawnToolAvailable_GatesDerivedAndOperator(t *testing.T) {
	cases := []struct {
		identity string
		want     bool
	}{
		{"shadow", true},
		{"maren-art", true}, // hyphen is an aspect name, not a hand
		{"plumb", true},
		{"shadow.umbra", false}, // derived hand — no sub-of-sub
		{"plumb.bob", false},
		{"anvil.hand-1", false}, // overflow naming is still derived
		{"operator", false},     // operator transport never spawns
		{"", false},
	}
	for _, c := range cases {
		if got := spawnToolAvailable(c.identity); got != c.want {
			t.Errorf("spawnToolAvailable(%q) = %v, want %v", c.identity, got, c.want)
		}
	}
}

// TestNewMCPServer_ParentGetsSpawnTool asserts a parent aspect's
// materialised MCP surface INCLUDES spawn alongside the existing comms
// tools (and that none of the existing tools regressed).
func TestNewMCPServer_ParentGetsSpawnTool(t *testing.T) {
	b := &wsBridge{defaultFrom: "shadow", log: slog.Default()}
	srv := newMCPServer(b, slog.Default())
	tools := srv.ListTools()

	if _, ok := tools["spawn"]; !ok {
		t.Error("parent aspect surface missing spawn tool")
	}
	// Existing comms tools unaffected.
	for _, want := range []string{"send_chat", "read_chat", "react_to", "search_knowledge", "store_knowledge"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("comms tool %q missing from surface", want)
		}
	}
}

// TestNewMCPServer_HandHasNoSpawnTool asserts a derived hand identity's
// surface OMITS spawn (no-sub-of-sub) while keeping every comms tool.
func TestNewMCPServer_HandHasNoSpawnTool(t *testing.T) {
	b := &wsBridge{defaultFrom: "shadow.umbra", log: slog.Default()}
	srv := newMCPServer(b, slog.Default())
	tools := srv.ListTools()

	if _, ok := tools["spawn"]; ok {
		t.Error("derived hand surface must NOT include spawn (no sub-of-sub)")
	}
	// Hands still get the full comms surface.
	for _, want := range []string{"send_chat", "read_chat", "react_to"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("hand comms tool %q missing from surface", want)
		}
	}
}

// TestSpawnTool_Schema pins the tool name + required/optional args so
// the schema claude-code sees matches NEX-601.
func TestSpawnTool_Schema(t *testing.T) {
	tool := spawnTool()
	if tool.Name != "spawn" {
		t.Fatalf("tool name = %q, want spawn", tool.Name)
	}
	props := tool.InputSchema.Properties
	for _, want := range []string{"brief", "count", "thread"} {
		if _, ok := props[want]; !ok {
			t.Errorf("spawn schema missing property %q", want)
		}
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "brief" {
		t.Errorf("spawn required = %v, want [brief]", tool.InputSchema.Required)
	}
}

// TestFormatSpawnResult covers the three per-hand states surfaced to the
// model: running, queued, and failed.
func TestFormatSpawnResult(t *testing.T) {
	out := formatSpawnResult(frames.SpawnResultPayload{Hands: []frames.SpawnHandle{
		{Name: "shadow.umbra", RunID: "run-1"},
		{Name: "shadow.gloam"},
		{Name: "shadow.shade", Error: "image pull failed"},
	}})
	for _, want := range []string{"run-1", "queued", "image pull failed", "shadow.umbra", "shadow.gloam", "shadow.shade"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatSpawnResult output missing %q\ngot: %s", want, out)
		}
	}
}
