// Package aspects provides DB-resident aspect registry and personality
// storage. Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md
// §3.1, §3.2, §14 part 2.
//
// The two tables (`aspects` + `aspect_personalities`) replace the on-disk
// aspect.json + CLAUDE.md/SOUL.md/PRIMER.md model. Aspects + their
// personalities live in nexus.db; agentfunnel hosts receive them at
// startup via the keyfile validation handshake.
//
// This package exposes:
//
//   - Aspect / Personality value types
//   - Status / StatusActive / StatusRetired enum
//   - Store interface with Get/List/Insert/Update/Bump/SetStatus and
//     PersonalityGet/PersonalitySet helpers
//   - SQLStore implementation backed by *sql.DB
//
// Higher-layer subcommands (mint, retire, resurrect, list, status,
// migrate, personality edit) consume Store. The validation endpoint
// (Part 4) consumes Store to authorize keyfile blobs.

package aspects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned by Get / PersonalityGet when no row matches.
var ErrNotFound = errors.New("aspects: not found")

// Status enumerates aspect lifecycle states. Mirrors the CHECK
// constraint in schema.sql — keep in sync.
type Status string

const (
	// StatusActive means the aspect can be minted, validated, and
	// connected.
	StatusActive Status = "active"

	// StatusRetired means all keyfiles are permanently dead. Mint
	// refused. Use Resurrect (sets back to active + bumps version)
	// to revive.
	StatusRetired Status = "retired"
)

// Aspect is the registry row. Timestamps are stored as ISO 8601 strings
// (datetime('now') in SQLite) — kept as strings here to avoid driver
// coupling. Callers that need time.Time can parse with time.Parse.
type Aspect struct {
	Name                  string
	Status                Status
	CurrentKeyfileVersion int64
	AspectPubkey          []byte // 32-byte Ed25519 public key
	Provider              string
	Model                 string
	Capabilities          string // JSON array, opaque
	Metadata              string // JSON object, opaque
	CreatedAt             string // ISO 8601 from datetime('now')
	UpdatedAt             string
}

// Personality is the loaded aspect_personalities row.
type Personality struct {
	AspectName string
	NexusMD    string
	SoulMD     string
	PrimerMD   string
	Composed   string // cached assembled SystemPrompt; may be "" if invalidated
	Version    int64
	UpdatedAt  string // ISO 8601
}

// Store is the public read/write interface for the aspects + personality
// tables. Implementations must be safe for concurrent use.
type Store interface {
	// Get returns the aspect row by name. Returns ErrNotFound if absent.
	Get(ctx context.Context, name string) (*Aspect, error)

	// List returns all aspect rows ordered by name.
	List(ctx context.Context) ([]Aspect, error)

	// Insert creates a new aspect. Caller is responsible for ensuring
	// `name` doesn't already exist; Insert returns an error on
	// duplicate (unique constraint violation).
	Insert(ctx context.Context, a Aspect) error

	// Update modifies an existing aspect. Touches updated_at. Returns
	// ErrNotFound if no row matches.
	Update(ctx context.Context, a Aspect) error

	// BumpKeyfileVersion atomically increments current_keyfile_version
	// for the named aspect AND replaces aspect_pubkey with the new key.
	// Used by `nexus aspect mint` on re-mint. Returns the new version
	// for caller use.
	BumpKeyfileVersion(ctx context.Context, name string, newPubkey []byte) (int64, error)

	// SetStatus atomically updates status. Used by retire.
	SetStatus(ctx context.Context, name string, status Status) error

	// Resurrect atomically transitions status retired→active AND bumps
	// current_keyfile_version with a fresh placeholder pubkey. Single
	// transaction so an old keyfile can never momentarily re-validate
	// between the two writes. Returns the new version. Returns
	// ErrNotFound if the aspect doesn't exist.
	//
	// Caller is responsible for running `aspect mint` immediately
	// afterwards to replace the placeholder with a real keypair.
	Resurrect(ctx context.Context, name string, placeholderPubkey []byte) (int64, error)

	// PersonalityGet returns the personality row for an aspect.
	// Returns ErrNotFound if no row exists (note: a row with empty
	// strings is "exists, just blank" — different from absent).
	PersonalityGet(ctx context.Context, aspectName string) (*Personality, error)

	// PersonalitySet upserts a personality row. Bumps version on any
	// content change. The composed field is invalidated (set to "")
	// on every write — readers recompute on demand.
	PersonalitySet(ctx context.Context, p Personality) error
}

