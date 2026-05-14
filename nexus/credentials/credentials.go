// Package credentials manages broker-mediated API credentials for
// external services (Anthropic, OpenAI, DeepSeek, etc.).
//
// Per agent-network/docs (task #218): aspects on remote/unsafe hosts
// need to call third-party APIs requiring keys. Today the key lives on
// the remote host as an env var or file. With this package, the broker
// becomes the credential authority — keys live encrypted in
// provider_credentials, aspects either:
//
//   - Use proxy tools (claude.completion, openai.chat.completion) where
//     the broker holds the key, makes the upstream call, and returns
//     the response. The key never leaves nexus.
//   - Use credential.fetch when proxy isn't available; the credential
//     name and value land in the aspect for the duration of the turn,
//     audit row written.
//
// The credential unit is (name, api_shape, base_url, key, default_model).
// api_shape is the WIRE PROTOCOL ("anthropic", "openai", ...), not the
// vendor. DeepSeek exposes both Anthropic-Messages and OpenAI-Chat-
// Completions endpoints on different base_urls; storing each as a
// separate credential entry lets aspects route protocol-shape-agnostically.
//
// Encryption: AES-256-GCM with a per-row nonce. The data key is derived
// via HKDF from nexus_identity.session_signing_secret using a fixed
// info string ("nexus.credentials.v1"). This doesn't protect against
// "attacker has full DB + nexus_identity access" — that's an OS-level
// concern — but it stops disk snapshots and DB backups from leaking
// keys in cleartext alongside knowledge entries and chat.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// APIShape is the wire-protocol a credential speaks. NOT the vendor.
// DeepSeek can register one credential as ShapeAnthropic and another
// as ShapeOpenAI — same vendor, two protocols.
type APIShape string

const (
	ShapeAnthropic APIShape = "anthropic"
	ShapeOpenAI    APIShape = "openai"
	// Future: ShapeOllama, etc. Add here as additive enum.
)

// Mode declares whether the credential can be plaintext-fetched or
// only used via broker proxy tools. The default is proxy — never let
// the raw key leave nexus unless the operator explicitly opts in.
type Mode string

const (
	ModeProxy Mode = "proxy"
	ModeFetch Mode = "fetch"
	ModeBoth  Mode = "both"
)

// Credential is the full record, including the encrypted key. Returned
// from the store for proxy-call use; callers that just need metadata
// (e.g. listing for the operator) should use Metadata.
type Credential struct {
	Name             string
	Description      string
	APIShape         APIShape
	BaseURL          string
	DefaultModel     string
	AllowedAspects   []string // "*" means all aspects
	Mode             Mode
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastUsedAt       *time.Time
	// EncryptedKey + EncryptionNonce are present on read; Resolve()
	// turns them into the plaintext key. Never JSON-serialise these.
	EncryptedKey    []byte
	EncryptionNonce []byte
}

