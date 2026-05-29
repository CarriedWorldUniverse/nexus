package broker

import (
	"context"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// NEX-332 phase 4: resolveProviderEnv must FAIL SAFE — return nil (caller
// falls back to its own process env) for every miss, never error out the
// validate handshake. These guard branches matter most: a misconfigured
// default must not block an otherwise-valid keyfile from connecting.
func TestResolveProviderEnv_Guards(t *testing.T) {
	ctx := context.Background()

	// nil store (legacy boot) → nil.
	if env := resolveProviderEnv(ctx, nil, "wren", "openai", nil); env != nil {
		t.Fatalf("nil store should yield nil, got %v", env)
	}

	db, err := storage.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := credentials.NewStore(db, []byte("test-secret-32-bytes-padding-vvvv"))
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}

	// claude-code self-authenticates (subscription/keychain) → no delivery,
	// without even touching the store.
	if env := resolveProviderEnv(ctx, store, "wren", "claude-code", nil); env != nil {
		t.Fatalf("claude-code should yield nil, got %v", env)
	}

	// Unknown provider → nil.
	if env := resolveProviderEnv(ctx, store, "wren", "bogus", nil); env != nil {
		t.Fatalf("unknown provider should yield nil, got %v", env)
	}

	// Known shape but no default configured for the aspect → nil (ErrNoDefault).
	if env := resolveProviderEnv(ctx, store, "wren", "openai", nil); env != nil {
		t.Fatalf("no default bound should yield nil, got %v", env)
	}
	if env := resolveProviderEnv(ctx, store, "wren", "claude-api", nil); env != nil {
		t.Fatalf("no default bound (anthropic) should yield nil, got %v", env)
	}
}
