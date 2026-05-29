// Admin REST surface for per-aspect model + credential overrides (NEX-263).
//
// Routes (admin-gated, registered when Config.Credentials is non-nil):
//
//	GET /api/admin/aspects/{name}/model-config  — read override state
//	PUT /api/admin/aspects/{name}/model-config  — set/clear override fields
//
// The columns live on the aspects table (added by NEX-263 schema
// migrations). Each field is independently nullable — null means "no
// override; aspect inherits the keyfile value at runtime". Operator
// sets these via the dashboard Settings → Aspects page (NEX-265) so
// model selection no longer requires editing keyfile JSON + restart.
//
// GET response shape mirrors credentials.AspectModelConfig:
//
//	{
//	  "aspect": "anvil",
//	  "primary_model":      "claude-opus-4-7"   | null,
//	  "primary_credential": "claude-api"        | null,
//	  "judge_model":        "deepseek-v4-flash" | null,
//	  "judge_credential":   "deepseek-ds"       | null,
//	  "compact_model":      "claude-haiku-4-5"  | null,
//	  "compact_credential": "claude-api"        | null
//	}
//
// A null field means the override is unset and the keyfile value
// applies. The Settings UI overlays keyfile defaults on top of this
// response to show the operator the effective value.
//
// PUT body accepts any subset of the same fields. Each field is
// independent — operator can set primary_model without touching
// judge_*. Empty string clears the override (writes NULL).
//
//	{ "primary_model": "claude-sonnet-4-6",  "judge_model": "" }
//	→ sets primary_model, clears judge_model, leaves the rest alone.

package broker

import (
	"encoding/json"
	"net/http"
)

// adminModelConfigReq mirrors AspectModelConfig but uses *string so the
// handler can distinguish "field omitted (leave alone)" from "field
// set to empty (clear override)". JSON nulls also decode to nil here.
type adminModelConfigReq struct {
	PrimaryModel      *string `json:"primary_model,omitempty"`
	PrimaryCredential *string `json:"primary_credential,omitempty"`
	JudgeModel        *string `json:"judge_model,omitempty"`
	JudgeCredential   *string `json:"judge_credential,omitempty"`
	JudgeProvider     *string `json:"judge_provider,omitempty"` // NEX-365 #3
	CompactModel      *string `json:"compact_model,omitempty"`
	CompactCredential *string `json:"compact_credential,omitempty"`
}

// handleAdminModelConfigGet returns the per-aspect model + credential
// override state. All-null response means no overrides; aspect runs
// purely from keyfile defaults.
func (b *Broker) handleAdminModelConfigGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	cfg, err := b.cfg.Credentials.GetAspectModelConfig(r.Context(), aspect)
	if err != nil {
		b.log.Error("admin model-config get", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleAdminModelConfigSet writes any subset of override columns on
// the aspect row. Each provided field is independent; omitted fields
// are left untouched; empty-string fields are cleared (NULL).
func (b *Broker) handleAdminModelConfigSet(w http.ResponseWriter, r *http.Request) {
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
	var req adminModelConfigReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}

	updates := map[string]*string{
		"primary_model":      req.PrimaryModel,
		"primary_credential": req.PrimaryCredential,
		"judge_model":        req.JudgeModel,
		"judge_credential":   req.JudgeCredential,
		"judge_provider":     req.JudgeProvider,
		"compact_model":      req.CompactModel,
		"compact_credential": req.CompactCredential,
	}
	applied := []string{}
	for col, val := range updates {
		if val == nil {
			continue
		}
		if err := b.cfg.Credentials.SetAspectModelField(r.Context(), aspect, col, *val); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		applied = append(applied, col)
	}
	b.log.Info("admin model-config set", "aspect", aspect, "applied", applied)

	cfg, err := b.cfg.Credentials.GetAspectModelConfig(r.Context(), aspect)
	if err != nil {
		b.log.Error("admin model-config read-back", "aspect", aspect, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"aspect": aspect, "applied": applied})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