// Metadata is the same shape minus the encrypted-key material — safe
// to expose in admin REST GET responses without risk of accidentally
// leaking ciphertext.
type Metadata struct {
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	APIShape       APIShape   `json:"api_shape"`
	BaseURL        string     `json:"base_url"`
	DefaultModel   string     `json:"default_model,omitempty"`
	AllowedAspects []string   `json:"allowed_aspects"`
	Mode           Mode       `json:"mode"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

// ToMetadata strips encrypted material from a Credential.
func (c Credential) ToMetadata() Metadata {
	return Metadata{
		Name:           c.Name,
		Description:    c.Description,
		APIShape:       c.APIShape,
		BaseURL:        c.BaseURL,
		DefaultModel:   c.DefaultModel,
		AllowedAspects: c.AllowedAspects,
		Mode:           c.Mode,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
		LastUsedAt:     c.LastUsedAt,
	}
}

// AllowedFor reports whether `aspect` is permitted to use this
// credential. "*" in AllowedAspects matches every aspect.
func (c Credential) AllowedFor(aspect string) bool {
	for _, allowed := range c.AllowedAspects {
		if allowed == "*" || allowed == aspect {
			return true
		}
	}
	return false
}

// UpsertParams carries the inputs for Set — the new credential
// (or update of an existing one). The plaintext Key is encrypted by
// the store; it's never written to disk in cleartext.
type UpsertParams struct {
	Name           string
	Description    string
	APIShape       APIShape
	BaseURL        string
	Key            string // plaintext; encrypted before write
	DefaultModel   string
	AllowedAspects []string
	Mode           Mode
}

// AuditAction enumerates the audit-log action types.
type AuditAction string

const (
	AuditProxyCall      AuditAction = "proxy_call"
	AuditPlaintextFetch AuditAction = "plaintext_fetch"
	AuditDenied         AuditAction = "denied"
)

// AuditEvent is what callers pass to RecordAudit. Details is
// JSON-marshalled to the credential_audit.details column.
type AuditEvent struct {
	CredentialName string
	Aspect         string
	Action         AuditAction
	Details        map[string]any
}

// AuditRow is one row from credential_audit, returned by ListAudit.
type AuditRow struct {
	ID             int64       `json:"id"`
	CredentialName string      `json:"credential_name"`
	Aspect         string      `json:"aspect"`
	Action         AuditAction `json:"action"`
	Timestamp      time.Time   `json:"ts"`
	Details        any         `json:"details"`
}

// Sentinel errors callers may need to dispatch on.
var (
	ErrNotFound       = errors.New("credentials: not found")
	ErrPermission     = errors.New("credentials: aspect not allowed")
	ErrShapeMismatch  = errors.New("credentials: api_shape mismatch")
	ErrInvalidMode    = errors.New("credentials: invalid mode")
	ErrPlaintextDenied = errors.New("credentials: plaintext fetch not allowed for this credential")
)

// Store is the persistent backing for credentials. Implementations
// must be safe for concurrent use.
type Store struct {
	db      *sql.DB
	dataKey []byte // derived once from session_signing_secret via HKDF
}

// NewStore wraps db and derives the encryption data key from the
// nexus identity's session_signing_secret. Failing to derive is
// fatal — callers should propagate the error rather than running
// without encryption.
func NewStore(db *sql.DB, sessionSigningSecret []byte) (*Store, error) {
	if db == nil {
		return nil, errors.New("credentials: nil db")
	}
	if len(sessionSigningSecret) == 0 {
		return nil, errors.New("credentials: empty session signing secret — has nexus identity init run?")
	}
	dataKey, err := deriveDataKey(sessionSigningSecret)
	if err != nil {
		return nil, fmt.Errorf("derive data key: %w", err)
	}
	return &Store{db: db, dataKey: dataKey}, nil
}

// deriveDataKey produces a 32-byte AES-256 key from the session
// signing secret via HKDF-SHA256 with a fixed info string. The info
// string is versioned so a future migration to a different KDF can
// distinguish old from new.
func deriveDataKey(sessionSecret []byte) ([]byte, error) {
	const info = "nexus.credentials.v1"
	// Salt is empty by design — the session secret already has
	// random entropy from identity init. Future versions can change
	// the info string to rotate without changing the secret itself.
	kdf := hkdf.New(sha256.New, sessionSecret, nil, []byte(info))
	key := make([]byte, 32) // AES-256
	if _, err := io.ReadFull(kdf, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Set upserts a credential. The plaintext Key is encrypted and stored;
// the in-memory Credential returned has EncryptedKey/Nonce populated.
func (s *Store) Set(ctx context.Context, p UpsertParams) error {
	if err := validateUpsert(p); err != nil {
		return err
	}
	ciphertext, nonce, err := s.encrypt([]byte(p.Key))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	allowedJSON, err := json.Marshal(p.AllowedAspects)
	if err != nil {
		return fmt.Errorf("marshal allowed_aspects: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_credentials
			(name, description, api_shape, base_url,
			 encrypted_key, encryption_nonce, default_model,
			 allowed_aspects, mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			description       = excluded.description,
			api_shape         = excluded.api_shape,
			base_url          = excluded.base_url,
			encrypted_key     = excluded.encrypted_key,
			encryption_nonce  = excluded.encryption_nonce,
			default_model     = excluded.default_model,
			allowed_aspects   = excluded.allowed_aspects,
			mode              = excluded.mode,
			updated_at        = excluded.updated_at
	`, p.Name, p.Description, string(p.APIShape), p.BaseURL,
		ciphertext, nonce, p.DefaultModel,
		string(allowedJSON), string(p.Mode), now, now)
	if err != nil {
		return fmt.Errorf("upsert credential %q: %w", p.Name, err)
	}
	return nil
}

func validateUpsert(p UpsertParams) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("credentials: name required")
	}
	if p.APIShape != ShapeAnthropic && p.APIShape != ShapeOpenAI {
		return fmt.Errorf("credentials: unsupported api_shape %q (want anthropic|openai)", p.APIShape)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return errors.New("credentials: base_url required")
	}
	if p.Key == "" {
		return errors.New("credentials: key required")
	}
	if p.Mode != ModeProxy && p.Mode != ModeFetch && p.Mode != ModeBoth {
		return fmt.Errorf("%w: %q", ErrInvalidMode, p.Mode)
	}
	if len(p.AllowedAspects) == 0 {
		return errors.New("credentials: allowed_aspects must be non-empty (use [\"*\"] for all)")
	}
	return nil
}

