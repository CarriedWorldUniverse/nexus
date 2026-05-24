// Package credentials manages broker-mediated credentials for external
// services that aspects need to call (Anthropic, OpenAI, DeepSeek,
// Jira, IMAP, future kinds).
//
// Per agent-network/docs (task #218) + epic #NEX-74/75: aspects on
// remote/unsafe hosts need third-party creds. Broker is the credential
// authority — creds live encrypted in the `credentials` table; aspects
// fetch via WS frame on demand or use proxy tools (claude.completion,
// openai.chat.completion), and the key never persists on remote disks.
//
// The credential unit is `(name, kind, bundle, mode, allowed_aspects)`.
// `kind` is the credential class — `provider` (Anthropic/OpenAI APIs),
// `jira` (Atlassian creds), `imap` (mailbox), future kinds. `bundle` is
// a per-kind JSON object encrypted at rest:
//
//	kind='provider'  → {"api_shape":"anthropic|openai", "base_url":"...", "key":"...", "default_model":"..."}
//	kind='jira'      → {"atlassian_email":"...", "atlassian_token":"...", "atlassian_subdomain":"..."}
//	kind='imap'      → {"host":"...", "port":993, "user":"...", "password":"...", "ssl":true}
//
// Adding a new kind = adding a bundle-validator entry; no schema change.
//
// Encryption: AES-256-GCM with per-row nonce. Data key derived via
// HKDF-SHA256 from nexus_identity.session_signing_secret using the
// fixed info string "nexus.credentials.v1". The same info string the
// pre-NEX-75 schema used, so existing data keys are compatible across
// the migration. Doesn't protect against full-DB-plus-nexus_identity
// compromise — that's an OS concern — but stops disk snapshots and
// backups from leaking creds in cleartext alongside knowledge entries
// and chat.
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

// Kind is the credential class. NOT the vendor; it's the shape of the
// bundle stored under that name.
type Kind string

const (
	KindProvider Kind = "provider" // Anthropic/OpenAI-shape API creds
	KindJira     Kind = "jira"     // Atlassian email/token/subdomain
	KindIMAP     Kind = "imap"     // Mailbox host/port/user/password/ssl
	// Future kinds register here as the bundle validators are added.
)

// IsKnownKind reports whether k is a kind this package validates.
// Unknown kinds are rejected at upsert time to prevent misnamed entries
// silently working as opaque blobs.
func IsKnownKind(k Kind) bool {
	switch k {
	case KindProvider, KindJira, KindIMAP:
		return true
	default:
		return false
	}
}

// APIShape is the wire-protocol a *provider*-kind credential speaks.
// Lives inside the provider bundle, not as its own column. Kept as a
// typed const so callers branching on shape (e.g. funnel env mapping)
// don't pass raw strings.
type APIShape string

const (
	ShapeAnthropic APIShape = "anthropic"
	ShapeOpenAI    APIShape = "openai"
	// Future: ShapeOllama, etc.
)

// Mode declares whether the credential can be plaintext-fetched or
// only used via broker proxy tools.
//
//   - ModeProxy: the broker holds the key, makes the upstream call,
//     returns the response. The aspect never sees the key.
//   - ModeFetch: plaintext fetch is allowed; the aspect receives the
//     bundle and uses it directly. Audit row written per fetch.
//   - ModeBoth: either path. Operator chooses per-call.
//
// Non-provider kinds (jira/imap) have no broker-side proxy yet, so
// they're inherently fetch-shape. Set ModeFetch or ModeBoth for those.
type Mode string

const (
	ModeProxy Mode = "proxy"
	ModeFetch Mode = "fetch"
	ModeBoth  Mode = "both"
)

// Credential is one row from `credentials`, with the bundle decrypted
// on access via the Bundle() / Decrypt*() helpers. Never serialise
// the raw bundle map directly — use ToMetadata() for safe public exposure.
type Credential struct {
	Name           string
	Description    string
	Kind           Kind
	AllowedAspects []string // "*" means all aspects
	Mode           Mode
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastUsedAt     *time.Time
	// encryptedBundle + nonce are the AES-256-GCM ciphertext + nonce of
	// the kind-specific JSON bundle. Decrypt with the store's data key
	// via Bundle() / ProviderEnv() / etc.
	encryptedBundle []byte
	nonce           []byte
}

