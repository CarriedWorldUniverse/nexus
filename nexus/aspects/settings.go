// Nexus settings (network-wide central content) — Part 9a.
//
// Per agent-network/docs/2026-05-08-personality-decomposition-spec.md:
// the central nexus_md lives in a single-row nexus_settings table,
// network-wide and admin-edited only. Per-aspect aspect_personalities
// .nexus_md remains as a short delta layered on top.
//
// This file holds the storage primitives. Consumers:
//   - frame.EmbeddedFrame.SystemPrompt (Part 9b) reads + concats the
//     central chunk above the per-aspect bundle.
//   - admin REST + CLI surfaces (Part 9c) edit it.
//   - migration (this file's MigrateCentralFromAspect) seeds it once
//     from existing aspect_personalities rows.

package aspects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// NexusSettings is the loaded single-row nexus_settings state.
type NexusSettings struct {
	NexusMD   string
	Version   int64
	UpdatedAt string // ISO 8601
}

// SettingsStore is the read/write interface for nexus_settings. Kept
// separate from Store so consumers that only need the central chunk
// don't pull in the full aspects surface (and so admin endpoints can
// gate access by capability).
type SettingsStore interface {
	// Get returns the current central settings. Returns a zero-valued
	// row (empty NexusMD, Version=1) if the row hasn't been initialised
	// — the schema's DEFAULT clauses ensure that bootstrap state is
	// always present once the table exists.
	Get(ctx context.Context) (*NexusSettings, error)

	// SetNexusMD writes new content to the central nexus_md column,
	// bumps version, stamps updated_at. Caller is responsible for
	// firing the network-wide refresh callback (Part 9d). Returns the
	// new version.
	SetNexusMD(ctx context.Context, content string) (int64, error)
}

// SQLSettingsStore is the *sql.DB-backed implementation.
type SQLSettingsStore struct {
	db *sql.DB
}

// NewSQLSettingsStore wraps a *sql.DB.
func NewSQLSettingsStore(db *sql.DB) *SQLSettingsStore {
	return &SQLSettingsStore{db: db}
}

// Get implements SettingsStore. Uses INSERT-OR-IGNORE-then-SELECT
// pattern so the first read after table creation returns the default
// row rather than ErrNoRows. SQLite's INSERT OR IGNORE is atomic so
// concurrent first-readers don't collide.
//
// The default row carries version=0 (per schema). First SetNexusMD
// always lands at version >= 1 — refresh-callback subscribers can
// rely on version-bump-on-first-write working uniformly regardless of
// whether Get was called first.
func (s *SQLSettingsStore) Get(ctx context.Context) (*NexusSettings, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO nexus_settings (id, nexus_md, version)
		VALUES (1, '', 0)
	`); err != nil {
		return nil, fmt.Errorf("settings.Get: ensure row: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT nexus_md, version, updated_at FROM nexus_settings WHERE id = 1
	`)
	var ns NexusSettings
	if err := row.Scan(&ns.NexusMD, &ns.Version, &ns.UpdatedAt); err != nil {
		return nil, fmt.Errorf("settings.Get: scan: %w", err)
	}
	return &ns, nil
}