// Get returns the credential including its decrypted-on-demand key
// material (callers use Decrypt to access plaintext). Returns
// ErrNotFound if the name doesn't exist.
func (s *Store) Get(ctx context.Context, name string) (Credential, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, description, api_shape, base_url,
		       encrypted_key, encryption_nonce, default_model,
		       allowed_aspects, mode, created_at, updated_at, last_used_at
		  FROM provider_credentials
		 WHERE name = ?
	`, name)
	var c Credential
	var apiShape, mode string
	var defaultModel sql.NullString
	var allowedJSON string
	var createdAt, updatedAt string
	var lastUsedAt sql.NullString
	if err := row.Scan(&c.Name, &c.Description, &apiShape, &c.BaseURL,
		&c.EncryptedKey, &c.EncryptionNonce, &defaultModel,
		&allowedJSON, &mode, &createdAt, &updatedAt, &lastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, ErrNotFound
		}
		return Credential{}, fmt.Errorf("scan: %w", err)
	}
	c.APIShape = APIShape(apiShape)
	c.Mode = Mode(mode)
	if defaultModel.Valid {
		c.DefaultModel = defaultModel.String
	}
	if err := json.Unmarshal([]byte(allowedJSON), &c.AllowedAspects); err != nil {
		return Credential{}, fmt.Errorf("unmarshal allowed_aspects: %w", err)
	}
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	if lastUsedAt.Valid {
		t := parseTime(lastUsedAt.String)
		c.LastUsedAt = &t
	}
	return c, nil
}

// Decrypt returns the plaintext key for c using the store's data key.
// Callers MUST NOT log or persist the result. Use immediately and
// drop the reference.
func (s *Store) Decrypt(c Credential) (string, error) {
	plaintext, err := s.decrypt(c.EncryptedKey, c.EncryptionNonce)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// EnvForCredential builds the {API_KEY, BASE_URL} env-var pair a
// provider would consume to authenticate against this credential's
// upstream. Per-shape: Anthropic credentials emit ANTHROPIC_API_KEY +
// ANTHROPIC_BASE_URL (URL omitted when empty so the provider falls
// back to its default endpoint); OpenAI credentials emit OPENAI_API_KEY
// + OPENAI_BASE_URL. Future shapes register their own canonical env
// names here.
//
// Returned map is fresh and caller-owned — safe to merge into a
// bridle.TurnRequest.ProviderEnv without copy.
func (s *Store) EnvForCredential(c Credential) (map[string]string, error) {
	key, err := s.Decrypt(c)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, 2)
	switch c.APIShape {
	case ShapeAnthropic:
		env["ANTHROPIC_API_KEY"] = key
		if c.BaseURL != "" {
			env["ANTHROPIC_BASE_URL"] = c.BaseURL
		}
	case ShapeOpenAI:
		env["OPENAI_API_KEY"] = key
		if c.BaseURL != "" {
			env["OPENAI_BASE_URL"] = c.BaseURL
		}
	default:
		// Unknown shape — surface as error rather than silently emit
		// nothing; the funnel's auth would otherwise fall through to
		// whatever process env has, masking misconfiguration.
		return nil, fmt.Errorf("credentials: no env mapping for api_shape %q", c.APIShape)
	}
	return env, nil
}

// ResolveDefaultForAspect looks up the aspect's default credential
// for the given shape via aspects.default_{anthropic,openai}_credential,
// loads the matching credential, verifies the aspect is allowed to use
// it, and returns a ready-to-overlay env map. Returns ErrNoDefault
// when the aspect has no default configured for the requested shape
// (caller can fall through to process-env / --bare-style behaviour).
func (s *Store) ResolveDefaultForAspect(ctx context.Context, aspect string, shape APIShape) (Credential, map[string]string, error) {
	col := ""
	switch shape {
	case ShapeAnthropic:
		col = "default_anthropic_credential"
	case ShapeOpenAI:
		col = "default_openai_credential"
	default:
		return Credential{}, nil, fmt.Errorf("credentials: no default-column mapping for api_shape %q", shape)
	}
	var name sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT `+col+` FROM aspects WHERE name = ?`, aspect).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, nil, ErrNoDefault
		}
		return Credential{}, nil, fmt.Errorf("lookup aspect default: %w", err)
	}
	if !name.Valid || name.String == "" {
		return Credential{}, nil, ErrNoDefault
	}
	c, err := s.Get(ctx, name.String)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("resolve credential %q for aspect %q: %w", name.String, aspect, err)
	}
	if !c.AllowedFor(aspect) {
		return Credential{}, nil, fmt.Errorf("credentials: aspect %q not allowed for credential %q", aspect, name.String)
	}
	env, err := s.EnvForCredential(c)
	if err != nil {
		return Credential{}, nil, err
	}
	return c, env, nil
}