// SQLStore is the *sql.DB-backed implementation of Store.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore wraps a *sql.DB.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// DBForTest exposes the underlying *sql.DB for tests in other packages
// that need to construct a sibling store sharing the same connection
// (e.g. SQLSettingsStore in Part 9 frame tests). Not for production use.
func (s *SQLStore) DBForTest() *sql.DB {
	return s.db
}

// scanAspect populates an Aspect from a *sql.Row or *sql.Rows.
func scanAspect(scan func(...any) error) (*Aspect, error) {
	var a Aspect
	var status string
	var caps, meta sql.NullString
	if err := scan(
		&a.Name, &status, &a.CurrentKeyfileVersion, &a.AspectPubkey,
		&a.Provider, &a.Model, &caps, &meta, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.Status = Status(status)
	if caps.Valid {
		a.Capabilities = caps.String
	}
	if meta.Valid {
		a.Metadata = meta.String
	}
	return &a, nil
}

const aspectColumns = `name, status, current_keyfile_version, aspect_pubkey,
		provider, model, capabilities, metadata, created_at, updated_at`

// Get implements Store.
func (s *SQLStore) Get(ctx context.Context, name string) (*Aspect, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+aspectColumns+` FROM aspects WHERE name = ?
	`, name)
	a, err := scanAspect(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("aspects.Get(%q): %w", name, err)
	}
	return a, nil
}

