package credentials

import (
	"context"
	"testing"
)

func TestResolveGitForAspect(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mk := func(name, host string, allowed ...string) {
		if err := s.Set(ctx, UpsertParams{
			Name: name, Kind: KindGit,
			Bundle:         map[string]any{"username": "u", "password": "p", "host": host},
			AllowedAspects: allowed, Mode: ModeFetch,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	mk("gh", "github.com", "worker-1")
	mk("gl", "gitlab.com", "worker-1")
	mk("only", "example.com", "solo")

	// host match among multiple allowed
	if c, err := s.ResolveGitForAspect(ctx, "worker-1", "github.com"); err != nil || c.Name != "gh" {
		t.Fatalf("host match: c=%q err=%v", c.Name, err)
	}
	// host with no matching cred → ErrNoDefault
	if _, err := s.ResolveGitForAspect(ctx, "worker-1", "nope.com"); err != ErrNoDefault {
		t.Fatalf("no host match: want ErrNoDefault, got %v", err)
	}
	// not allowed → ErrNoDefault
	if _, err := s.ResolveGitForAspect(ctx, "worker-2", "github.com"); err != ErrNoDefault {
		t.Fatalf("not allowed: want ErrNoDefault, got %v", err)
	}
	// empty host + multiple allowed → ErrNoDefault (ambiguous)
	if _, err := s.ResolveGitForAspect(ctx, "worker-1", ""); err != ErrNoDefault {
		t.Fatalf("ambiguous: want ErrNoDefault, got %v", err)
	}
	// empty host + single allowed → the sole cred
	if c, err := s.ResolveGitForAspect(ctx, "solo", ""); err != nil || c.Name != "only" {
		t.Fatalf("sole: c=%q err=%v", c.Name, err)
	}
}
