package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// TestResolveMCPProfilePersonalityFallback: a pool spawn ({personality}-{role})
// with no profile of its own inherits its personality's mcp_profile — the fix
// for pool workers silently getting no MCP servers.
func TestResolveMCPProfilePersonalityFallback(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	astore := aspects.NewSQLStore(db)
	cstore, err := credentials.NewStore(db, []byte("test-session-signing-secret-32-bytes-padded"))
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}
	ctx := context.Background()
	mustInsert := func(name string) {
		if err := astore.Insert(ctx, aspects.Aspect{Name: name, Status: aspects.StatusActive, Provider: "openai", Model: "ornith"}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	// personality "anvil" carries a vision profile; the spawn has none.
	mustInsert("anvil")
	visionProfile := `{"mcpServers":{"nexus-vision":{"command":"/usr/local/bin/nexus-vision-mcp","args":[]}}}`
	if err := cstore.SetMCPProfile(ctx, "anvil", visionProfile); err != nil {
		t.Fatalf("set anvil profile: %v", err)
	}
	got, err := resolveMCPProfile(ctx, cstore, "anvil-builder-complex")
	if err != nil {
		t.Fatalf("resolve spawn: %v", err)
	}
	if !strings.Contains(got, "nexus-vision") {
		t.Fatalf("spawn should inherit personality profile, got %q", got)
	}

	// a spawn WITH its own profile uses it, not the personality's.
	mustInsert("anvil-builder")
	own := `{"mcpServers":{"nexus-comms":{"command":"x","args":[]}}}`
	if err := cstore.SetMCPProfile(ctx, "anvil-builder", own); err != nil {
		t.Fatalf("set spawn profile: %v", err)
	}
	got2, err := resolveMCPProfile(ctx, cstore, "anvil-builder")
	if err != nil {
		t.Fatalf("resolve own: %v", err)
	}
	if !strings.Contains(got2, "nexus-comms") || strings.Contains(got2, "nexus-vision") {
		t.Fatalf("own profile should win over personality's, got %q", got2)
	}

	// unknown personality → empty, no error (not a hard failure).
	got3, err := resolveMCPProfile(ctx, cstore, "ghost-builder")
	if err != nil || got3 != "" {
		t.Fatalf("unknown → empty; got %q err %v", got3, err)
	}
}
