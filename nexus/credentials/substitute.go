// Substitute resolves ${credential:NAME.field} placeholders in an MCP
// profile blob (NEX-168). Each placeholder is split last-dot-wins so
// credential names containing dots (e.g. github.nexus-anvil) work
// without escaping: name is everything before the last dot, field is
// everything after.
//
// Resolution rules (from shadow's spec):
//   - No dot in placeholder body → ErrSubstituteMalformed.
//   - Empty name or empty field → ErrSubstituteMalformed.
//   - Unclosed ${credential: → ErrSubstituteMalformed.
//   - Unknown credential name → ErrSubstituteUnknownCredential.
//   - Field absent from credential bundle → ErrSubstituteUnknownField.
//
// Audit: one credential_audit row per RESOLVED reference (not per call).
// action='fetch', details={via:"mcp_profile_substitute",
// profile_aspect:<aspect>, credential:<name>, field:<field>}.
//
// Atomicity: if any reference fails, NO audit rows are written and the
// returned string is empty. Implementation does two passes — first
// resolves every reference to validate, then writes audit rows in a
// single transaction and returns the rendered output. A partial render
// would yield a half-substituted profile, which is worse than failing
// loudly.

package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Substitution sentinel errors. Callers can distinguish:
//   - malformed placeholder (operator typo in the profile)
//   - unknown credential (operator referenced a credential that doesn't exist)
//   - unknown field (operator referenced a field the credential doesn't have)
var (
	ErrSubstituteMalformed         = errors.New("credentials: malformed credential placeholder")
	ErrSubstituteUnknownCredential = errors.New("credentials: unknown credential in placeholder")
	ErrSubstituteUnknownField      = errors.New("credentials: unknown field in credential bundle")
)

// placeholderPrefix is the literal token that opens a credential
// reference. Kept as a constant so any future syntax change has a
// single source of truth.
const placeholderPrefix = "${credential:"

// resolvedRef is one parsed-and-resolved placeholder.
type resolvedRef struct {
	start, end int    // byte range in the input string (end is one past the closing brace)
	credName   string // credential name (left of last dot)
	field      string // field name (right of last dot)
	value      string // resolved plaintext value
}