// SetNexusMD implements SettingsStore. Atomic UPDATE-or-INSERT via
// upsert so the row is materialised on first write even if Get hasn't
// been called yet.
//
// First-write semantics: INSERT arm starts version at 1 (matches
// "this is the first set"); subsequent writes bump from there. The
// default row from Get sits at version=0; if Get was called first,
// SetNexusMD bumps 0→1 via the ON CONFLICT path. Either way, the
// first set lands at version=1 and refresh subscribers see the bump.
func (s *SQLSettingsStore) SetNexusMD(ctx context.Context, content string) (int64, error) {
	var newVersion int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO nexus_settings (id, nexus_md, version)
		VALUES (1, ?, 1)
		ON CONFLICT(id) DO UPDATE SET
			nexus_md   = excluded.nexus_md,
			version    = nexus_settings.version + 1,
			updated_at = datetime('now')
		RETURNING version
	`, content).Scan(&newVersion)
	if err != nil {
		return 0, fmt.Errorf("settings.SetNexusMD: %w", err)
	}
	return newVersion, nil
}

// MigrationResult summarises what MigrateCentralFromAspect did. Used
// by callers (boot path, CLI dispatch) to log/render the outcome.
type MigrationResult struct {
	// SeededFrom names the aspect whose nexus_md content was used as
	// the seed. Empty if nothing was migrated (table already populated
	// or no source rows).
	SeededFrom string

	// ContentBytes is the size of the seeded content.
	ContentBytes int

	// DivergentAspects lists names of aspects whose nexus_md differs
	// from the seeded content. Operator should manually prune these
	// after migration to avoid duplication in the composed prompt.
	DivergentAspects []string

	// Skipped is true when the migration was a no-op (nexus_settings
	// already has non-empty content, or no aspects had any nexus_md).
	Skipped bool
	Reason  string // why skipped, when Skipped == true
}

// schemaMetaKeyMigrated is the schema_meta key set after a successful
// migration. Idempotent persistent marker — once set, the migration
// never runs again, even if the operator deliberately blanks central
// content via the admin endpoint and restarts. Without this marker,
// the empty-string idempotency check would re-seed from per-aspect
// rows that may have been pruned post-migration to short deltas.
const schemaMetaKeyMigrated = "nexus_settings_migrated"

// MigrateCentralFromAspect is the spec §6 one-shot bootstrap. Seeds
// nexus_settings.nexus_md from existing aspect_personalities rows.
//
// Selection rule:
//  1. Prefer the row for `preferredFrame` (typically "keel"). Its
//     content is authoritative for network-wide operational scope.
//  2. If that aspect has no row (or empty nexus_md), fall back to
//     the most-recently-updated non-empty row.
//  3. If no aspects have any nexus_md content, skip silently.
//
// Per spec §6 (revised): per-aspect rows are LEFT UNTOUCHED. Operator
// manually prunes duplicates after migration via personality edit.
//
// Idempotent: persistent schema_meta marker prevents re-runs after
// the first successful migration. The empty-string fallback check is
// retained as belt-and-suspenders for upgrades from a brief window
// where the marker wasn't yet persisted.
func MigrateCentralFromAspect(ctx context.Context, db *sql.DB, preferredFrame string) (*MigrationResult, error) {
	// Persistent marker check first — once migration has run
	// successfully, the row is operator's territory and we never
	// overwrite it.
	var migratedFlag string
	err := db.QueryRowContext(ctx, `
		SELECT value FROM schema_meta WHERE key = ?
	`, schemaMetaKeyMigrated).Scan(&migratedFlag)
	if err == nil && migratedFlag == "1" {
		return &MigrationResult{Skipped: true, Reason: "already migrated (schema_meta marker present)"}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("migrate central: read schema_meta: %w", err)
	}

	// Materialise the row if absent so the subsequent Get is reading
	// a real (possibly default) value rather than hitting ErrNoRows.
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO nexus_settings (id, nexus_md, version)
		VALUES (1, '', 0)
	`); err != nil {
		return nil, fmt.Errorf("migrate central: ensure row: %w", err)
	}

	// Belt-and-suspenders: if nexus_settings is already populated by
	// some prior path that didn't set the marker (legacy / partial
	// upgrade), still skip. Set the marker so future boots take the
	// fast path.
	var existing string
	err = db.QueryRowContext(ctx, `SELECT nexus_md FROM nexus_settings WHERE id = 1`).Scan(&existing)
	if err != nil {
		return nil, fmt.Errorf("migrate central: read existing: %w", err)
	}
	if existing != "" {
		_, _ = db.ExecContext(ctx, `
			INSERT OR REPLACE INTO schema_meta (key, value) VALUES (?, '1')
		`, schemaMetaKeyMigrated)
		return &MigrationResult{Skipped: true, Reason: "nexus_settings already has content"}, nil
	}

	// Seed selection. Prefer keel (or whatever name is configured),
	// then fall back to most-recently-updated.
	seed, seedFrom, err := pickSeedContent(ctx, db, preferredFrame)
	if err != nil {
		return nil, err
	}
	if seed == "" {
		return &MigrationResult{Skipped: true, Reason: "no aspects have nexus_md content"}, nil
	}

	// Apply the seed. Direct UPDATE rather than INSERT-OR-UPDATE
	// because we already INSERTed above. Bumps version 0 → 1 so
	// refresh subscribers see the seed land as a real change.
	if _, err := db.ExecContext(ctx, `
		UPDATE nexus_settings
		SET nexus_md = ?, version = version + 1, updated_at = datetime('now')
		WHERE id = 1
	`, seed); err != nil {
		return nil, fmt.Errorf("migrate central: write seed: %w", err)
	}

	// Persistent marker so future boots skip even if operator blanks
	// central content via the admin endpoint and restarts.
	if _, err := db.ExecContext(ctx, `
		INSERT OR REPLACE INTO schema_meta (key, value) VALUES (?, '1')
	`, schemaMetaKeyMigrated); err != nil {
		return nil, fmt.Errorf("migrate central: mark migrated: %w", err)
	}

	// Surface the divergent aspects for operator awareness.
	divergent, err := listDivergentAspects(ctx, db, seed)
	if err != nil {
		return nil, fmt.Errorf("migrate central: list divergent: %w", err)
	}

	return &MigrationResult{
		SeededFrom:       seedFrom,
		ContentBytes:     len(seed),
		DivergentAspects: divergent,
	}, nil
}

// pickSeedContent returns (content, fromAspect, err). Prefers
// preferredFrame if it has a non-empty nexus_md; falls back to the
// most-recently-updated aspect with non-empty content.
func pickSeedContent(ctx context.Context, db *sql.DB, preferredFrame string) (string, string, error) {
	if preferredFrame != "" {
		var c string
		err := db.QueryRowContext(ctx, `
			SELECT nexus_md FROM aspect_personalities
			WHERE aspect_name = ? AND nexus_md != ''
		`, preferredFrame).Scan(&c)
		if err == nil {
			return c, preferredFrame, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("query preferred: %w", err)
		}
	}

	// Fallback: most-recently-updated row with non-empty nexus_md.
	// rowid DESC is the secondary tiebreaker — when two rows have the
	// same updated_at (which can happen with sub-second writes given
	// SQLite's datetime() resolution is per-second), prefer the
	// most-recently-inserted row. Matches "last writer wins" intent.
	var (
		c    string
		name string
	)
	err := db.QueryRowContext(ctx, `
		SELECT aspect_name, nexus_md FROM aspect_personalities
		WHERE nexus_md != ''
		ORDER BY updated_at DESC, rowid DESC
		LIMIT 1
	`).Scan(&name, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("query fallback: %w", err)
	}
	return c, name, nil
}

// listDivergentAspects returns the names of aspects whose nexus_md
// differs from the seed (and is non-empty). Used to surface manual-
// pruning candidates to the operator post-migration.
func listDivergentAspects(ctx context.Context, db *sql.DB, seed string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT aspect_name FROM aspect_personalities
		WHERE nexus_md != '' AND nexus_md != ?
		ORDER BY aspect_name
	`, seed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