// Metadata is the public-safe shape — strips the encrypted material.
// Use for admin REST GET responses, list views, etc.
type Metadata struct {
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Kind           Kind       `json:"kind"`
	AllowedAspects []string   `json:"allowed_aspects"`
	Mode           Mode       `json:"mode"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

// ToMetadata strips encrypted material from a Credential. Safe to JSON
// serialise.
func (c Credential) ToMetadata() Metadata {
	return Metadata{
		Name:           c.Name,
		Description:    c.Description,
		Kind:           c.Kind,
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

// ProviderBundle is the parsed shape of a kind='provider' bundle.
// Unmarshalled by ProviderBundle() helper after decrypting the bundle.
type ProviderBundle struct {
	APIShape     APIShape `json:"api_shape"`
	BaseURL      string   `json:"base_url"`
	Key          string   `json:"key"`
	DefaultModel string   `json:"default_model,omitempty"`
}

// JiraBundle is the parsed shape of a kind='jira' bundle.
type JiraBundle struct {
	Email     string `json:"atlassian_email"`
	Token     string `json:"atlassian_token"`
	Subdomain string `json:"atlassian_subdomain"`
}

// IMAPBundle is the parsed shape of a kind='imap' bundle.
type IMAPBundle struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSL      bool   `json:"ssl"`
}

// UpsertParams carries the inputs for Set. Bundle is the kind-specific
// plaintext payload (will be JSON-marshalled then encrypted). It's
// `map[string]any` rather than the typed structs above so the handler
// layer (admin REST) can stay generic; per-kind validation runs in
// validateBundle.
type UpsertParams struct {
	Name           string
	Description    string
	Kind           Kind
	Bundle         map[string]any
	AllowedAspects []string
	Mode           Mode
}

// AuditAction enumerates the audit-log action types.
type AuditAction string

const (
	AuditProxyCall      AuditAction = "proxy_call"
	AuditPlaintextFetch AuditAction = "plaintext_fetch"
	AuditFetch          AuditAction = "fetch" // WS frame credential.fetch
	AuditDenied         AuditAction = "denied"
)

// AuditEvent is what callers pass to RecordAudit.
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
	ErrNotFound        = errors.New("credentials: not found")
	ErrPermission      = errors.New("credentials: aspect not allowed")
	ErrKindMismatch    = errors.New("credentials: kind mismatch")
	ErrInvalidMode     = errors.New("credentials: invalid mode")
	ErrPlaintextDenied = errors.New("credentials: plaintext fetch not allowed for this credential")
	ErrUnknownKind     = errors.New("credentials: unknown kind")
	ErrNoDefault       = errors.New("credentials: aspect has no default for this kind")
)

// Store is the persistent backing for credentials. Safe for concurrent use.
type Store struct {
	db      *sql.DB
	dataKey []byte // derived once from session_signing_secret via HKDF
}

// NewStore wraps db and derives the encryption data key from the
// nexus identity's session_signing_secret. Failing to derive is fatal —
// callers should propagate the error rather than running without
// encryption.
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
// signing secret via HKDF-SHA256 with a fixed info string. Versioned so
// future migration to a different KDF can distinguish old from new.
// The info string is preserved from the pre-NEX-75 schema so existing
// encrypted rows continue to decrypt correctly through the migration.
func deriveDataKey(sessionSecret []byte) ([]byte, error) {
	const info = "nexus.credentials.v1"
	kdf := hkdf.New(sha256.New, sessionSecret, nil, []byte(info))
	key := make([]byte, 32) // AES-256
	if _, err := io.ReadFull(kdf, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Set upserts a credential. The Bundle is JSON-marshalled then encrypted
// before write.
func (s *Store) Set(ctx context.Context, p UpsertParams) error {
	if err := validateUpsert(p); err != nil {
		return err
	}
	bundleJSON, err := json.Marshal(p.Bundle)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}
	ciphertext, nonce, err := s.encrypt(bundleJSON)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	allowedJSON, err := json.Marshal(p.AllowedAspects)
	if err != nil {
		return fmt.Errorf("marshal allowed_aspects: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO credentials
			(name, description, kind,
			 encrypted_bundle, encryption_nonce,
			 allowed_aspects, mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			description       = excluded.description,
			kind              = excluded.kind,
			encrypted_bundle  = excluded.encrypted_bundle,
			encryption_nonce  = excluded.encryption_nonce,
			allowed_aspects   = excluded.allowed_aspects,
			mode              = excluded.mode,
			updated_at        = excluded.updated_at
	`, p.Name, p.Description, string(p.Kind),
		ciphertext, nonce,
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
	if !IsKnownKind(p.Kind) {
		return fmt.Errorf("%w: %q", ErrUnknownKind, p.Kind)
	}
	if p.Mode != ModeProxy && p.Mode != ModeFetch && p.Mode != ModeBoth {
		return fmt.Errorf("%w: %q", ErrInvalidMode, p.Mode)
	}
	if len(p.AllowedAspects) == 0 {
		return errors.New("credentials: allowed_aspects must be non-empty (use [\"*\"] for all)")
	}
	if p.Bundle == nil {
		return errors.New("credentials: bundle required")
	}
	if err := validateBundle(p.Kind, p.Bundle); err != nil {
		return err
	}
	return nil
}