// List implements Store.
func (s *SQLStore) List(ctx context.Context) ([]Aspect, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+aspectColumns+` FROM aspects ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("aspects.List: %w", err)
	}
	defer rows.Close()

	var out []Aspect
	for rows.Next() {
		a, err := scanAspect(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("aspects.List scan: %w", err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aspects.List rows: %w", err)
	}
	return out, nil
}

// Insert implements Store.
func (s *SQLStore) Insert(ctx context.Context, a Aspect) error {
	if a.Status == "" {
		a.Status = StatusActive
	}
	if a.CurrentKeyfileVersion == 0 {
		a.CurrentKeyfileVersion = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO aspects
			(name, status, current_keyfile_version, aspect_pubkey,
			 provider, model, capabilities, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, a.Name, string(a.Status), a.CurrentKeyfileVersion, a.AspectPubkey,
		a.Provider, a.Model, nullIfEmpty(a.Capabilities), nullIfEmpty(a.Metadata))
	if err != nil {
		return fmt.Errorf("aspects.Insert(%q): %w", a.Name, err)
	}
	return nil
}

// Update implements Store. Updates everything except name and timestamps.
func (s *SQLStore) Update(ctx context.Context, a Aspect) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE aspects SET
			status = ?, current_keyfile_version = ?, aspect_pubkey = ?,
			provider = ?, model = ?, capabilities = ?, metadata = ?,
			updated_at = datetime('now')
		WHERE name = ?
	`, string(a.Status), a.CurrentKeyfileVersion, a.AspectPubkey,
		a.Provider, a.Model, nullIfEmpty(a.Capabilities), nullIfEmpty(a.Metadata),
		a.Name)
	if err != nil {
		return fmt.Errorf("aspects.Update(%q): %w", a.Name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("aspects.Update(%q) rows affected: %w", a.Name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// BumpKeyfileVersion implements Store. Uses RETURNING (SQLite 3.35+)
// to atomically increment + read the new value.
func (s *SQLStore) BumpKeyfileVersion(ctx context.Context, name string, newPubkey []byte) (int64, error) {
	var newVersion int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE aspects
		SET current_keyfile_version = current_keyfile_version + 1,
		    aspect_pubkey = ?,
		    updated_at = datetime('now')
		WHERE name = ?
		RETURNING current_keyfile_version
	`, newPubkey, name).Scan(&newVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("aspects.BumpKeyfileVersion(%q): %w", name, err)
	}
	return newVersion, nil
}

// SetStatus implements Store.
func (s *SQLStore) SetStatus(ctx context.Context, name string, status Status) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE aspects SET status = ?, updated_at = datetime('now')
		WHERE name = ?
	`, string(status), name)
	if err != nil {
		return fmt.Errorf("aspects.SetStatus(%q, %q): %w", name, status, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("aspects.SetStatus(%q) rows affected: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Resurrect implements Store. Atomic retired→active + version bump
// + pubkey replacement in a single transaction. Returns ErrNotFound
// if the row is missing or not retired (so resurrect-on-active is a
// noop instead of a silent state-change).
func (s *SQLStore) Resurrect(ctx context.Context, name string, placeholderPubkey []byte) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("aspects.Resurrect(%q): begin: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort after commit

	// Single UPDATE that touches status + version + pubkey. Guarded by
	// status='retired' so resurrecting an already-active row is a noop
	// (zero rows affected → ErrNotFound for the caller).
	var newVersion int64
	err = tx.QueryRowContext(ctx, `
		UPDATE aspects
		SET status = 'active',
		    current_keyfile_version = current_keyfile_version + 1,
		    aspect_pubkey = ?,
		    updated_at = datetime('now')
		WHERE name = ? AND status = 'retired'
		RETURNING current_keyfile_version
	`, placeholderPubkey, name).Scan(&newVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("aspects.Resurrect(%q): %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("aspects.Resurrect(%q): commit: %w", name, err)
	}
	return newVersion, nil
}

// PersonalityGet implements Store.
func (s *SQLStore) PersonalityGet(ctx context.Context, aspectName string) (*Personality, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT aspect_name, nexus_md, soul_md, primer_md, composed, version, updated_at
		FROM aspect_personalities WHERE aspect_name = ?
	`, aspectName)
	var p Personality
	if err := row.Scan(&p.AspectName, &p.NexusMD, &p.SoulMD, &p.PrimerMD, &p.Composed, &p.Version, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("aspects.PersonalityGet(%q): %w", aspectName, err)
	}
	return &p, nil
}

// PersonalitySet implements Store. Upserts; on update, bumps version
// and invalidates the composed cache (next reader recomputes).
func (s *SQLStore) PersonalitySet(ctx context.Context, p Personality) error {
	// INSERT OR ... ON CONFLICT bumps version + clears composed on
	// every write so writes are idempotent for "set" semantics + the
	// version always reflects the most recent edit.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO aspect_personalities (aspect_name, nexus_md, soul_md, primer_md, composed, version)
		VALUES (?, ?, ?, ?, '', 1)
		ON CONFLICT(aspect_name) DO UPDATE SET
			nexus_md   = excluded.nexus_md,
			soul_md    = excluded.soul_md,
			primer_md  = excluded.primer_md,
			composed   = '',
			version    = aspect_personalities.version + 1,
			updated_at = datetime('now')
	`, p.AspectName, p.NexusMD, p.SoulMD, p.PrimerMD)
	if err != nil {
		return fmt.Errorf("aspects.PersonalitySet(%q): %w", p.AspectName, err)
	}
	return nil
}

// nullIfEmpty wraps a string in sql.NullString if non-empty, returning
// nil otherwise. SQLite stores NULL for the absent fields rather than
// empty strings, which matches the schema's nullable columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
