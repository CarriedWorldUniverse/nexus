package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandHandlerExitZeroParsesDecision(t *testing.T) {
	script := writeScript(t, `#!/bin/sh
python3 -c 'import json,sys; payload=json.load(sys.stdin); print(json.dumps({"additionalContext": payload["message"], "decision": "allow"}))'
`)

	got, err := NewCommandHandler(script).Run(context.Background(), map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got.AdditionalContext != "hello" || got.Decision != "allow" {
		t.Fatalf("Decision = %+v", got)
	}
}

func TestCommandHandlerExitTwoBlocksWithStderr(t *testing.T) {
	script := writeScript(t, `#!/bin/sh
echo "blocked by hook" >&2
exit 2
`)

	got, err := NewCommandHandler(script).Run(context.Background(), map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got.Decision != "block" || got.PermissionDecision != "block" {
		t.Fatalf("Decision = %+v, want block", got)
	}
	if got.SystemMessage != "blocked by hook" {
		t.Fatalf("SystemMessage = %q", got.SystemMessage)
	}
}

func TestCommandHandlerOtherExitIsNonBlockingError(t *testing.T) {
	script := writeScript(t, `#!/bin/sh
echo "bad script" >&2
exit 1
`)

	got, err := NewCommandHandler(script).Run(context.Background(), nil)
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
	if got.Decision == "block" || got.Decision == "deny" {
		t.Fatalf("Decision = %+v, want non-blocking zero decision", got)
	}
}

func TestHTTPHandlerPostsPayloadAndParsesDecision(t *testing.T) {
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(Decision{AdditionalContext: "from http"})
	}))
	t.Cleanup(srv.Close)

	got, err := NewHTTPHandler(srv.URL).Run(context.Background(), map[string]any{"x": "y"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got.AdditionalContext != "from http" {
		t.Fatalf("AdditionalContext = %q", got.AdditionalContext)
	}
	if gotPayload["x"] != "y" {
		t.Fatalf("payload = %#v", gotPayload)
	}
}

func TestMCPToolHandlerUsesInvoker(t *testing.T) {
	invoker := MCPInvokerFunc(func(_ context.Context, tool string, payload map[string]any) (Decision, error) {
		if tool != "search_knowledge" {
			t.Fatalf("tool = %q", tool)
		}
		if payload["query"] != "memory" {
			t.Fatalf("payload = %#v", payload)
		}
		return Decision{AdditionalContext: "memory hit"}, nil
	})

	got, err := NewMCPToolHandler("search_knowledge", invoker).Run(context.Background(), map[string]any{"query": "memory"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got.AdditionalContext != "memory hit" {
		t.Fatalf("AdditionalContext = %q", got.AdditionalContext)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if runtime.GOOS == "windows" {
		t.Skip("shell script test requires /bin/sh")
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

var _ = strings.Contains
