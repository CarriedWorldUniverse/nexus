// Admin REST surface for flipping an aspect's provider+model runtime
// binding without re-minting (NEX-335).
//
// Routes (admin-gated, registered when KeyfileValidator is non-nil):
//
//	GET /api/admin/aspects/{name}/provider-binding  — read current binding
//	PUT /api/admin/aspects/{name}/provider-binding  — set provider + model
//
// Distinct from /model-config (NEX-263), which manages per-aspect
// MODEL overrides for primary/judge/compact KINDS. That endpoint
// leaves the broker-authoritative aspects.provider column alone;
// this one updates it directly. The two cooperate: provider-binding
// is "what runtime does this aspect use"; model-config is "what
// model id does each turn-kind run on, possibly per-kind".
//
// Why a dedicated endpoint rather than re-mint: re-mint generates a
// new aspect pubkey and invalidates the keyfile in the operator's
// hand. That's correct semantics for "rotate identity"; it's overkill
// for "switch from claude-code to openai." This endpoint preserves
// the keyfile + identity and changes only the broker→aspect runtime
// binding fields. agentfunnel picks up the new provider on its next
// validate (reconnect or restart).

package broker

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// supportedProviders mirrors the buildProvider switch in
// runtime/cmd/agentfunnel/main.go. Update both when adding a backend.
var supportedProviders = map[string]bool{
	"claude":     true, // alias for claude-api
	"claude-api": true,
	"claudecode": true, // alias for claude-code
	"claude-code": true,
	"openai":     true,
	"codex":      true, // alias for codex-cli
	"codex-cli":  true,
	"codexcli":   true, // alias for codex-cli
}

type providerBindingResponse struct {
	Aspect   string `json:"aspect"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type providerBindingReq struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// handleAdminProviderBindingGet returns the current provider + model.
func (b *Broker) handleAdminProviderBindingGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.KeyfileValidator == nil || b.cfg.KeyfileValidator.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "aspects store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	row, err := b.cfg.KeyfileValidator.Store.Get(r.Context(), aspect)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "aspect not found")
			return
		}
		b.log.Error("admin provider-binding get", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, providerBindingResponse{
		Aspect:   row.Name,
		Provider: row.Provider,
		Model:    row.Model,
	})
}

// handleAdminProviderBindingSet writes the provider + model columns.
// Both required; provider validated against the supported-list so a
// typo can't write garbage that buildProvider would later reject at
// agentfunnel startup (operator sees the failure at the API boundary
// instead of a few minutes later in a remote aspect's logs).
func (b *Broker) handleAdminProviderBindingSet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.KeyfileValidator == nil || b.cfg.KeyfileValidator.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "aspects store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req providerBindingReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "provider required")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model required")
		return
	}
	if !supportedProviders[req.Provider] {
		writeError(w, http.StatusBadRequest, "unsupported provider "+req.Provider)
		return
	}

	if err := b.cfg.KeyfileValidator.Store.SetProviderAndModel(r.Context(), aspect, req.Provider, req.Model); err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "aspect not found")
			return
		}
		b.log.Error("admin provider-binding set", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	b.log.Info("admin provider-binding set", "aspect", aspect, "provider", req.Provider, "model", req.Model)

	row, err := b.cfg.KeyfileValidator.Store.Get(r.Context(), aspect)
	if err != nil {
		// Write succeeded; readback failed. Return what we set.
		writeJSON(w, http.StatusOK, providerBindingResponse{
			Aspect:   aspect,
			Provider: req.Provider,
			Model:    req.Model,
		})
		return
	}
	writeJSON(w, http.StatusOK, providerBindingResponse{
		Aspect:   row.Name,
		Provider: row.Provider,
		Model:    row.Model,
	})
}