// validateBundle checks per-kind required fields in the bundle payload.
// Keeps the handler layer thin — admin REST just unmarshals JSON into a
// map and passes it through; this is where the shape contract enforces.
func validateBundle(kind Kind, bundle map[string]any) error {
	requireString := func(key string) error {
		v, ok := bundle[key]
		if !ok {
			return fmt.Errorf("credentials: %s bundle missing %q", kind, key)
		}
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("credentials: %s bundle %q must be a non-empty string", kind, key)
		}
		return nil
	}
	switch kind {
	case KindProvider:
		if err := requireString("api_shape"); err != nil {
			return err
		}
		if err := requireString("base_url"); err != nil {
			return err
		}
		if err := requireString("key"); err != nil {
			return err
		}
		// default_model is optional.
		shape, _ := bundle["api_shape"].(string)
		if APIShape(shape) != ShapeAnthropic && APIShape(shape) != ShapeOpenAI {
			return fmt.Errorf("credentials: provider bundle api_shape %q unsupported (want anthropic|openai)", shape)
		}
	case KindJira:
		if err := requireString("atlassian_email"); err != nil {
			return err
		}
		if err := requireString("atlassian_token"); err != nil {
			return err
		}
		if err := requireString("atlassian_subdomain"); err != nil {
			return err
		}
	case KindIMAP:
		if err := requireString("host"); err != nil {
			return err
		}
		if err := requireString("user"); err != nil {
			return err
		}
		if err := requireString("password"); err != nil {
			return err
		}
		// port + ssl have defaults at the consumer; not required here.
	}
	return nil
}

