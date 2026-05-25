// Admin REST surface for network-wide judge + compact defaults (NEX-294).
//
// Routes (admin-gated, registered when Config.Credentials is non-nil):
//
//	GET /api/admin/network-defaults  — read effective network defaults
//	PUT /api/admin/network-defaults  — set/clear any subset of fields
//
// The columns live on the single-row network_defaults table (added by
// NEX-294 Slice 1). Each field is independently nullable — empty string
// = "no network default; resolution falls through to caller's legacy
// fallback (haiku model, ambient env credential)".
//
// Primary model + primary credential intentionally NOT exposed here:
// primary is per-aspect differentiation by design (operators set those
// via /api/admin/aspects/{name}/model-config from NEX-263).
//
// GET response shape mirrors credentials.NetworkDefaults:
//
//	{
//	  "judge_model":        "deepseek-chat" | "",
//	  "judge_credential":   "deepseek-ds"   | "",
//	  "compact_model":      "deepseek-chat" | "",
//	  "compact_credential": "deepseek-ds"   | ""
//	}
//
// Empty string = no default set. Resolution order at runtime:
//
//	per-aspect override (NEX-263) > network default (this) > legacy fallback
//
// PUT body accepts any subset of the same fields (same pattern as
// NEX-263's model-config). Each field is independent — operator can
// set judge_credential without touching compact_*. Empty string
// clears the default (writes NULL).
//
//	{ "judge_model": "deepseek-chat", "judge_credential": "ds-cred" }
//	→ sets the judge model + credential, leaves compact untouched.

package broker

import (
	"encoding/json"
	"net/http"
)

// adminNetworkDefaultsReq mirrors NetworkDefaults but uses *string so
// the handler can distinguish "field omitted (leave alone)" from
// "field set to empty (clear default)". JSON nulls decode to nil here.
type adminNetworkDefaultsReq struct {
	JudgeModel        *string `json:"judge_model,omitempty"`
	JudgeCredential   *string `json:"judge_credential,omitempty"`
	CompactModel      *string `json:"compact_model,omitempty"`
	CompactCredential *string `json:"compact_credential,omitempty"`
}

// handleAdminNetworkDefaultsGet returns the current network-wide
// defaults. All-empty response means no defaults configured — every
// aspect falls through to per-aspect override or legacy fallback.
func (b *Broker) handleAdminNetworkDefaultsGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	nd, err := b.cfg.Credentials.GetNetworkDefaults(r.Context())
	if err != nil {
		b.log.Error("admin network-defaults get", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, nd)
}

// handleAdminNetworkDefaultsSet writes any subset of network-default
// columns. Each provided field is independent; omitted fields are
// left untouched; empty-string fields are cleared (NULL). Mirrors the
// shape of NEX-263's per-aspect model-config setter so the same
// frontend submit logic can be reused (only the path + key set
// differ).
func (b *Broker) handleAdminNetworkDefaultsSet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req adminNetworkDefaultsReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}

	updates := map[string]*string{
		"judge_model":        req.JudgeModel,
		"judge_credential":   req.JudgeCredential,
		"compact_model":      req.CompactModel,
		"compact_credential": req.CompactCredential,
	}
	applied := []string{}
	for col, val := range updates {
		if val == nil {
			continue
		}
		if err := b.cfg.Credentials.SetNetworkDefaultField(r.Context(), col, *val); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		applied = append(applied, col)
	}
	b.log.Info("admin network-defaults set", "applied", applied)

	nd, err := b.cfg.Credentials.GetNetworkDefaults(r.Context())
	if err != nil {
		b.log.Error("admin network-defaults read-back", "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
		return
	}
	writeJSON(w, http.StatusOK, nd)
}
