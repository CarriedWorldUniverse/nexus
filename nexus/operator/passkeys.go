// Package operator implements the operator-identity substrate for the
// dashboard-ws-port spec (2026-05-09). Holds the passkey store and,
// in a follow-up sub-part, the WebAuthn login handler.
//
// Operators are not aspects: they have no on-disk aspect.json, no
// home directory, no autospawn entry. They exist only as live WS
// connections minted from a passkey-unlocked keyfile at login.
// This package is the persistence side of that — registered passkeys
// outlive sessions; sessions don't.
package operator

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PasskeyStore wraps an open *sql.DB and exposes CRUD over the
// operator_passkeys table (schema in nexus/storage/schema.sql).
// Safe for concurrent use — all operations go through *sql.DB which
// handles its own locking.
type PasskeyStore struct {
	db *sql.DB
}

// NewPasskeyStore wraps an already-open *sql.DB. Expects the Nexus
// schema to be in place (storage.Bootstrap has run).
func NewPasskeyStore(db *sql.DB) *PasskeyStore {
	return &PasskeyStore{db: db}
}

// Passkey is a single registered WebAuthn credential.
//
// CredentialID is the raw binary credential id from the authenticator
// (browser passes it as base64url; callers should decode before
// passing in / re-encode on the way out). PublicKey is the COSE-
// encoded public key returned at registration; the verification
// helper in webauthn.go (sub-part 5b) parses it.
//
// SignCount is the authenticator's monotonic replay counter. Stored
// as int64 even though WebAuthn treats it as uint32 — SQLite has no
// unsigned integer; we widen so values approaching 2^31 don't trip
// signedness conversions, and zero stays zero. SaveSignCount enforces
// strict-greater-than semantics; an equal-or-lower value coming back
// from the authenticator is a clone/replay signal.
type Passkey struct {
	ID            int64
	CredentialID  []byte
	PublicKey     []byte
	SignCount     int64
	Label         string
	RegisteredAt  string
	LastUsedAt    string // empty until first successful login
}

// Errors callers can branch on.
var (
	// ErrPasskeyNotFound: the credential id has no row in the table.
	// Distinguished from a database error so the caller can surface
	// the operator-visible "unknown passkey" without leaking internals.
	ErrPasskeyNotFound = errors.New("operator: passkey not found")

	// ErrSignCountReplay: the authenticator presented a sign_count
	// that is not strictly greater than the stored value. WebAuthn
	// spec §6.1.1 — same or lower is a clone/replay signal. Reject
	// the assertion outright.
	ErrSignCountReplay = errors.New("operator: passkey sign_count not strictly increasing (replay or clone signal)")

	// ErrCredentialIDInUse: tried to register a credential id that
	// already has a row. Surfaces during registration, not login.
	ErrCredentialIDInUse = errors.New("operator: credential id already registered")
)

