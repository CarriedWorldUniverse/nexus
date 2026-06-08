package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigBuildsRegistry(t *testing.T) {
	cfg := Config{
		Hooks: map[string][]MatcherConfig{
			"SessionStart": {
				{
					Matcher: "",
					Hooks: []HandlerConfig{
						{Type: "command", Command: writeScript(t, `#!/bin/sh
echo '{"additionalContext":"loaded"}'
`)},
					},
				},
			},
		},
	}

	engine, err := LoadConfig(cfg, nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	got, err := engine.Dispatch(context.Background(), "SessionStart", nil)
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if got.AdditionalContext != "loaded" {
		t.Fatalf("AdditionalContext = %q", got.AdditionalContext)
	}
}

func TestLoadFileReadsHooksJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "mcp_tool", "tool": "audit_tool", "timeout": 1}
        ]
      }
    ]
  }
}`), 0o644); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
	invoker := MCPInvokerFunc(func(context.Context, string, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "from file"}, nil
	})

	engine, err := LoadFile(path, LoadOptions{MCPInvoker: invoker})
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	got, err := engine.Dispatch(context.Background(), "PreToolUse", map[string]any{"tool_name": "Bash"})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if got.AdditionalContext != "from file" {
		t.Fatalf("AdditionalContext = %q", got.AdditionalContext)
	}
}