// Get returns the credential with encrypted material intact. Use
// Bundle() / ProviderBundle() / etc. to access plaintext.
func (s *Store) Get(ctx context.Context, name string) (Credential, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, description, kind,
		       encrypted_bundle, encryption_nonce,
		       allowed_aspects, mode, created_at, updated_at, last_used_at
		  FROM credentials
		 WHERE name = ?
	`, name)
	var c Credential
	var kind, mode string
	var allowedJSON string
	var createdAt, updatedAt string
	var lastUsedAt sql.NullString
	if err := row.Scan(&c.Name, &c.Description, &kind,
		&c.encryptedBundle, &c.nonce,
		&allowedJSON, &mode, &createdAt, &updatedAt, &lastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, ErrNotFound
		}
		return Credential{}, fmt.Errorf("scan: %w", err)
	}
	c.Kind = Kind(kind)
	c.Mode = Mode(mode)
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

// Bundle returns the decrypted, raw bundle map for c. Callers MUST NOT
// log or persist the result; use immediately and drop the reference.
// Kind-typed accessors (ProviderBundle, JiraBundle, IMAPBundle) are
// preferred when the kind is known statically.
func (s *Store) Bundle(c Credential) (map[string]any, error) {
	plaintext, err := s.decrypt(c.encryptedBundle, c.nonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt bundle: %w", err)
	}
	var bundle map[string]any
	if err := json.Unmarshal(plaintext, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal bundle: %w", err)
	}
	return bundle, nil
}

// ProviderBundle returns the decrypted bundle for a kind='provider'
// credential, typed. Returns ErrKindMismatch if c is not provider-kind.
func (s *Store) ProviderBundle(c Credential) (ProviderBundle, error) {
	if c.Kind != KindProvider {
		return ProviderBundle{}, fmt.Errorf("%w: have %q want %q", ErrKindMismatch, c.Kind, KindProvider)
	}
	plaintext, err := s.decrypt(c.encryptedBundle, c.nonce)
	if err != nil {
		return ProviderBundle{}, fmt.Errorf("decrypt bundle: %w", err)
	}
	var pb ProviderBundle
	if err := json.Unmarshal(plaintext, &pb); err != nil {
		return ProviderBundle{}, fmt.Errorf("unmarshal provider bundle: %w", err)
	}
	return pb, nil
}

// JiraBundle returns the decrypted bundle for a kind='jira' credential.
func (s *Store) JiraBundle(c Credential) (JiraBundle, error) {
	if c.Kind != KindJira {
		return JiraBundle{}, fmt.Errorf("%w: have %q want %q", ErrKindMismatch, c.Kind, KindJira)
	}
	plaintext, err := s.decrypt(c.encryptedBundle, c.nonce)
	if err != nil {
		return JiraBundle{}, fmt.Errorf("decrypt bundle: %w", err)
	}
	var jb JiraBundle
	if err := json.Unmarshal(plaintext, &jb); err != nil {
		return JiraBundle{}, fmt.Errorf("unmarshal jira bundle: %w", err)
	}
	return jb, nil
}

// IMAPBundle returns the decrypted bundle for a kind='imap' credential.
func (s *Store) IMAPBundle(c Credential) (IMAPBundle, error) {
	if c.Kind != KindIMAP {
		return IMAPBundle{}, fmt.Errorf("%w: have %q want %q", ErrKindMismatch, c.Kind, KindIMAP)
	}
	plaintext, err := s.decrypt(c.encryptedBundle, c.nonce)
	if err != nil {
		return IMAPBundle{}, fmt.Errorf("decrypt bundle: %w", err)
	}
	var ib IMAPBundle
	if err := json.Unmarshal(plaintext, &ib); err != nil {
		return IMAPBundle{}, fmt.Errorf("unmarshal imap bundle: %w", err)
	}
	return ib, nil
}

// EnvForCredential builds the {API_KEY, BASE_URL} env-var pair a
// provider would consume. Only meaningful for kind='provider'.
// Returns ErrKindMismatch for non-provider kinds.
//
// Anthropic shape emits ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL (URL
// omitted when empty so the provider falls back to its default
// endpoint). OpenAI shape emits OPENAI_API_KEY + OPENAI_BASE_URL.
// Future shapes register their own canonical env names here.
//
// Returned map is fresh and caller-owned — safe to merge into a
// bridle.TurnRequest.ProviderEnv without copy.
func (s *Store) EnvForCredential(c Credential) (map[string]string, error) {
	pb, err := s.ProviderBundle(c)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, 2)
	switch pb.APIShape {
	case ShapeAnthropic:
		env["ANTHROPIC_API_KEY"] = pb.Key
		if pb.BaseURL != "" {
			env["ANTHROPIC_BASE_URL"] = pb.BaseURL
		}
	case ShapeOpenAI:
		env["OPENAI_API_KEY"] = pb.Key
		if pb.BaseURL != "" {
			env["OPENAI_BASE_URL"] = pb.BaseURL
		}
	default:
		return nil, fmt.Errorf("credentials: no env mapping for api_shape %q", pb.APIShape)
	}
	return env, nil
}

// ResolveDefaultForAspect looks up the aspect's default credential for
// the given provider shape via aspects.default_{anthropic,openai}_credential,
// loads the matching credential, verifies the aspect is allowed, and
// returns a ready-to-overlay env map. Returns ErrNoDefault when the
// aspect has no default configured (caller falls through to
// process-env / subscription auth).
//
// Provider-kind only. For non-provider kinds use ResolveDefaultBundle.
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
	c, err := s.resolveAspectDefault(ctx, aspect, col)
	if err != nil {
		return Credential{}, nil, err
	}
	env, err := s.EnvForCredential(c)
	if err != nil {
		return Credential{}, nil, err
	}
	return c, env, nil
}

// ResolveDefaultBundle looks up the aspect's default credential for a
// non-provider kind (jira, imap) via aspects.default_<kind>_credential,
// loads the row, verifies allow, and returns it. Caller uses the
// kind-typed accessor (JiraBundle / IMAPBundle) on the result to get
// plaintext. Returns ErrNoDefault when no default configured.
func (s *Store) ResolveDefaultBundle(ctx context.Context, aspect string, kind Kind) (Credential, error) {
	col := ""
	switch kind {
	case KindJira:
		col = "default_jira_credential"
	case KindIMAP:
		col = "default_imap_credential"
	default:
		return Credential{}, fmt.Errorf("credentials: no default-column mapping for kind %q", kind)
	}
	c, err := s.resolveAspectDefault(ctx, aspect, col)
	if err != nil {
		return Credential{}, err
	}
	if c.Kind != kind {
		return Credential{}, fmt.Errorf("%w: aspect default %q is kind %q, requested %q", ErrKindMismatch, c.Name, c.Kind, kind)
	}
	return c, nil
}

// AspectDefaults describes the per-aspect default-credential
// configuration. Used by admin REST and CLI to read/write the
// columns on the aspects table without callers knowing the
// per-kind column names. Empty string in any field means "unset"
// (no default for that shape/kind).
type AspectDefaults struct {
	Aspect             string  `json:"aspect"`
	AnthropicDefault   *string `json:"default_anthropic_credential,omitempty"`
	OpenAIDefault      *string `json:"default_openai_credential,omitempty"`
	JiraDefault        *string `json:"default_jira_credential,omitempty"`
	IMAPDefault        *string `json:"default_imap_credential,omitempty"`
}

// GetAspectDefaults reads the per-aspect default-credential columns
// for `aspect`. Returns zero-value AspectDefaults with all-nil
// pointers if the aspect row doesn't exist (idempotent read — the
// caller can treat absence and all-unset the same).
func (s *Store) GetAspectDefaults(ctx context.Context, aspect string) (AspectDefaults, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT default_anthropic_credential,
		       default_openai_credential,
		       default_jira_credential,
		       default_imap_credential
		  FROM aspects WHERE name = ?
	`, aspect)
	var ad AspectDefaults
	ad.Aspect = aspect
	var anth, oai, jira, imap sql.NullString
	if err := row.Scan(&anth, &oai, &jira, &imap); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ad, nil
		}
		return ad, fmt.Errorf("scan aspect defaults: %w", err)
	}
	if anth.Valid {
		v := anth.String
		ad.AnthropicDefault = &v
	}
	if oai.Valid {
		v := oai.String
		ad.OpenAIDefault = &v
	}
	if jira.Valid {
		v := jira.String
		ad.JiraDefault = &v
	}
	if imap.Valid {
		v := imap.String
		ad.IMAPDefault = &v
	}
	return ad, nil
}

