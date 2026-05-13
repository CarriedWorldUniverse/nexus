package credentials

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE provider_credentials (
			name TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			api_shape TEXT NOT NULL,
			base_url TEXT NOT NULL,
			encrypted_key BLOB NOT NULL,
			encryption_nonce BLOB NOT NULL,
			default_model TEXT,
			allowed_aspects TEXT NOT NULL DEFAULT '["*"]',
			mode TEXT NOT NULL DEFAULT 'proxy',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_used_at TEXT
		);
		CREATE TABLE credential_audit (
			id INTEGER PRIMARY KEY,
			credential_name TEXT NOT NULL,
			aspect TEXT NOT NULL,
			action TEXT NOT NULL,
			ts TEXT NOT NULL DEFAULT (datetime('now')),
			details TEXT NOT NULL DEFAULT '{}'
		);
	`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	secret := []byte("test-session-signing-secret-32-bytes-padded")
	s, err := NewStore(db, secret)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, db
}

func TestSetGetDecrypt_RoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name:           "deepseek-anthropic",
		Description:    "DeepSeek via Anthropic shape",
		APIShape:       ShapeAnthropic,
		BaseURL:        "https://api.deepseek.com/anthropic",
		Key:            "sk-deepseek-abc123",
		DefaultModel:   "deepseek-chat",
		AllowedAspects: []string{"*"},
		Mode:           ModeProxy,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, err := s.Get(ctx, "deepseek-anthropic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.APIShape != ShapeAnthropic {
		t.Errorf("api_shape: got %q", c.APIShape)
	}
	if c.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("base_url: got %q", c.BaseURL)
	}
	plain, err := s.Decrypt(c)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "sk-deepseek-abc123" {
		t.Errorf("plaintext mismatch: got %q", plain)
	}
}

func TestSet_Upsert(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := UpsertParams{
		Name:           "k",
		APIShape:       ShapeOpenAI,
		BaseURL:        "https://api.example.com",
		Key:            "v1",
		AllowedAspects: []string{"keel"},
		Mode:           ModeProxy,
	}
	if err := s.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Key = "v2"
	p.Description = "updated"
	if err := s.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := s.Decrypt(c)
	if plain != "v2" {
		t.Errorf("expected key v2, got %q", plain)
	}
	if c.Description != "updated" {
		t.Errorf("description not updated: %q", c.Description)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Get(context.Background(), "nope")
	if err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, n := range []string{"b", "a", "c"} {
		_ = s.Set(ctx, UpsertParams{Name: n, APIShape: ShapeOpenAI, BaseURL: "x", Key: "k", AllowedAspects: []string{"*"}, Mode: ModeProxy})
	}
	ms, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("len: %d", len(ms))
	}
	if ms[0].Name != "a" || ms[1].Name != "b" || ms[2].Name != "c" {
		t.Errorf("order: %+v", ms)
	}
}

func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, UpsertParams{Name: "k", APIShape: ShapeOpenAI, BaseURL: "x", Key: "v", AllowedAspects: []string{"*"}, Mode: ModeProxy})
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "k"); err != ErrNotFound {
		t.Errorf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestAllowedFor(t *testing.T) {
	c := Credential{AllowedAspects: []string{"keel", "anvil"}}
	if !c.AllowedFor("keel") {
		t.Error("keel should be allowed")
	}
	if c.AllowedFor("forge") {
		t.Error("forge should be denied")
	}
	c2 := Credential{AllowedAspects: []string{"*"}}
	if !c2.AllowedFor("anyone") {
		t.Error("wildcard should allow anyone")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		p    UpsertParams
		ok   bool
	}{
		{"empty name", UpsertParams{APIShape: ShapeOpenAI, BaseURL: "x", Key: "k", AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"bad shape", UpsertParams{Name: "n", APIShape: "bogus", BaseURL: "x", Key: "k", AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"no base url", UpsertParams{Name: "n", APIShape: ShapeOpenAI, Key: "k", AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"no key", UpsertParams{Name: "n", APIShape: ShapeOpenAI, BaseURL: "x", AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"bad mode", UpsertParams{Name: "n", APIShape: ShapeOpenAI, BaseURL: "x", Key: "k", AllowedAspects: []string{"*"}, Mode: "weird"}, false},
		{"no aspects", UpsertParams{Name: "n", APIShape: ShapeOpenAI, BaseURL: "x", Key: "k", Mode: ModeProxy}, false},
		{"ok", UpsertParams{Name: "n", APIShape: ShapeOpenAI, BaseURL: "x", Key: "k", AllowedAspects: []string{"*"}, Mode: ModeProxy}, true},
	}
	for _, tc := range cases {
		err := validateUpsert(tc.p)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestAudit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		err := s.RecordAudit(ctx, AuditEvent{
			CredentialName: "k",
			Aspect:         "keel",
			Action:         AuditProxyCall,
			Details:        map[string]any{"model": "claude-opus-4-7"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListAudit(ctx, "k", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("len: %d", len(rows))
	}
}

func TestEncryptIsNondeterministic(t *testing.T) {
	s, _ := newTestStore(t)
	c1, n1, err := s.encrypt([]byte("same-plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	c2, n2, err := s.encrypt([]byte("same-plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) == string(c2) || string(n1) == string(n2) {
		t.Error("encrypt should be nondeterministic (different nonces)")
	}
}
