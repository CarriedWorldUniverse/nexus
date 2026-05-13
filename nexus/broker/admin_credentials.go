// Admin REST surface for broker-mediated API credentials (task #218).
//
// Routes (all admin-gated, registered when Config.Credentials is non-nil):
//
//	GET    /api/admin/credentials             — list metadata (no keys)
//	PUT    /api/admin/credentials/{name}      — upsert credential
//	GET    /api/admin/credentials/{name}      — get one (metadata only)
//	DELETE /api/admin/credentials/{name}      — delete
//	GET    /api/admin/credentials/{name}/audit — recent audit rows
//
// Plaintext key material is NEVER returned by any GET. The PUT body is
// the only way a key enters the system; once stored it's accessible
// only via proxy tools (or — when mode=fetch — via credential.fetch on
// the aspect WS surface, with audit row written every time).

package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

type adminCredUpsertReq struct {
	Description    string   `json:"description"`
	APIShape       string   `json:"api_shape"`
	BaseURL        string   `json:"base_url"`
	Key            string   `json:"key"`
	DefaultModel   string   `json:"default_model,omitempty"`
	AllowedAspects []string `json:"allowed_aspects"`
	Mode           string   `json:"mode"`
}

func (b *Broker) handleAdminCredentialsList(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	ms, err := b.cfg.Credentials.List(r.Context())
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
	dec.DisallowUnknownFields()
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
	params := credentials.UpsertParams{
		Name:           name,
		Description:    req.Description,
		APIShape:       credentials.APIShape(req.APIShape),
		BaseURL:        req.BaseURL,
		Key:            req.Key,
		DefaultModel:   req.DefaultModel,
		AllowedAspects: allowed,
		Mode:           mode,
	}
	if err := b.cfg.Credentials.Set(r.Context(), params); err != nil {
		// validation errors come back as plain errors, not sentinels.
		// Surface them verbatim so the caller can see what was wrong;
		// they don't leak secret material because Set only validates
		// the shape of inputs, not the key value.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b.log.Info("admin credential upsert", "name", name, "api_shape", req.APIShape)
	// Re-fetch metadata for the response so the caller sees the
	// canonical timestamps and (in the future) any defaults applied.
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