// SetAspectDefault sets one default-credential column for `aspect`.
// `kind` selects which column (provider's anthropic/openai shapes
// are addressed via APIShape; non-provider kinds via Kind):
//
//	column                          → DefaultColumn value
//	default_anthropic_credential    → "anthropic"
//	default_openai_credential       → "openai"
//	default_jira_credential         → "jira"
//	default_imap_credential         → "imap"
//
// Pass credentialName="" to clear the default (NULL in the column).
// Returns an error if the named credential doesn't exist (and
// credentialName != ""), or if the aspect row doesn't exist.
//
// Note: this doesn't validate that the credential's stored kind
// matches the requested default-column kind. Callers (admin REST,
// CLI) should do that check themselves before invoking — it gives
// them a chance to surface a clean 400 to the operator rather than
// silently letting a mis-shaped default sit on the row.
func (s *Store) SetAspectDefault(ctx context.Context, aspect, defaultColumn, credentialName string) error {
	col := ""
	switch defaultColumn {
	case "anthropic":
		col = "default_anthropic_credential"
	case "openai":
		col = "default_openai_credential"
	case "jira":
		col = "default_jira_credential"
	case "imap":
		col = "default_imap_credential"
	default:
		return fmt.Errorf("credentials: unknown default-column key %q (want anthropic|openai|jira|imap)", defaultColumn)
	}
	// Verify the credential exists if we're setting (not clearing).
	if credentialName != "" {
		if _, err := s.Get(ctx, credentialName); err != nil {
			return fmt.Errorf("credentials: cannot set default %q for aspect %q: %w", credentialName, aspect, err)
		}
	}
	var arg any
	if credentialName == "" {
		arg = nil
	} else {
		arg = credentialName
	}
	res, err := s.db.ExecContext(ctx, `UPDATE aspects SET `+col+` = ? WHERE name = ?`, arg, aspect)
	if err != nil {
		return fmt.Errorf("update aspect default: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("credentials: aspect %q not found (cannot set default)", aspect)
	}
	return nil
}

// AspectModelConfig is the per-aspect, per-kind model + credential
// override read from the aspects table (NEX-263). Each field is nullable
// — nil means "no override; inherit the keyfile value at runtime".
// Lives here rather than in a separate aspectconfig package because the
// infrastructure (aspects-row CRUD) is identical and the credential
// fields reference credential names this Store already manages.
type AspectModelConfig struct {
	Aspect            string  `json:"aspect"`
	PrimaryModel      *string `json:"primary_model,omitempty"`
	PrimaryCredential *string `json:"primary_credential,omitempty"`
	JudgeModel        *string `json:"judge_model,omitempty"`
	JudgeCredential   *string `json:"judge_credential,omitempty"`
	CompactModel      *string `json:"compact_model,omitempty"`
	CompactCredential *string `json:"compact_credential,omitempty"`
}

// modelConfigColumns is the allowed set of column names for
// SetAspectModelField. Mirrored exactly in the schema migrations
// added by NEX-263 — keep these in sync.
var modelConfigColumns = map[string]bool{
	"primary_model":      true,
	"primary_credential": true,
	"judge_model":        true,
	"judge_credential":   true,
	"compact_model":      true,
	"compact_credential": true,
}

// GetAspectModelConfig reads the per-aspect model + credential override
// columns for `aspect`. Returns AspectModelConfig with all-nil pointers
// if the aspect row doesn't exist (mirrors GetAspectDefaults semantics).
func (s *Store) GetAspectModelConfig(ctx context.Context, aspect string) (AspectModelConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT primary_model, primary_credential,
		       judge_model,   judge_credential,
		       compact_model, compact_credential
		  FROM aspects WHERE name = ?
	`, aspect)
	cfg := AspectModelConfig{Aspect: aspect}
	var pm, pc, jm, jc, cm, cc sql.NullString
	if err := row.Scan(&pm, &pc, &jm, &jc, &cm, &cc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("scan aspect model config: %w", err)
	}
	if pm.Valid {
		v := pm.String
		cfg.PrimaryModel = &v
	}
	if pc.Valid {
		v := pc.String
		cfg.PrimaryCredential = &v
	}
	if jm.Valid {
		v := jm.String
		cfg.JudgeModel = &v
	}
	if jc.Valid {
		v := jc.String
		cfg.JudgeCredential = &v
	}
	if cm.Valid {
		v := cm.String
		cfg.CompactModel = &v
	}
	if cc.Valid {
		v := cc.String
		cfg.CompactCredential = &v
	}
	return cfg, nil
}

// SetAspectModelField writes a single model-config column on the aspect
// row. column must be one of primary_model / primary_credential /
// judge_model / judge_credential / compact_model / compact_credential.
// Pass value="" to clear (NULL in the column).
//
// For *_credential columns with a non-empty value, the credential's
// existence is verified before write to surface clean errors at the
// REST layer rather than producing a dangling reference.
func (s *Store) SetAspectModelField(ctx context.Context, aspect, column, value string) error {
	if !modelConfigColumns[column] {
		return fmt.Errorf("credentials: unknown model-config column %q", column)
	}
	// Verify credential exists if we're naming one (not clearing, not a model field).
	if value != "" && (column == "primary_credential" || column == "judge_credential" || column == "compact_credential") {
		if _, err := s.Get(ctx, value); err != nil {
			return fmt.Errorf("credentials: cannot set %s=%q for aspect %q: %w", column, value, aspect, err)
		}
	}
	var arg any
	if value == "" {
		arg = nil
	} else {
		arg = value
	}
	res, err := s.db.ExecContext(ctx, `UPDATE aspects SET `+column+` = ? WHERE name = ?`, arg, aspect)
	if err != nil {
		return fmt.Errorf("update aspect model config: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("credentials: aspect %q not found (cannot set %s)", aspect, column)
	}
	return nil
}

// resolveAspectDefault is the shared lookup-and-allow-check for both
// provider and non-provider default-credential resolution.
func (s *Store) resolveAspectDefault(ctx context.Context, aspect, col string) (Credential, error) {
	var name sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT `+col+` FROM aspects WHERE name = ?`, aspect).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, ErrNoDefault
		}
		return Credential{}, fmt.Errorf("lookup aspect default: %w", err)
	}
	if !name.Valid || name.String == "" {
		return Credential{}, ErrNoDefault
	}
	c, err := s.Get(ctx, name.String)
	if err != nil {
		return Credential{}, fmt.Errorf("resolve credential %q for aspect %q: %w", name.String, aspect, err)
	}
	if !c.AllowedFor(aspect) {
		return Credential{}, fmt.Errorf("%w: aspect %q not allowed for credential %q", ErrPermission, aspect, name.String)
	}
	return c, nil
}

