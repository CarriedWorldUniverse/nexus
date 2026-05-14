// Admin REST surface for broker-mediated credentials (#218, NEX-74/75/76).
//
// Routes (all admin-gated, registered when Config.Credentials is non-nil):
//
//	GET    /api/admin/credentials                          — list metadata (no bundles)
//	                                                          ?kind=<kind> to filter
//	PUT    /api/admin/credentials/{name}                   — upsert credential
//	GET    /api/admin/credentials/{name}                   — get one (metadata only)
//	DELETE /api/admin/credentials/{name}                   — delete
//	GET    /api/admin/credentials/{name}/audit             — recent audit rows
//	GET    /api/admin/aspects/{name}/credential-defaults   — read per-aspect defaults
//	PUT    /api/admin/aspects/{name}/credential-defaults   — set/clear per-aspect defaults
//
// Plaintext bundle material is NEVER returned by any GET. The PUT body
// is the only way a credential bundle enters the system; once stored
// it's accessible only via proxy tools (or — when mode=fetch — via
// credential.fetch on the aspect WS surface, with audit row written).
//
// Upsert request shape (NEX-76):
//
// Legacy provider-only shape (back-compat with #218 era curl scripts):
//
//	{ "api_shape": "anthropic", "base_url": "...", "key": "...",
//	  "default_model": "...", "allowed_aspects": ["*"], "mode": "proxy" }
//
// New kind-typed shape (NEX-76+):
//
//	{ "kind": "jira", "bundle": {"atlassian_email":"...","atlassian_token":"...","atlassian_subdomain":"..."},
//	  "allowed_aspects": ["forge","plumb"], "mode": "fetch", "description": "..." }
//
// When `kind` is set on the request, the new shape is used and any
// legacy top-level fields (api_shape/base_url/key/default_model) are
// IGNORED. When `kind` is unset, the legacy provider-shape branch
// runs — top-level fields get packed into a provider bundle.

package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// adminCredUpsertReq carries both the legacy provider-shape fields and
// the new kind-typed fields. The handler branches on whether Kind is
// set: kind="" → legacy path (provider-only), kind set → new path
// (any kind + bundle). DisallowUnknownFields is off here because the
// transitional shape needs both vocabularies tolerated.
type adminCredUpsertReq struct {
	Description    string         `json:"description"`
	AllowedAspects []string       `json:"allowed_aspects"`
	Mode           string         `json:"mode"`

	// Legacy provider-only fields. Used when Kind is empty / "provider".
	APIShape     string `json:"api_shape,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	Key          string `json:"key,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`

	// New kind-typed fields (NEX-76). When Kind is non-empty, Bundle
	// carries the per-kind opaque payload; legacy top-level fields are
	// ignored entirely.
	Kind   string         `json:"kind,omitempty"`
	Bundle map[string]any `json:"bundle,omitempty"`
}

func (b *Broker) handleAdminCredentialsList(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	// Optional ?kind= filter — passes through to store.List.
	kindFilter := credentials.Kind(r.URL.Query().Get("kind"))
	if kindFilter != "" && !credentials.IsKnownKind(kindFilter) {
		writeError(w, http.StatusBadRequest, "unknown kind: "+string(kindFilter))
		return
	}
	ms, err := b.cfg.Credentials.List(r.Context(), kindFilter)
	if err != nil {
		b.log.Error("admin credentials list", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ms == nil {
		ms = []credentials.Metadata{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": ms})
}

func (b *Broker) handleAdminCredentialUpsert(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "credential name required in path")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req adminCredUpsertReq
	dec := json.NewDecoder(r.Body)
	// Don't DisallowUnknownFields — we accept both the legacy and
	// new shapes on the same endpoint, so callers may send fields
	// the other shape doesn't recognise. validateBundle on the
	// store side catches actual content errors.
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}
	mode := credentials.Mode(req.Mode)
	if mode == "" {
		mode = credentials.ModeProxy
	}
	allowed := req.AllowedAspects
	if len(allowed) == 0 {
		allowed = []string{"*"}
	}

	// Branch on which request shape the caller used.
	//   - req.Kind set (NEX-76 path):  use the explicit kind + bundle.
	//   - req.Kind empty (legacy path): provider-only — pack top-level
	//     legacy fields into a provider bundle.
	var (
		kind   credentials.Kind
		bundle map[string]any
	)
	if req.Kind != "" {
		kind = credentials.Kind(req.Kind)
		if !credentials.IsKnownKind(kind) {
			writeError(w, http.StatusBadRequest, "unknown kind: "+req.Kind)
			return
		}
		if req.Bundle == nil {
			writeError(w, http.StatusBadRequest, "bundle required when kind is set")
			return
		}
		bundle = req.Bundle
	} else {
		// Legacy provider-shape — back-compat with pre-NEX-76 callers
		// (curl scripts, agent-network admin tooling). Pack the
		// top-level provider fields into the bundle map.
		kind = credentials.KindProvider
		bundle = map[string]any{
			"api_shape": req.APIShape,
			"base_url":  req.BaseURL,
			"key":       req.Key,
		}
		if req.DefaultModel != "" {
			bundle["default_model"] = req.DefaultModel
		}
	}

	params := credentials.UpsertParams{
		Name:           name,
		Description:    req.Description,
		Kind:           kind,
		Bundle:         bundle,
		AllowedAspects: allowed,
		Mode:           mode,
	}
	if err := b.cfg.Credentials.Set(r.Context(), params); err != nil {
		// validation errors come back as plain errors (not sentinels).
		// Surface them verbatim — they describe what's wrong with the
		// shape, not the secret material.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b.log.Info("admin credential upsert", "name", name, "kind", kind)
	// Re-fetch metadata for the response so the caller sees canonical
	// timestamps + kind.
	c, err := b.cfg.Credentials.Get(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"name": name})
		return
	}
	writeJSON(w, http.StatusOK, c.ToMetadata())
}

func (b *Broker) handleAdminCredentialGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	name := r.PathValue("name")
	c, err := b.cfg.Credentials.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		b.log.Error("admin credential get", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, c.ToMetadata())
}