// ErrNoDefault is returned by ResolveDefaultForAspect when the aspect
// has no default credential configured for the requested shape.
// Callers should treat this as "fall through to existing behaviour"
// — usually that means subscription-auth claude-code or process-env
// API keys — not as an error condition.
var ErrNoDefault = errors.New("credentials: aspect has no default for this api shape")

// List returns every credential's metadata. Never includes key material.
func (s *Store) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, description, api_shape, base_url, default_model,
		       allowed_aspects, mode, created_at, updated_at, last_used_at
		  FROM provider_credentials
		 ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metadata
	for rows.Next() {
		var m Metadata
		var apiShape, mode string
		var defaultModel sql.NullString
		var allowedJSON string
		var createdAt, updatedAt string
		var lastUsedAt sql.NullString
		if err := rows.Scan(&m.Name, &m.Description, &apiShape, &m.BaseURL,
			&defaultModel, &allowedJSON, &mode, &createdAt, &updatedAt, &lastUsedAt); err != nil {
			return nil, err
		}
		m.APIShape = APIShape(apiShape)
		m.Mode = Mode(mode)
		if defaultModel.Valid {
			m.DefaultModel = defaultModel.String
		}
		if err := json.Unmarshal([]byte(allowedJSON), &m.AllowedAspects); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(createdAt)
		m.UpdatedAt = parseTime(updatedAt)
		if lastUsedAt.Valid {
			t := parseTime(lastUsedAt.String)
			m.LastUsedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes a credential by name. ON DELETE CASCADE on
// credential_audit means the audit trail goes with it — operator can
// still see the deletion via outer logging, but per-credential audit
// rows are cleaned up to avoid orphan references. If you need
// persistent audit history past delete, copy out before calling.
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM provider_credentials WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastUsed updates last_used_at to now for a credential. Called
// after a successful proxy call or plaintext fetch. Best-effort — a
// failure here is logged but does not fail the underlying operation.
func (s *Store) TouchLastUsed(ctx context.Context, name string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE provider_credentials SET last_used_at = ? WHERE name = ?`, now, name)
	return err
}

// RecordAudit writes a credential_audit row. Failures are returned to
// the caller; the caller decides whether to fail the underlying op
// (typically no — audit is best-effort observability, not a gate).
func (s *Store) RecordAudit(ctx context.Context, ev AuditEvent) error {
	details := "{}"
	if ev.Details != nil {
		b, err := json.Marshal(ev.Details)
		if err != nil {
			return fmt.Errorf("marshal audit details: %w", err)
		}
		details = string(b)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO credential_audit (credential_name, aspect, action, details)
		VALUES (?, ?, ?, ?)
	`, ev.CredentialName, ev.Aspect, string(ev.Action), details)
	return err
}

// ListAudit returns recent audit rows for one credential, newest first.
func (s *Store) ListAudit(ctx context.Context, credentialName string, limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, credential_name, aspect, action, ts, details
		  FROM credential_audit
		 WHERE credential_name = ?
		 ORDER BY id DESC
		 LIMIT ?
	`, credentialName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var action, ts, details string
		if err := rows.Scan(&r.ID, &r.CredentialName, &r.Aspect, &action, &ts, &details); err != nil {
			return nil, err
		}
		r.Action = AuditAction(action)
		r.Timestamp = parseTime(ts)
		var detailsObj any
		if err := json.Unmarshal([]byte(details), &detailsObj); err == nil {
			r.Details = detailsObj
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// encrypt produces ciphertext + nonce for plaintext using AES-256-GCM.
// Each call generates a fresh 96-bit nonce.
func (s *Store) encrypt(plaintext []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(s.dataKey)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// decrypt is the inverse — used by Decrypt above to surface plaintext
// to callers that need the actual key value.
func (s *Store) decrypt(ciphertext, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.dataKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// parseTime parses RFC3339Nano or RFC3339 timestamps. SQLite's
// datetime() defaults to RFC3339-without-fractional; our Set writes
// with nanos. Both must round-trip cleanly.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