// List returns every credential's metadata. Never includes bundle material.
// Optional kind filter: empty kind = return all kinds.
func (s *Store) List(ctx context.Context, kindFilter Kind) ([]Metadata, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if kindFilter == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, description, kind,
			       allowed_aspects, mode, created_at, updated_at, last_used_at
			  FROM credentials
			 ORDER BY name
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, description, kind,
			       allowed_aspects, mode, created_at, updated_at, last_used_at
			  FROM credentials
			 WHERE kind = ?
			 ORDER BY name
		`, string(kindFilter))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metadata
	for rows.Next() {
		var m Metadata
		var kind, mode string
		var allowedJSON string
		var createdAt, updatedAt string
		var lastUsedAt sql.NullString
		if err := rows.Scan(&m.Name, &m.Description, &kind,
			&allowedJSON, &mode, &createdAt, &updatedAt, &lastUsedAt); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		m.Mode = Mode(mode)
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
// credential_audit cleans up audit rows for that credential.
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE name = ?`, name)
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
// after a successful proxy call or fetch. Best-effort.
func (s *Store) TouchLastUsed(ctx context.Context, name string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE credentials SET last_used_at = ? WHERE name = ?`, now, name)
	return err
}

// RecordAudit writes a credential_audit row.
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
// Fresh 96-bit nonce per call.
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

// decrypt is the inverse.
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
