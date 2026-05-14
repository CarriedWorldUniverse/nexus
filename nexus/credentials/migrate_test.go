package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// newMigrationTestDB sets up a DB with BOTH the legacy and new tables.
// Seed the legacy with a known provider credential encrypted under the
// shared dataKey, then exercise MigrateLegacyTable and verify the row
// surfaces in the new table with identical decrypted contents.
func newMigrationTestDB(t *testing.T) (*Store, *sql.DB) {
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

// seedLegacyProvider writes a row to the OLD `provider_credentials`
// table using the store's data key so the migration path can decrypt it.
func seedLegacyProvider(t *testing.T, s *Store, db *sql.DB, name, shape, baseURL, key, defaultModel string) {
	t.Helper()
	enc, nonce, err := s.encrypt([]byte(key))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	allowedJSON, _ := json.Marshal([]string{"*"})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var defModelArg any
	if defaultModel != "" {
		defModelArg = defaultModel
	}
	_, err = db.Exec(`
		INSERT INTO provider_credentials
			(name, description, api_shape, base_url,
			 encrypted_key, encryption_nonce, default_model,
			 allowed_aspects, mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, name, "seeded", shape, baseURL, enc, nonce, defModelArg, string(allowedJSON), "proxy", now, now)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
}

func TestMigrateLegacyTable_HappyPath(t *testing.T) {
	s, db := newMigrationTestDB(t)
	ctx := context.Background()

	seedLegacyProvider(t, s, db, "anth-prod", "anthropic", "https://api.anthropic.com", "sk-ant-xxx", "claude-opus-4-7")
	seedLegacyProvider(t, s, db, "deepseek-anth", "anthropic", "https://api.deepseek.com/anthropic", "sk-ds-xxx", "deepseek-chat")
	seedLegacyProvider(t, s, db, "openai", "openai", "https://api.openai.com/v1", "sk-oai-xxx", "")

	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("MigrateLegacyTable: %v", err)
	}

	// Legacy table should be gone.
	exists, err := tableExists(ctx, db, "provider_credentials")
	if err != nil {
		t.Fatalf("tableExists check: %v", err)
	}
	if exists {
		t.Error("provider_credentials should be dropped after migration")
	}

	// All three rows should be present in the new table with kind=provider
	// and decryptable bundles matching what we seeded.
	cases := []struct {
		name, shape, baseURL, key, defaultModel string
	}{
		{"anth-prod", "anthropic", "https://api.anthropic.com", "sk-ant-xxx", "claude-opus-4-7"},
		{"deepseek-anth", "anthropic", "https://api.deepseek.com/anthropic", "sk-ds-xxx", "deepseek-chat"},
		{"openai", "openai", "https://api.openai.com/v1", "sk-oai-xxx", ""},
	}
	for _, tc := range cases {
		c, err := s.Get(ctx, tc.name)
		if err != nil {
			t.Fatalf("Get %q: %v", tc.name, err)
		}
		if c.Kind != KindProvider {
			t.Errorf("%s: kind got %q want provider", tc.name, c.Kind)
		}
		pb, err := s.ProviderBundle(c)
		if err != nil {
			t.Fatalf("ProviderBundle %q: %v", tc.name, err)
		}
		if string(pb.APIShape) != tc.shape {
			t.Errorf("%s: api_shape got %q want %q", tc.name, pb.APIShape, tc.shape)
		}
		if pb.BaseURL != tc.baseURL {
			t.Errorf("%s: base_url got %q want %q", tc.name, pb.BaseURL, tc.baseURL)
		}
		if pb.Key != tc.key {
			t.Errorf("%s: key round-trip mismatch", tc.name)
		}
		if pb.DefaultModel != tc.defaultModel {
			t.Errorf("%s: default_model got %q want %q", tc.name, pb.DefaultModel, tc.defaultModel)
		}
	}
}

func TestMigrateLegacyTable_Idempotent(t *testing.T) {
	s, db := newMigrationTestDB(t)
	ctx := context.Background()
	seedLegacyProvider(t, s, db, "x", "openai", "u", "k", "")
	// First call performs the migration.
	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call should be a no-op (legacy table is gone).
	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	// Third call after re-creating the legacy table (simulating an
	// upgrade rollback then re-upgrade) should NOT clobber existing
	// destination rows when the destination already has the same name.
	_, err := db.Exec(`
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
		)
	`)
	if err != nil {
		t.Fatalf("re-create legacy: %v", err)
	}
	seedLegacyProvider(t, s, db, "x", "openai", "u", "OLD-VALUE", "")
	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("third call: %v", err)
	}
	// Verify the destination still has the original "k", not "OLD-VALUE"
	c, err := s.Get(ctx, "x")
	if err != nil {
		t.Fatalf("Get after re-migration: %v", err)
	}
	pb, _ := s.ProviderBundle(c)
	if pb.Key != "k" {
		t.Errorf("destination clobbered by re-migration: got %q want %q (k)", pb.Key, "k")
	}
}

func TestMigrateLegacyTable_NoLegacyTable(t *testing.T) {
	s, db := newMigrationTestDB(t)
	ctx := context.Background()
	// Drop the legacy table without seeding it — simulates a fresh-DB
	// boot where schema.sql never created provider_credentials.
	if _, err := db.Exec(`DROP TABLE provider_credentials`); err != nil {
		t.Fatalf("drop legacy: %v", err)
	}
	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("MigrateLegacyTable on fresh DB: %v", err)
	}
}

func TestMigrateLegacyTable_PreservesTimestamps(t *testing.T) {
	s, db := newMigrationTestDB(t)
	ctx := context.Background()

	// Seed with a known timestamp explicitly (not "now").
	enc, nonce, _ := s.encrypt([]byte("k"))
	allowedJSON, _ := json.Marshal([]string{"*"})
	stamp := "2026-01-15T10:00:00Z"
	_, err := db.Exec(`
		INSERT INTO provider_credentials
			(name, description, api_shape, base_url,
			 encrypted_key, encryption_nonce, default_model,
			 allowed_aspects, mode, created_at, updated_at)
		VALUES ('vintage', '', 'openai', 'u', ?, ?, NULL, ?, 'proxy', ?, ?)
	`, enc, nonce, string(allowedJSON), stamp, stamp)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.MigrateLegacyTable(ctx); err != nil {
		t.Fatalf("MigrateLegacyTable: %v", err)
	}

	c, err := s.Get(ctx, "vintage")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// CreatedAt + UpdatedAt should match what we seeded — migration
	// must NOT bump them to migration-time.
	want, _ := time.Parse(time.RFC3339, stamp)
	if !c.CreatedAt.Equal(want) {
		t.Errorf("created_at: got %v want %v", c.CreatedAt, want)
	}
	if !c.UpdatedAt.Equal(want) {
		t.Errorf("updated_at: got %v want %v", c.UpdatedAt, want)
	}
}
