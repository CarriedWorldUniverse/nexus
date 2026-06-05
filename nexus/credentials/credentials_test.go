package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// newTestStore brings up an in-memory SQLite with the post-NEX-75
// `credentials` + `credential_audit` schema. Mirror of the live
// storage/schema.sql definitions — kept inline so the test stays
// self-contained.
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE credentials (
			name TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			encrypted_bundle BLOB NOT NULL,
			encryption_nonce BLOB NOT NULL,
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
		CREATE TABLE aspects (
			name TEXT PRIMARY KEY,
			default_anthropic_credential TEXT,
			default_openai_credential TEXT,
			default_jira_credential TEXT,
			default_imap_credential TEXT,
			-- NEX-263 per-aspect model + credential overrides
			primary_model TEXT,
			primary_credential TEXT,
			judge_model TEXT,
			judge_credential TEXT,
			judge_provider TEXT,
			compact_model TEXT,
			compact_credential TEXT
		);
		CREATE TABLE mcp_profiles (
			aspect_name TEXT PRIMARY KEY
			              REFERENCES aspects(name) ON DELETE CASCADE,
			profile     TEXT NOT NULL DEFAULT '{}',
			updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
		);
		-- NEX-294 network-wide judge + compact defaults (single-row).
		CREATE TABLE network_defaults (
			singleton            INTEGER PRIMARY KEY CHECK (singleton = 1),
			judge_model          TEXT,
			judge_credential     TEXT,
			judge_provider       TEXT,
			compact_model        TEXT,
			compact_credential   TEXT,
			updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT OR IGNORE INTO network_defaults (singleton) VALUES (1);
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

// providerBundle builds the canonical kind='provider' bundle map for
// tests. Mirrors the shape validateBundle enforces.
func providerBundle(shape APIShape, baseURL, key string) map[string]any {
	return map[string]any{
		"api_shape": string(shape),
		"base_url":  baseURL,
		"key":       key,
	}
}

func TestSetGetProviderRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	bundle := providerBundle(ShapeAnthropic, "https://api.deepseek.com/anthropic", "sk-deepseek-abc123")
	bundle["default_model"] = "deepseek-chat"
	err := s.Set(ctx, UpsertParams{
		Name:           "deepseek-anthropic",
		Description:    "DeepSeek via Anthropic shape",
		Kind:           KindProvider,
		Bundle:         bundle,
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
	if c.Kind != KindProvider {
		t.Errorf("kind: got %q want %q", c.Kind, KindProvider)
	}
	pb, err := s.ProviderBundle(c)
	if err != nil {
		t.Fatalf("ProviderBundle: %v", err)
	}
	if pb.APIShape != ShapeAnthropic {
		t.Errorf("api_shape: got %q", pb.APIShape)
	}
	if pb.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("base_url: got %q", pb.BaseURL)
	}
	if pb.Key != "sk-deepseek-abc123" {
		t.Errorf("key: got %q", pb.Key)
	}
	if pb.DefaultModel != "deepseek-chat" {
		t.Errorf("default_model: got %q", pb.DefaultModel)
	}
}

func TestSetGetJiraRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name: "jira-prod",
		Kind: KindJira,
		Bundle: map[string]any{
			"atlassian_email":     "ops@example.com",
			"atlassian_token":     "tok-abc-123",
			"atlassian_subdomain": "myorg",
		},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, err := s.Get(ctx, "jira-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Kind != KindJira {
		t.Errorf("kind: got %q want %q", c.Kind, KindJira)
	}
	jb, err := s.JiraBundle(c)
	if err != nil {
		t.Fatalf("JiraBundle: %v", err)
	}
	if jb.Email != "ops@example.com" || jb.Token != "tok-abc-123" || jb.Subdomain != "myorg" {
		t.Errorf("jira bundle round-trip mismatch: %+v", jb)
	}
}

// NEX-88: optional ProjectKey on JiraBundle survives store round-trip
// AND is omitted from the encrypted payload when unset (omitempty —
// keeps existing rows on disk byte-compatible with the new schema).
func TestSetGetJiraRoundTrip_WithProjectKey(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name: "jira-wks",
		Kind: KindJira,
		Bundle: map[string]any{
			"atlassian_email":     "ops@example.com",
			"atlassian_token":     "tok-abc-123",
			"atlassian_subdomain": "myorg",
			"project_key":         "WKS",
		},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, err := s.Get(ctx, "jira-wks")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	jb, err := s.JiraBundle(c)
	if err != nil {
		t.Fatalf("JiraBundle: %v", err)
	}
	if jb.ProjectKey != "WKS" {
		t.Errorf("ProjectKey: got %q want WKS", jb.ProjectKey)
	}
	if jb.Email != "ops@example.com" {
		t.Errorf("Email round-trip broken: got %q", jb.Email)
	}
}

// NEX-88: omitting project_key in the bundle preserves the back-compat
// contract — Get returns a JiraBundle with ProjectKey == "" and the
// other fields intact. Existing credential rows (created pre-NEX-88)
// must continue working without migration.
func TestSetGetJiraRoundTrip_WithoutProjectKey(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name: "jira-legacy",
		Kind: KindJira,
		Bundle: map[string]any{
			"atlassian_email":     "ops@example.com",
			"atlassian_token":     "tok-abc-123",
			"atlassian_subdomain": "myorg",
		},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, _ := s.Get(ctx, "jira-legacy")
	jb, err := s.JiraBundle(c)
	if err != nil {
		t.Fatalf("JiraBundle: %v", err)
	}
	if jb.ProjectKey != "" {
		t.Errorf("ProjectKey should be empty on a legacy bundle; got %q", jb.ProjectKey)
	}
	if jb.Email != "ops@example.com" {
		t.Errorf("Email round-trip broken: got %q", jb.Email)
	}
}

func TestSetGetIMAPRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name: "mail-default",
		Kind: KindIMAP,
		Bundle: map[string]any{
			"host":     "imap.example.com",
			"port":     993,
			"user":     "ops@example.com",
			"password": "hunter2",
			"ssl":      true,
		},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, err := s.Get(ctx, "mail-default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ib, err := s.IMAPBundle(c)
	if err != nil {
		t.Fatalf("IMAPBundle: %v", err)
	}
	if ib.Host != "imap.example.com" || ib.Port != 993 || ib.User != "ops@example.com" || ib.Password != "hunter2" || !ib.SSL {
		t.Errorf("imap bundle round-trip mismatch: %+v", ib)
	}
}

func TestKindMismatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name:           "jira-x",
		Kind:           KindJira,
		Bundle:         map[string]any{"atlassian_email": "a@b", "atlassian_token": "t", "atlassian_subdomain": "s"},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	c, _ := s.Get(ctx, "jira-x")
	if _, err := s.ProviderBundle(c); err == nil {
		t.Error("ProviderBundle on a jira credential should return ErrKindMismatch")
	}
	if _, err := s.IMAPBundle(c); err == nil {
		t.Error("IMAPBundle on a jira credential should return ErrKindMismatch")
	}
}

