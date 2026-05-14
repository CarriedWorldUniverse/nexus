package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// MigrateLegacyTable handles the one-time data move from the pre-NEX-75
// `provider_credentials` table into the post-NEX-75 `credentials` table.
//
// Why this lives here rather than in storage.Bootstrap: rows need to be
// re-encrypted from the old shape (encrypted KEY only, with api_shape/
// base_url/default_model in cleartext columns) to the new shape
// (encrypted BUNDLE — a JSON object packing key + api_shape + base_url
// + default_model together). Re-encryption requires the data key,
// which only exists on Store; that's an import cycle if storage tried
// to do it. Caller (cmd/nexus/main.go) invokes this once after
// NewStore + after storage.Bootstrap.
//
// Idempotent + safe to call on every boot:
//   - If `provider_credentials` doesn't exist → no-op.
//   - If `credentials` is non-empty → no-op (migration already ran;
//     don't double-migrate and clobber post-migration upserts).
//   - Otherwise: decrypt each old row's key, build a provider-bundle
//     JSON, re-encrypt as the new shape, insert into `credentials`,
//     and DROP the old table once every row migrated cleanly.
//
// Atomicity: all of this runs in a single transaction. If anything fails
// (decrypt error on a row, insert conflict, anything), the transaction
// rolls back and the old table stays intact. Operator sees the failure
// in startup logs and can investigate without data loss.
func (s *Store) MigrateLegacyTable(ctx context.Context) error {
	exists, err := tableExists(ctx, s.db, "provider_credentials")
	if err != nil {
		return fmt.Errorf("check provider_credentials existence: %w", err)
	}
	if !exists {
		// Fresh DB or already-migrated (table dropped). No-op.
		return nil
	}

	// Bail if the destination already has rows. We don't want to merge —
	// the operator may have re-created an entry under the same name with
	// a different key after the schema rename, and clobbering that with
	// the legacy version would silently lose work.
	var destCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM credentials`).Scan(&destCount); err != nil {
		return fmt.Errorf("count credentials: %w", err)
	}
	if destCount > 0 {
		// Destination populated. Don't migrate — but DO drop the legacy
		// table if it's now redundant (i.e. its names are a subset of
		// the destination). Otherwise leave both in place and surface a
		// log line for the operator to investigate manually.
		var legacyOnly int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM provider_credentials
			 WHERE name NOT IN (SELECT name FROM credentials)
		`).Scan(&legacyOnly)
		if err != nil {
			return fmt.Errorf("count legacy-only names: %w", err)
		}
		if legacyOnly == 0 {
			if _, err := s.db.ExecContext(ctx, `DROP TABLE provider_credentials`); err != nil {
				return fmt.Errorf("drop legacy table after destination-already-populated check: %w", err)
			}
		}
		// Otherwise we leave provider_credentials in place — operator
		// has both tables to reconcile. Not our call to resolve silently.
		return nil
	}

	// Source has rows, destination empty. Run the migration in a tx.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op if Commit succeeded. Always safe.
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(ctx, `
		SELECT name, description, api_shape, base_url,
		       encrypted_key, encryption_nonce, default_model,
		       allowed_aspects, mode, created_at, updated_at, last_used_at
		  FROM provider_credentials
	`)
	if err != nil {
		return fmt.Errorf("query legacy rows: %w", err)
	}

	type legacyRow struct {
		name, description, apiShape, baseURL string
		encKey, nonce                        []byte
		defaultModel                         sql.NullString
		allowedJSON, mode                    string
		createdAt, updatedAt                 string
		lastUsedAt                           sql.NullString
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.name, &r.description, &r.apiShape, &r.baseURL,
			&r.encKey, &r.nonce, &r.defaultModel,
			&r.allowedJSON, &r.mode, &r.createdAt, &r.updatedAt, &r.lastUsedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy row: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy rows: %w", err)
	}

	for _, r := range legacy {
		// Decrypt the legacy key using this store's data key (same KDF
		// info string, so cross-compatible).
		plaintextKey, err := s.decrypt(r.encKey, r.nonce)
		if err != nil {
			return fmt.Errorf("decrypt legacy row %q: %w", r.name, err)
		}

		// Build the new-shape provider bundle.
		bundle := map[string]any{
			"api_shape": r.apiShape,
			"base_url":  r.baseURL,
			"key":       string(plaintextKey),
		}
		if r.defaultModel.Valid && r.defaultModel.String != "" {
			bundle["default_model"] = r.defaultModel.String
		}

		// Re-encrypt as a single bundle blob with a fresh nonce.
		ciphertext, newNonce, err := s.encryptJSON(bundle)
		if err != nil {
			return fmt.Errorf("re-encrypt bundle for %q: %w", r.name, err)
		}

		// Insert into the new table preserving created_at / updated_at /
		// last_used_at. Skip ON CONFLICT — destination is empty per the
		// guard above, so a conflict here is a programming error.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO credentials
				(name, description, kind,
				 encrypted_bundle, encryption_nonce,
				 allowed_aspects, mode, created_at, updated_at, last_used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, r.name, r.description, string(KindProvider),
			ciphertext, newNonce,
			r.allowedJSON, r.mode,
			r.createdAt, r.updatedAt,
			r.lastUsedAt)
		if err != nil {
			return fmt.Errorf("insert migrated row %q: %w", r.name, err)
		}
	}

	// All rows migrated; drop the legacy table. The FK on credential_audit
	// was rewritten in schema.sql to point at the new `credentials` table,
	// so audit rows survive the rename transparently.
	if _, err := tx.ExecContext(ctx, `DROP TABLE provider_credentials`); err != nil {
		return fmt.Errorf("drop legacy table after migration: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	return nil
}

// encryptJSON is a convenience around encrypt for callers passing a
// map (or other JSON-serialisable value).
func (s *Store) encryptJSON(v any) ([]byte, []byte, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal: %w", err)
	}
	return s.encrypt(plaintext)
}

// tableExists checks whether `name` is a table in the connected DB.
// Returns (false, nil) if absent (NOT an error).
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx, `
		SELECT name FROM sqlite_master
		 WHERE type='table' AND name = ?
	`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