func (b *Broker) handleAdminCredentialDelete(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	name := r.PathValue("name")
	if err := b.cfg.Credentials.Delete(r.Context(), name); err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		b.log.Error("admin credential delete", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	b.log.Info("admin credential delete", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func (b *Broker) handleAdminCredentialAudit(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	name := r.PathValue("name")
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	rows, err := b.cfg.Credentials.ListAudit(r.Context(), name, limit)
	if err != nil {
		b.log.Error("admin credential audit", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rows == nil {
		rows = []credentials.AuditRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": rows})
}

// -------------------------------------------------------------------
// Per-aspect credential defaults (NEX-76)
// -------------------------------------------------------------------

// adminAspectDefaultsReq carries the four default-credential fields the
// operator can set on an aspect. All fields are pointers so the
// distinction between "set to credential X", "clear to NULL", and "no
// change" is explicit in the request:
//
//	field omitted        → no change
//	field set to ""      → clear (UPDATE col = NULL)
//	field set to "name"  → set to that credential
type adminAspectDefaultsReq struct {
	Anthropic *string `json:"default_anthropic_credential,omitempty"`
	OpenAI    *string `json:"default_openai_credential,omitempty"`
	Jira      *string `json:"default_jira_credential,omitempty"`
	IMAP      *string `json:"default_imap_credential,omitempty"`
}

// handleAdminAspectDefaultsGet returns the per-aspect default
// credential names for each kind/shape.
func (b *Broker) handleAdminAspectDefaultsGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	ad, err := b.cfg.Credentials.GetAspectDefaults(r.Context(), aspect)
	if err != nil {
		b.log.Error("admin aspect defaults get", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, ad)
}

// handleAdminAspectDefaultsSet writes one or more default-credential
// columns on the aspect row. Each field in the request is independent
// — operator can set Anthropic + leave Jira untouched in the same call.
func (b *Broker) handleAdminAspectDefaultsSet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req adminAspectDefaultsReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}

	// Apply each field that was sent. Empty string means clear (NULL).
	// Track which updates we attempted so the response reflects only
	// what changed.
	updates := map[string]*string{
		"anthropic": req.Anthropic,
		"openai":    req.OpenAI,
		"jira":      req.Jira,
		"imap":      req.IMAP,
	}
	applied := []string{}
	for col, val := range updates {
		if val == nil {
			continue
		}
		if err := b.cfg.Credentials.SetAspectDefault(r.Context(), aspect, col, *val); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		applied = append(applied, col)
	}
	b.log.Info("admin aspect defaults set", "aspect", aspect, "applied", applied)

	// Echo back the current state.
	ad, err := b.cfg.Credentials.GetAspectDefaults(r.Context(), aspect)
	if err != nil {
		b.log.Error("admin aspect defaults read-back", "aspect", aspect, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"aspect": aspect, "applied": applied})
		return
	}
	writeJSON(w, http.StatusOK, ad)
}