// Substitute renders profile by replacing ${credential:NAME.field}
// references with the corresponding bundle field values. Aspect is the
// profile owner — recorded on each audit row as profile_aspect.
//
// Returns the rendered string, or an error and an empty string if any
// reference fails to resolve. No audit row is written on failure.
func (s *Store) Substitute(ctx context.Context, aspect, profile string) (string, error) {
	// Pass 1: parse every placeholder, resolve each, collect into refs.
	// Bail on the first error — no audit row is written until pass 2.
	refs, err := s.resolveAllPlaceholders(ctx, profile)
	if err != nil {
		return "", err
	}

	// No references → fast path, no transaction needed.
	if len(refs) == 0 {
		return profile, nil
	}

	// Pass 2: render + audit in a single transaction. If anything goes
	// wrong with the audit writes, roll back so we don't leave a
	// half-audited render in place.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("substitute: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, r := range refs {
		if err := s.recordSubstituteAudit(ctx, tx, aspect, r.credName, r.field); err != nil {
			return "", fmt.Errorf("substitute: record audit for %q.%q: %w", r.credName, r.field, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("substitute: commit audit tx: %w", err)
	}

	return renderRefs(profile, refs), nil
}

// resolveAllPlaceholders scans profile for ${credential:...} placeholders,
// validates each, resolves the credential bundle, and pulls the named
// field. Returns the parsed refs in source order so the renderer can
// splice values back in left-to-right.
func (s *Store) resolveAllPlaceholders(ctx context.Context, profile string) ([]resolvedRef, error) {
	// Cache decoded bundles by credential name so a profile that
	// references the same credential multiple times doesn't re-decrypt
	// per reference.
	bundleCache := map[string]map[string]any{}

	var refs []resolvedRef
	idx := 0
	for {
		open := strings.Index(profile[idx:], placeholderPrefix)
		if open < 0 {
			break
		}
		open += idx
		bodyStart := open + len(placeholderPrefix)
		closeRel := strings.Index(profile[bodyStart:], "}")
		if closeRel < 0 {
			return nil, fmt.Errorf("%w: unclosed ${credential: starting at offset %d", ErrSubstituteMalformed, open)
		}
		bodyEnd := bodyStart + closeRel
		body := profile[bodyStart:bodyEnd]

		// Placeholders sit inside JSON string values. A `"` in the body
		// means we ran past the string boundary without seeing a closing
		// brace — treat that as unclosed/malformed so the operator sees a
		// clear error rather than the parser greedily consuming subsequent
		// JSON structure.
		if strings.ContainsAny(body, "\"\n") {
			return nil, fmt.Errorf("%w: placeholder at offset %d not closed before string/line end", ErrSubstituteMalformed, open)
		}

		credName, field, err := parsePlaceholderBody(body)
		if err != nil {
			return nil, err
		}

		bundle, cached := bundleCache[credName]
		if !cached {
			c, gerr := s.Get(ctx, credName)
			if errors.Is(gerr, ErrNotFound) {
				return nil, fmt.Errorf("%w: %q", ErrSubstituteUnknownCredential, credName)
			}
			if gerr != nil {
				return nil, fmt.Errorf("substitute: load credential %q: %w", credName, gerr)
			}
			b, berr := s.Bundle(c)
			if berr != nil {
				return nil, fmt.Errorf("substitute: decrypt bundle %q: %w", credName, berr)
			}
			bundle = b
			bundleCache[credName] = b
		}

		raw, ok := bundle[field]
		if !ok {
			return nil, fmt.Errorf("%w: credential %q has no field %q", ErrSubstituteUnknownField, credName, field)
		}
		strVal, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: credential %q field %q is not a string (got %T)", ErrSubstituteUnknownField, credName, field, raw)
		}

		refs = append(refs, resolvedRef{
			start:    open,
			end:      bodyEnd + 1, // past closing '}'
			credName: credName,
			field:    field,
			value:    strVal,
		})
		idx = bodyEnd + 1
	}
	return refs, nil
}

// parsePlaceholderBody splits the body of a ${credential:BODY}
// placeholder into (credentialName, field). Last-dot-wins so dotted
// credential names like github.nexus-anvil work without escaping.
func parsePlaceholderBody(body string) (string, string, error) {
	if body == "" {
		return "", "", fmt.Errorf("%w: empty placeholder body", ErrSubstituteMalformed)
	}
	dot := strings.LastIndex(body, ".")
	if dot < 0 {
		return "", "", fmt.Errorf("%w: placeholder %q missing required .field", ErrSubstituteMalformed, body)
	}
	name := body[:dot]
	field := body[dot+1:]
	if name == "" {
		return "", "", fmt.Errorf("%w: placeholder %q has empty credential name", ErrSubstituteMalformed, body)
	}
	if field == "" {
		return "", "", fmt.Errorf("%w: placeholder %q has empty field name", ErrSubstituteMalformed, body)
	}
	return name, field, nil
}

// renderRefs splices each resolved value into profile at the placeholder's
// byte range. refs are in source order, so building from left to right
// is safe.
func renderRefs(profile string, refs []resolvedRef) string {
	var b strings.Builder
	b.Grow(len(profile))
	cursor := 0
	for _, r := range refs {
		b.WriteString(profile[cursor:r.start])
		b.WriteString(r.value)
		cursor = r.end
	}
	b.WriteString(profile[cursor:])
	return b.String()
}

// recordSubstituteAudit writes one credential_audit row inside tx for a
// resolved substitution reference. Mirrors Store.RecordAudit but takes
// the tx so all rows commit atomically with the render.
func (s *Store) recordSubstituteAudit(ctx context.Context, tx *sql.Tx, aspect, credName, field string) error {
	details, err := json.Marshal(map[string]any{
		"via":            "mcp_profile_substitute",
		"profile_aspect": aspect,
		"credential":     credName,
		"field":          field,
	})
	if err != nil {
		return fmt.Errorf("marshal audit details: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credential_audit (credential_name, aspect, action, details)
		VALUES (?, ?, ?, ?)
	`, credName, aspect, string(AuditFetch), string(details))
	return err
}
