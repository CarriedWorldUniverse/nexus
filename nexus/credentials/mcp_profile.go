// MCP profile CRUD (NEX-168). The mcp_profiles table holds one row per
// aspect: a JSON blob describing the aspect's MCP-server configuration
// with credential references left as placeholders. The store keeps the
// blob opaque — shape validation lives at the agent funnel, not here.
//
// Substitution (Store.Substitute) reads the profile, resolves
// ${credential:NAME.field} placeholders against the credentials table,
// and writes a credential_audit row per resolved reference. The
// rendered profile is never persisted — re-rendering on every fetch
// keeps secrets out of this table.

package credentials

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetMCPProfile returns the stored profile JSON blob for an aspect, or
// the empty string when no row exists. Absent and empty-profile are
// treated identically by callers — there's no semantic difference
// between "aspect was never configured" and "aspect has an empty
// profile" at the wire layer.
func (s *Store) GetMCPProfile(ctx context.Context, aspect string) (string, error) {
	var profile string
	err := s.db.QueryRowContext(ctx,
		`SELECT profile FROM mcp_profiles WHERE aspect_name = ?`, aspect,
	).Scan(&profile)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get mcp profile %q: %w", aspect, err)
	}
	return profile, nil
}

// SetMCPProfile upserts the profile JSON blob for an aspect. The blob is
// stored verbatim — caller is responsible for ensuring it's valid JSON
// (the admin handler does this check before calling Set).
func (s *Store) SetMCPProfile(ctx context.Context, aspect, profile string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_profiles (aspect_name, profile, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(aspect_name) DO UPDATE SET
			profile    = excluded.profile,
			updated_at = excluded.updated_at
	`, aspect, profile, now)
	if err != nil {
		return fmt.Errorf("set mcp profile %q: %w", aspect, err)
	}
	return nil
}
