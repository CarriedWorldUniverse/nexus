// Package identity manages the Nexus's application-layer identity:
// a stable UUID (nexus_id), an Ed25519 server keypair used as the
// crypto_box recipient for aspect keyfiles, and an HMAC secret for
// signing session JWTs.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md
// §3.3 and §14 part 1.
//
// The identity is a single row in the nexus_identity table. Init
// populates it once; subsequent boots load it. Boot fails loud if
// the table is empty — operators must run `nexus identity init`
// explicitly. Don't silently regenerate: nexus_id must be stable
// across restarts so keyfiles minted by this Nexus continue to
// validate (the keyfile envelope embeds nexus_id).
//
// Distinct from PR-A2's `nexus cert init`. That is transport-layer
// (TLS cert + key for HTTPS/WSS). This is application-layer
// (keyfile decryption + JWT signing). Both are required; both
// have their own init subcommand.

package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotInitialized is returned by Load when the nexus_identity row
// is missing. Callers should surface this with a hint pointing at the
// `nexus identity init` subcommand.
var ErrNotInitialized = errors.New("identity: nexus_identity row missing — run `nexus identity init`")

// ErrAlreadyInitialized is returned by Init when the row already
// exists. Use --force to override.
var ErrAlreadyInitialized = errors.New("identity: already initialised — use --force to regenerate (warning: invalidates all existing keyfiles)")

// sessionSecretSize is the byte length of the HMAC-SHA256 signing
// secret for JWTs. 32 bytes is standard for HS256.
const sessionSecretSize = 32

// Identity is the loaded Nexus application-layer identity.
type Identity struct {
	// NexusID is a stable UUID identifying this Nexus instance.
	// Persisted across restarts; never regenerated except by `nexus
	// identity init --force`. Embedded into every keyfile minted by
	// this Nexus so aspects can verify they're talking to the right
	// instance.
	NexusID string

	// ServerPublicKey is the Ed25519 public key. For NaCl
	// crypto_box_seal targeting (keyfile spec §4), the X25519
	// equivalent is what gets used as the seal recipient — derive via
	// the proper edwards25519 → curve25519 mapping, not by slicing.
	ServerPublicKey ed25519.PublicKey

	// ServerPrivateKey is the raw Ed25519 private key (seed || pubkey,
	// 64 bytes per the std library convention).
	//
	// FOOTGUN for Part 4 author: when deriving the X25519 scalar for
	// crypto_box_seal_open decryption, do NOT use ServerPrivateKey[:32]
	// directly. That is the seed, not the clamped scalar. The correct
	// path goes through edwards25519's proper conversion (e.g.
	// filippo.io/edwards25519 SetBytesWithClamping or equivalent).
	// Slicing the seed will silently produce a wrong scalar and
	// decryption of legitimate keyfiles will fail in confusing ways.
	ServerPrivateKey ed25519.PrivateKey

	// SessionSigningSecret is the 32-byte HMAC-SHA256 secret used to
	// sign session JWTs (HS256). Spec §6.
	SessionSigningSecret []byte
}

// Init creates the nexus_identity row. Generates a fresh UUID for
// nexus_id, a fresh Ed25519 keypair for the server, and a fresh
// 32-byte HMAC secret for session JWT signing.
//
// If the row already exists, returns ErrAlreadyInitialized unless
// force is true. Force-init regenerates everything — invalidates all
// keyfiles minted by the previous identity (they no longer decrypt)
// and all in-flight JWTs (signed with the old secret).
//
// Safe to call inside a transaction; the operation is a single INSERT
// (or UPDATE if force).
func Init(ctx context.Context, db *sql.DB, force bool) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate server keypair: %w", err)
	}
	secret := make([]byte, sessionSecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("identity: generate session secret: %w", err)
	}
	id := uuid.NewString()

	if force {
		// Replace the row regardless of prior state.
		_, err = db.ExecContext(ctx, `
			INSERT INTO nexus_identity (id, nexus_id, server_pubkey, server_privkey, session_signing_secret)
			VALUES (1, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				nexus_id = excluded.nexus_id,
				server_pubkey = excluded.server_pubkey,
				server_privkey = excluded.server_privkey,
				session_signing_secret = excluded.session_signing_secret
		`, id, []byte(pub), []byte(priv), secret)
		if err != nil {
			return nil, fmt.Errorf("identity: upsert nexus_identity: %w", err)
		}
		return &Identity{
			NexusID:              id,
			ServerPublicKey:      pub,
			ServerPrivateKey:     priv,
			SessionSigningSecret: secret,
		}, nil
	}

	// Non-force path: insert exactly once. INSERT OR IGNORE is the
	// clean SQLite idiom — single statement, atomic, no race window
	// between SELECT COUNT and INSERT, and constraint violations
	// (whether from id-collision or future schema invariants) all
	// resolve to RowsAffected=0 → ErrAlreadyInitialized rather than
	// surfacing as opaque constraint errors.
	res, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO nexus_identity (id, nexus_id, server_pubkey, server_privkey, session_signing_secret)
		VALUES (1, ?, ?, ?, ?)
	`, id, []byte(pub), []byte(priv), secret)
	if err != nil {
		return nil, fmt.Errorf("identity: insert nexus_identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("identity: rows affected check: %w", err)
	}
	if n == 0 {
		return nil, ErrAlreadyInitialized
	}
	return &Identity{
		NexusID:              id,
		ServerPublicKey:      pub,
		ServerPrivateKey:     priv,
		SessionSigningSecret: secret,
	}, nil
}

// Load reads the nexus_identity row and returns the Identity. Returns
// ErrNotInitialized if the row is absent — boot path should fail loud
// with a hint at the init command.
func Load(ctx context.Context, db *sql.DB) (*Identity, error) {
	row := db.QueryRowContext(ctx, `
		SELECT nexus_id, server_pubkey, server_privkey, session_signing_secret
		FROM nexus_identity WHERE id = 1
	`)
	var id Identity
	var pub, priv, secret []byte
	if err := row.Scan(&id.NexusID, &pub, &priv, &secret); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotInitialized
		}
		return nil, fmt.Errorf("identity: load: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: server_pubkey wrong size: got %d want %d", len(pub), ed25519.PublicKeySize)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: server_privkey wrong size: got %d want %d", len(priv), ed25519.PrivateKeySize)
	}
	if len(secret) != sessionSecretSize {
		return nil, fmt.Errorf("identity: session_signing_secret wrong size: got %d want %d", len(secret), sessionSecretSize)
	}
	id.ServerPublicKey = ed25519.PublicKey(pub)
	id.ServerPrivateKey = ed25519.PrivateKey(priv)
	id.SessionSigningSecret = secret
	return &id, nil
}