func TestSetUpsert(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := UpsertParams{
		Name:           "k",
		Kind:           KindProvider,
		Bundle:         providerBundle(ShapeOpenAI, "https://api.example.com", "v1"),
		AllowedAspects: []string{"keel"},
		Mode:           ModeProxy,
	}
	if err := s.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Bundle["key"] = "v2"
	p.Description = "updated"
	if err := s.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := s.ProviderBundle(c)
	if pb.Key != "v2" {
		t.Errorf("expected key v2, got %q", pb.Key)
	}
	if c.Description != "updated" {
		t.Errorf("description not updated: %q", c.Description)
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Get(context.Background(), "nope")
	if err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// Mix of kinds; List should return all without filter, filtered by kind otherwise.
	for _, n := range []string{"b-prov", "a-prov", "c-prov"} {
		_ = s.Set(ctx, UpsertParams{
			Name: n, Kind: KindProvider, Bundle: providerBundle(ShapeOpenAI, "x", "k"),
			AllowedAspects: []string{"*"}, Mode: ModeProxy,
		})
	}
	_ = s.Set(ctx, UpsertParams{
		Name: "d-jira", Kind: KindJira,
		Bundle:         map[string]any{"atlassian_email": "a@b", "atlassian_token": "t", "atlassian_subdomain": "s"},
		AllowedAspects: []string{"*"}, Mode: ModeFetch,
	})

	ms, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 {
		t.Fatalf("list-all len: got %d want 4", len(ms))
	}
	// alphabetical order by name
	want := []string{"a-prov", "b-prov", "c-prov", "d-jira"}
	for i, m := range ms {
		if m.Name != want[i] {
			t.Errorf("[%d] got %q want %q", i, m.Name, want[i])
		}
	}

	provs, err := s.List(ctx, KindProvider)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 3 {
		t.Errorf("provider filter len: got %d want 3", len(provs))
	}
	jiras, err := s.List(ctx, KindJira)
	if err != nil {
		t.Fatal(err)
	}
	if len(jiras) != 1 || jiras[0].Name != "d-jira" {
		t.Errorf("jira filter: got %+v", jiras)
	}
}

func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, UpsertParams{
		Name: "k", Kind: KindProvider, Bundle: providerBundle(ShapeOpenAI, "x", "v"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})
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
	good := providerBundle(ShapeOpenAI, "x", "k")
	cases := []struct {
		name string
		p    UpsertParams
		ok   bool
	}{
		{"empty name", UpsertParams{Kind: KindProvider, Bundle: good, AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"unknown kind", UpsertParams{Name: "n", Kind: "weird", Bundle: good, AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"nil bundle", UpsertParams{Name: "n", Kind: KindProvider, Bundle: nil, AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"bad provider shape", UpsertParams{Name: "n", Kind: KindProvider, Bundle: providerBundle("bogus", "x", "k"), AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"missing provider key", UpsertParams{Name: "n", Kind: KindProvider, Bundle: map[string]any{"api_shape": "openai", "base_url": "x"}, AllowedAspects: []string{"*"}, Mode: ModeProxy}, false},
		{"missing jira field", UpsertParams{Name: "n", Kind: KindJira, Bundle: map[string]any{"atlassian_email": "a@b", "atlassian_token": "t"}, AllowedAspects: []string{"*"}, Mode: ModeFetch}, false},
		{"bad mode", UpsertParams{Name: "n", Kind: KindProvider, Bundle: good, AllowedAspects: []string{"*"}, Mode: "weird"}, false},
		{"no aspects", UpsertParams{Name: "n", Kind: KindProvider, Bundle: good, Mode: ModeProxy}, false},
		{"ok provider", UpsertParams{Name: "n", Kind: KindProvider, Bundle: good, AllowedAspects: []string{"*"}, Mode: ModeProxy}, true},
		{"ok jira", UpsertParams{Name: "n", Kind: KindJira, Bundle: map[string]any{"atlassian_email": "a@b", "atlassian_token": "t", "atlassian_subdomain": "s"}, AllowedAspects: []string{"*"}, Mode: ModeFetch}, true},
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

func TestGitKind_RoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	err := s.Set(ctx, UpsertParams{
		Name:           "worker-git",
		Kind:           KindGit,
		Bundle:         map[string]any{"username": "nexus-cw", "password": "ghp_x", "host": "github.com"},
		AllowedAspects: []string{"worker-1"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set git: %v", err)
	}
	c, err := s.Get(ctx, "worker-git")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Kind != KindGit {
		t.Fatalf("kind = %q, want git", c.Kind)
	}
	gb, err := s.GitBundle(c)
	if err != nil {
		t.Fatalf("GitBundle: %v", err)
	}
	if gb.Username != "nexus-cw" || gb.Password != "ghp_x" || gb.Host != "github.com" {
		t.Fatalf("bundle = %+v", gb)
	}
	if !c.AllowedFor("worker-1") || c.AllowedFor("worker-2") {
		t.Fatalf("AllowedFor scoping wrong")
	}
}

func TestGitKind_RejectsIncompleteBundle(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.Set(context.Background(), UpsertParams{
		Name:           "bad-git",
		Kind:           KindGit,
		Bundle:         map[string]any{"username": "x"}, // missing password/host
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err == nil {
		t.Fatal("want validation error for incomplete git bundle")
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

func TestEnvForCredential_ProviderShapes(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, UpsertParams{
		Name: "anth", Kind: KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.anthropic.com", "sk-ant"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})
	_ = s.Set(ctx, UpsertParams{
		Name: "oai", Kind: KindProvider,
		Bundle:         providerBundle(ShapeOpenAI, "https://api.openai.com/v1", "sk-oai"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})

	a, _ := s.Get(ctx, "anth")
	env, err := s.EnvForCredential(a)
	if err != nil {
		t.Fatalf("EnvForCredential anth: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant" || env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Errorf("anthropic env: %+v", env)
	}

	o, _ := s.Get(ctx, "oai")
	env2, err := s.EnvForCredential(o)
	if err != nil {
		t.Fatalf("EnvForCredential oai: %v", err)
	}
	if env2["OPENAI_API_KEY"] != "sk-oai" || env2["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("openai env: %+v", env2)
	}
}

func TestAspectDefaults_RoundTrip(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	// Insert an aspect row + two provider creds.
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}
	_ = s.Set(ctx, UpsertParams{
		Name: "anth-prod", Kind: KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.anthropic.com", "sk-ant"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})
	_ = s.Set(ctx, UpsertParams{
		Name: "oai-prod", Kind: KindProvider,
		Bundle:         providerBundle(ShapeOpenAI, "https://api.openai.com/v1", "sk-oai"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})

	// Initial read: all unset.
	ad, err := s.GetAspectDefaults(ctx, "keel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ad.AnthropicDefault != nil || ad.OpenAIDefault != nil || ad.JiraDefault != nil || ad.IMAPDefault != nil {
		t.Errorf("expected all defaults nil, got %+v", ad)
	}

	// Set anthropic.
	if err := s.SetAspectDefault(ctx, "keel", "anthropic", "anth-prod"); err != nil {
		t.Fatalf("set anthropic: %v", err)
	}
	if err := s.SetAspectDefault(ctx, "keel", "openai", "oai-prod"); err != nil {
		t.Fatalf("set openai: %v", err)
	}
	ad, err = s.GetAspectDefaults(ctx, "keel")
	if err != nil {
		t.Fatalf("get after sets: %v", err)
	}
	if ad.AnthropicDefault == nil || *ad.AnthropicDefault != "anth-prod" {
		t.Errorf("anthropic default: got %v", ad.AnthropicDefault)
	}
	if ad.OpenAIDefault == nil || *ad.OpenAIDefault != "oai-prod" {
		t.Errorf("openai default: got %v", ad.OpenAIDefault)
	}

	// Clear anthropic (empty string).
	if err := s.SetAspectDefault(ctx, "keel", "anthropic", ""); err != nil {
		t.Fatalf("clear anthropic: %v", err)
	}
	ad, err = s.GetAspectDefaults(ctx, "keel")
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if ad.AnthropicDefault != nil {
		t.Errorf("anthropic default should be cleared, got %v", *ad.AnthropicDefault)
	}
	if ad.OpenAIDefault == nil || *ad.OpenAIDefault != "oai-prod" {
		t.Errorf("openai default lost during anthropic clear: %v", ad.OpenAIDefault)
	}
}

func TestSetAspectDefault_UnknownColumn(t *testing.T) {
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.SetAspectDefault(context.Background(), "keel", "weird", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown default-column") {
		t.Errorf("expected unknown-column error, got %v", err)
	}
}

func TestSetAspectDefault_UnknownCredential(t *testing.T) {
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.SetAspectDefault(context.Background(), "keel", "anthropic", "does-not-exist")
	if err == nil {
		t.Error("expected error setting default to nonexistent credential")
	}
}

func TestSetAspectDefault_UnknownAspect(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, UpsertParams{
		Name: "x", Kind: KindProvider,
		Bundle:         providerBundle(ShapeOpenAI, "u", "k"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})
	err := s.SetAspectDefault(ctx, "no-such-aspect", "openai", "x")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected aspect-not-found error, got %v", err)
	}
}

func TestEnvForCredential_NonProviderKindFails(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, UpsertParams{
		Name: "j", Kind: KindJira,
		Bundle:         map[string]any{"atlassian_email": "a@b", "atlassian_token": "t", "atlassian_subdomain": "s"},
		AllowedAspects: []string{"*"}, Mode: ModeFetch,
	})
	c, _ := s.Get(ctx, "j")
	if _, err := s.EnvForCredential(c); err == nil {
		t.Error("EnvForCredential on jira kind should fail with ErrKindMismatch")
	}
}

// NEX-263: per-aspect model override CRUD.

func TestAspectModelConfig_Lifecycle(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}
	_ = s.Set(ctx, UpsertParams{
		Name: "claude-api", Kind: KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.anthropic.com", "sk-ant"),
		AllowedAspects: []string{"*"}, Mode: ModeProxy,
	})

	// Initial read: all unset.
	cfg, err := s.GetAspectModelConfig(ctx, "keel")
	if err != nil {
		t.Fatalf("get initial: %v", err)
	}
	if cfg.PrimaryModel != nil || cfg.JudgeModel != nil || cfg.CompactModel != nil ||
		cfg.PrimaryCredential != nil || cfg.JudgeCredential != nil || cfg.CompactCredential != nil {
		t.Errorf("expected all-nil config, got %+v", cfg)
	}

	// Set primary_model + primary_credential.
	if err := s.SetAspectModelField(ctx, "keel", "primary_model", "claude-opus-4-7"); err != nil {
		t.Fatalf("set primary_model: %v", err)
	}
	if err := s.SetAspectModelField(ctx, "keel", "primary_credential", "claude-api"); err != nil {
		t.Fatalf("set primary_credential: %v", err)
	}
	cfg, _ = s.GetAspectModelConfig(ctx, "keel")
	if cfg.PrimaryModel == nil || *cfg.PrimaryModel != "claude-opus-4-7" {
		t.Errorf("primary_model: got %v", cfg.PrimaryModel)
	}
	if cfg.PrimaryCredential == nil || *cfg.PrimaryCredential != "claude-api" {
		t.Errorf("primary_credential: got %v", cfg.PrimaryCredential)
	}

	// Clear primary_model only — primary_credential should survive.
	if err := s.SetAspectModelField(ctx, "keel", "primary_model", ""); err != nil {
		t.Fatalf("clear primary_model: %v", err)
	}
	cfg, _ = s.GetAspectModelConfig(ctx, "keel")
	if cfg.PrimaryModel != nil {
		t.Errorf("primary_model should be cleared, got %v", *cfg.PrimaryModel)
	}
	if cfg.PrimaryCredential == nil || *cfg.PrimaryCredential != "claude-api" {
		t.Errorf("primary_credential lost during primary_model clear: %v", cfg.PrimaryCredential)
	}
}

func TestSetAspectModelField_UnknownColumn(t *testing.T) {
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.SetAspectModelField(context.Background(), "keel", "weird_column", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown model-config column") {
		t.Errorf("expected unknown-column error, got %v", err)
	}
}

func TestSetAspectModelField_UnknownCredential(t *testing.T) {
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.SetAspectModelField(context.Background(), "keel", "judge_credential", "does-not-exist")
	if err == nil {
		t.Error("expected error setting judge_credential to nonexistent credential")
	}
	// Model fields don't validate against credentials store — bare strings are fine.
	if err := s.SetAspectModelField(context.Background(), "keel", "judge_model", "any-string-model"); err != nil {
		t.Errorf("model field should not validate against credentials store: %v", err)
	}
}

func TestSetAspectModelField_UnknownAspect(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.SetAspectModelField(context.Background(), "no-such-aspect", "primary_model", "x")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected aspect-not-found error, got %v", err)
	}
}

func TestSetAspectModelField_ClearCredentialDoesNotValidate(t *testing.T) {
	// Clearing (empty value) should never touch the credentials store —
	// the credential being cleared may have already been deleted.
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('keel')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetAspectModelField(context.Background(), "keel", "judge_credential", ""); err != nil {
		t.Errorf("clear should not validate against credentials store: %v", err)
	}
}

// NEX-294: fresh DB returns zero NetworkDefaults — singleton row
// exists (INSERT OR IGNORE on bootstrap) but no columns set.
func TestGetNetworkDefaults_Empty(t *testing.T) {
	s, _ := newTestStore(t)
	nd, err := s.GetNetworkDefaults(context.Background())
	if err != nil {
		t.Fatalf("GetNetworkDefaults: %v", err)
	}
	if nd != (NetworkDefaults{}) {
		t.Errorf("expected zero defaults on fresh store; got %+v", nd)
	}
}

// NEX-294: SetNetworkDefaultField writes a single column; GetNetworkDefaults
// reflects it.
func TestSetGetNetworkDefaults_Roundtrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	// Seed a credential the JudgeCredential default can reference (the
	// setter validates existence for *_credential fields).
	if err := s.Set(ctx, UpsertParams{
		Name:           "deepseek-judge",
		Description:    "test",
		Kind:           KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.deepseek.com/anthropic", "sk-test"),
		AllowedAspects: []string{"*"},
		Mode:           ModeProxy,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := s.SetNetworkDefaultField(ctx, "judge_model", "deepseek-chat"); err != nil {
		t.Fatalf("set judge_model: %v", err)
	}
	if err := s.SetNetworkDefaultField(ctx, "judge_credential", "deepseek-judge"); err != nil {
		t.Fatalf("set judge_credential: %v", err)
	}
	if err := s.SetNetworkDefaultField(ctx, "compact_model", "deepseek-chat"); err != nil {
		t.Fatalf("set compact_model: %v", err)
	}

	nd, err := s.GetNetworkDefaults(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if nd.JudgeModel != "deepseek-chat" {
		t.Errorf("JudgeModel = %q, want deepseek-chat", nd.JudgeModel)
	}
	if nd.JudgeCredential != "deepseek-judge" {
		t.Errorf("JudgeCredential = %q, want deepseek-judge", nd.JudgeCredential)
	}
	if nd.CompactModel != "deepseek-chat" {
		t.Errorf("CompactModel = %q, want deepseek-chat", nd.CompactModel)
	}
	if nd.CompactCredential != "" {
		t.Errorf("CompactCredential = %q, want empty (unset)", nd.CompactCredential)
	}
}

// NEX-294: SetNetworkDefaultField rejects unknown columns + missing
// credential references.
func TestSetNetworkDefaultField_Validations(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if err := s.SetNetworkDefaultField(ctx, "primary_model", "x"); err == nil {
		t.Error("primary_model should be rejected (not defaultable at network level)")
	}
	if err := s.SetNetworkDefaultField(ctx, "judge_credential", "does-not-exist"); err == nil {
		t.Error("setting judge_credential to unknown credential should error")
	}
	// Clearing (empty value) should NOT validate against the store.
	if err := s.SetNetworkDefaultField(ctx, "judge_credential", ""); err != nil {
		t.Errorf("clearing judge_credential should not error: %v", err)
	}
}

// NEX-294 resolution: per-aspect override wins, network default
// fills the gap, neither = empty (caller's legacy fallback).
func TestEffectiveJudge_Resolution(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('anvil'), ('harrow')`); err != nil {
		t.Fatalf("seed aspects: %v", err)
	}
	if err := s.Set(ctx, UpsertParams{
		Name:           "deepseek-judge",
		Kind:           KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.deepseek.com/v1", "sk-test"),
		AllowedAspects: []string{"*"},
		Mode:           ModeProxy,
	}); err != nil {
		t.Fatalf("seed cred: %v", err)
	}
	if err := s.Set(ctx, UpsertParams{
		Name:           "anthropic-judge",
		Kind:           KindProvider,
		Bundle:         providerBundle(ShapeAnthropic, "https://api.anthropic.com", "sk-ant-test"),
		AllowedAspects: []string{"*"},
		Mode:           ModeProxy,
	}); err != nil {
		t.Fatalf("seed cred: %v", err)
	}

	// Network default: judge_model + judge_credential -> deepseek
	if err := s.SetNetworkDefaultField(ctx, "judge_model", "deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkDefaultField(ctx, "judge_credential", "deepseek-judge"); err != nil {
		t.Fatal(err)
	}
	// Per-aspect override on anvil only: judge -> anthropic
	if err := s.SetAspectModelField(ctx, "anvil", "judge_model", "haiku-special"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAspectModelField(ctx, "anvil", "judge_credential", "anthropic-judge"); err != nil {
		t.Fatal(err)
	}

	// anvil: override wins
	if m, err := s.EffectiveJudgeModel(ctx, "anvil"); err != nil || m != "haiku-special" {
		t.Errorf("anvil EffectiveJudgeModel = %q err=%v, want haiku-special", m, err)
	}
	if c, err := s.EffectiveJudgeCredential(ctx, "anvil"); err != nil || c != "anthropic-judge" {
		t.Errorf("anvil EffectiveJudgeCredential = %q err=%v, want anthropic-judge", c, err)
	}
	// harrow: no override, network default applies
	if m, err := s.EffectiveJudgeModel(ctx, "harrow"); err != nil || m != "deepseek-chat" {
		t.Errorf("harrow EffectiveJudgeModel = %q err=%v, want deepseek-chat (network default)", m, err)
	}
	if c, err := s.EffectiveJudgeCredential(ctx, "harrow"); err != nil || c != "deepseek-judge" {
		t.Errorf("harrow EffectiveJudgeCredential = %q err=%v, want deepseek-judge (network default)", c, err)
	}

	// Clear network defaults: harrow should now return empty (legacy fallback territory)
	_ = s.SetNetworkDefaultField(ctx, "judge_model", "")
	_ = s.SetNetworkDefaultField(ctx, "judge_credential", "")
	if m, err := s.EffectiveJudgeModel(ctx, "harrow"); err != nil || m != "" {
		t.Errorf("harrow with no defaults: EffectiveJudgeModel = %q err=%v, want empty", m, err)
	}
}

// The network-defaults GET response is consumed by the dashboard JS as
// snake_case keys (fresh.judge_model, fresh.judge_provider, …). Pin the
// wire shape so a missing/renamed json tag — which Go round-trip tests
// can't catch (they marshal+unmarshal the same struct) — fails the build
// instead of silently blanking the settings panel on load.
func TestNetworkDefaults_JSONWireShape(t *testing.T) {
	b, err := json.Marshal(NetworkDefaults{
		JudgeModel: "m", JudgeCredential: "c", JudgeProvider: "claude-api",
		CompactModel: "cm", CompactCredential: "cc",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"judge_model", "judge_credential", "judge_provider", "compact_model", "compact_credential"} {
		if _, ok := got[k]; !ok {
			t.Errorf("network-defaults JSON missing snake_case key %q (got %s) — dashboard reads this key", k, b)
		}
	}
}

// NEX-365 #3: judge-PROVIDER resolution mirrors judge-model — per-aspect
// override wins, network default fills the gap, neither = empty (caller
// keeps its own filter_provider / inherited provider). This is the lever
// that lets the operator put EVERY aspect's cheap-judge on one cross-
// provider endpoint without touching each aspect's primary provider.
func TestEffectiveJudgeProvider_Resolution(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('anvil'), ('harrow')`); err != nil {
		t.Fatalf("seed aspects: %v", err)
	}

	// No network default, no override → empty (neither layer set).
	if p, err := s.EffectiveJudgeProvider(ctx, "harrow"); err != nil || p != "" {
		t.Errorf("no policy: EffectiveJudgeProvider = %q err=%v, want empty", p, err)
	}

	// Network-wide judge provider → applies to every aspect.
	if err := s.SetNetworkDefaultField(ctx, "judge_provider", "claude-api"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.EffectiveJudgeProvider(ctx, "harrow"); err != nil || p != "claude-api" {
		t.Errorf("harrow EffectiveJudgeProvider = %q err=%v, want claude-api (network default)", p, err)
	}

	// Per-aspect override wins over the network default.
	if err := s.SetAspectModelField(ctx, "anvil", "judge_provider", "claude-code"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.EffectiveJudgeProvider(ctx, "anvil"); err != nil || p != "claude-code" {
		t.Errorf("anvil EffectiveJudgeProvider = %q err=%v, want claude-code (override wins)", p, err)
	}
	// harrow still sees the network default.
	if p, err := s.EffectiveJudgeProvider(ctx, "harrow"); err != nil || p != "claude-api" {
		t.Errorf("harrow EffectiveJudgeProvider = %q err=%v, want claude-api", p, err)
	}

	// Clearing the network default drops harrow back to empty.
	if err := s.SetNetworkDefaultField(ctx, "judge_provider", ""); err != nil {
		t.Fatal(err)
	}
	if p, err := s.EffectiveJudgeProvider(ctx, "harrow"); err != nil || p != "" {
		t.Errorf("cleared: harrow EffectiveJudgeProvider = %q err=%v, want empty", p, err)
	}
	// anvil keeps its per-aspect override regardless.
	if p, err := s.EffectiveJudgeProvider(ctx, "anvil"); err != nil || p != "claude-code" {
		t.Errorf("anvil EffectiveJudgeProvider = %q err=%v, want claude-code", p, err)
	}
}

// NEX-294: compact-side resolution mirrors judge.
func TestEffectiveCompact_Resolution(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	if _, err := db.Exec(`INSERT INTO aspects(name) VALUES ('wren')`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkDefaultField(ctx, "compact_model", "deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	if m, err := s.EffectiveCompactModel(ctx, "wren"); err != nil || m != "deepseek-chat" {
		t.Errorf("wren EffectiveCompactModel = %q err=%v, want deepseek-chat", m, err)
	}
}
