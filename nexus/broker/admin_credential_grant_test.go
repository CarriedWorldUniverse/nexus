package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

func TestAdminCredentialGrant(t *testing.T) {
	srv, store, tok := newCredentialAdminTestServer(t)
	ctx := context.Background()
	if err := store.Set(ctx, credentials.UpsertParams{
		Name:           "worker-git",
		Kind:           credentials.KindGit,
		Bundle:         map[string]any{"username": "u", "password": "p", "host": "github.com"},
		AllowedAspects: []string{"worker-1"},
		Mode:           credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	do := func(name, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/admin/credentials/"+name+"/grant",
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do("worker-git", `{"aspect":"worker-2"}`); code != 200 {
		t.Fatalf("grant status=%d want 200", code)
	}
	c, _ := store.Get(ctx, "worker-git")
	if !c.AllowedFor("worker-1") || !c.AllowedFor("worker-2") {
		t.Fatalf("allowed = %v", c.AllowedAspects)
	}
	if code := do("nope", `{"aspect":"x"}`); code != 404 {
		t.Fatalf("missing-cred grant status=%d want 404", code)
	}
	if code := do("worker-git", `{}`); code != 400 {
		t.Fatalf("empty-aspect grant status=%d want 400", code)
	}
}