// Register inserts a new passkey row. credentialID + publicKey come
// from the WebAuthn registration ceremony. label is operator-given
// ("<operator-host>", "dMon"). Returns the new row id.
//
// Empty credentialID, publicKey, or label are rejected — these are
// always required. Future: a duplicate credential id surfaces as
// ErrCredentialIDInUse for clean caller branching, instead of a
// raw sqlite UNIQUE-constraint error string.
func (s *PasskeyStore) Register(ctx context.Context, credentialID, publicKey []byte, label string) (int64, error) {
	if len(credentialID) == 0 {
		return 0, errors.New("operator.Register: empty credential_id")
	}
	if len(publicKey) == 0 {
		return 0, errors.New("operator.Register: empty public_key")
	}
	if strings.TrimSpace(label) == "" {
		return 0, errors.New("operator.Register: empty label")
	}

	const q = `
	INSERT INTO operator_passkeys(credential_id, public_key, label)
	VALUES (?, ?, ?)
	RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, q, credentialID, publicKey, label).Scan(&id)
	if err != nil {
		// Map sqlite UNIQUE-constraint violation to a typed error.
		// We don't import the sqlite driver here for the constant —
		// matching on the error string is intentional brittleness so
		// the test harness catches it if the driver renames the
		// message. The alternative (driver-specific import) would
		// pin this package to a specific sqlite library; the string
		// match is the same shape used in nexus/aspects/.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrCredentialIDInUse
		}
		return 0, fmt.Errorf("operator.Register: %w", err)
	}
	return id, nil
}

// GetByCredentialID returns the passkey row for a given credential
// id. ErrPasskeyNotFound when no row exists.
//
// Uses constant-time compare on the credential_id at the SQL layer
// (LIKE/= over a BLOB primary key column is fine — the lookup itself
// is indexed, and the in-Go compare below is just a paranoia check
// that the driver returned what we asked for. WebAuthn credential
// ids are not secrets — they're public client-side identifiers — so
// timing-channel concerns are minimal, but the compare costs nothing.)
func (s *PasskeyStore) GetByCredentialID(ctx context.Context, credentialID []byte) (Passkey, error) {
	if len(credentialID) == 0 {
		return Passkey{}, errors.New("operator.GetByCredentialID: empty credential_id")
	}
	const q = `
	SELECT id, credential_id, public_key, sign_count, label, registered_at,
	       COALESCE(last_used_at, '')
	FROM operator_passkeys
	WHERE credential_id = ?`

	var p Passkey
	err := s.db.QueryRowContext(ctx, q, credentialID).Scan(
		&p.ID, &p.CredentialID, &p.PublicKey, &p.SignCount,
		&p.Label, &p.RegisteredAt, &p.LastUsedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Passkey{}, ErrPasskeyNotFound
		}
		return Passkey{}, fmt.Errorf("operator.GetByCredentialID: %w", err)
	}
	if subtle.ConstantTimeCompare(p.CredentialID, credentialID) != 1 {
		// Should be unreachable — the WHERE clause already matched —
		// but defends against a driver returning a different row by
		// mistake. Failing loud > silently authenticating the wrong
		// credential.
		return Passkey{}, errors.New("operator.GetByCredentialID: credential_id mismatch on read-back")
	}
	return p, nil
}

// SaveSignCount records the new sign_count from a successful login
// and stamps last_used_at to now. Enforces the WebAuthn §6.1.1
// replay rule:
//
//   - If `next` is strictly greater than the stored value: accept,
//     update both columns.
//   - If both stored and `next` are 0: accept, update last_used_at
//     only. Per WebAuthn §6.1.1, a sign_count that stays at 0
//     means the authenticator does not implement the counter
//     (typical for platform authenticators — Touch ID, Face ID,
//     Windows Hello). Treating 0→0 as a replay would lock
//     operators out of their own dashboards.
//   - Otherwise (stored > 0 and next <= stored, or stored > 0 and
//     next == 0 which is a downgrade): reject as ErrSignCountReplay.
//
// The strict-greater branch uses a single SQL statement with the
// comparison in the WHERE clause so concurrent logins can't both
// observe the same prior value and race past each other —
// RowsAffected==0 means the predicate failed (replay or no row).
//
// The 0→0 branch is its own UPDATE that only fires when both the
// stored and presented counters are zero. The two UPDATEs have
// disjoint WHERE clauses, so at most one will land.
func (s *PasskeyStore) SaveSignCount(ctx context.Context, id int64, next int64) error {
	if next < 0 {
		return fmt.Errorf("operator.SaveSignCount: negative next sign_count")
	}

	// Branch 1: strict-greater-than. Atomic via WHERE predicate.
	const qStrict = `
	UPDATE operator_passkeys
	SET sign_count = ?, last_used_at = datetime('now')
	WHERE id = ? AND sign_count < ?`

	res, err := s.db.ExecContext(ctx, qStrict, next, id, next)
	if err != nil {
		return fmt.Errorf("operator.SaveSignCount: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("operator.SaveSignCount rows: %w", err)
	}
	if n == 1 {
		return nil
	}

	// Branch 2: zero-counter authenticator. Only fires when both
	// stored and presented are 0. WebAuthn §6.1.1 says treat as
	// "authenticator does not support counter" — accept and just
	// stamp last_used_at. The WHERE clause guards against accepting
	// a downgrade (stored > 0, next == 0) — that path falls through
	// to the replay return below.
	if next == 0 {
		const qZero = `
		UPDATE operator_passkeys
		SET last_used_at = datetime('now')
		WHERE id = ? AND sign_count = 0`

		res, err := s.db.ExecContext(ctx, qZero, id)
		if err != nil {
			return fmt.Errorf("operator.SaveSignCount zero: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("operator.SaveSignCount zero rows: %w", err)
		}
		if n == 1 {
			return nil
		}
	}

	return ErrSignCountReplay
}

// List returns all registered passkeys, newest registration first.
// Used by the operator-side "manage devices" view (sub-part 5e) and
// by the registration CLI to confirm a successful enrol.
func (s *PasskeyStore) List(ctx context.Context) ([]Passkey, error) {
	const q = `
	SELECT id, credential_id, public_key, sign_count, label, registered_at,
	       COALESCE(last_used_at, '')
	FROM operator_passkeys
	ORDER BY id DESC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("operator.List: %w", err)
	}
	defer rows.Close()

	var out []Passkey
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(
			&p.ID, &p.CredentialID, &p.PublicKey, &p.SignCount,
			&p.Label, &p.RegisteredAt, &p.LastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("operator.List scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a passkey by id. Returns the number of rows
// removed (0 if the id didn't exist; 1 on success). Used by the
// reset-passkey CLI and by future per-device revoke.
func (s *PasskeyStore) Delete(ctx context.Context, id int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM operator_passkeys WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("operator.Delete: %w", err)
	}
	return res.RowsAffected()
}

// DeleteAll wipes every passkey. The reset-passkey CLI calls this
// before re-running registration when the operator has lost all
// devices. There is no recovery path more graceful than this in v1.
func (s *PasskeyStore) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM operator_passkeys`)
	if err != nil {
		return 0, fmt.Errorf("operator.DeleteAll: %w", err)
	}
	return res.RowsAffected()
}
