// Admin REST surface for per-aspect MCP profiles (NEX-168).
//
// Routes (registered when Config.Credentials is non-nil — the store
// holds the table accessors):
//
//	GET /api/admin/aspects/{name}/mcp_profile  — read profile blob
//	PUT /api/admin/aspects/{name}/mcp_profile  — upsert profile blob
//
// The profile body is the operator-authored MCP-server JSON, kept
// verbatim with credential references as ${credential:NAME.field}
// placeholders. Substitution happens at fetch time via
// credentials.Store.Substitute — never at admin write time, so the
// stored row never contains secret material.

package broker

import (
	"encoding/json"
	"io"
	"net/http"
)

// adminMCPProfileResponse is the GET response shape.
type adminMCPProfileResponse struct {
	Aspect  string `json:"aspect"`
	Profile string `json:"profile"`
}

// handleAdminMCPProfileGet returns the stored profile blob for an
// aspect. Missing rows return "" rather than 404 — callers treat absent
// and empty identically.
func (b *Broker) handleAdminMCPProfileGet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	profile, err := b.cfg.Credentials.GetMCPProfile(r.Context(), aspect)
	if err != nil {
		b.log.Error("admin mcp profile get", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, adminMCPProfileResponse{Aspect: aspect, Profile: profile})
}

// handleAdminMCPProfileSet upserts the profile blob. Body is the raw
// JSON profile — validated as parseable JSON here so syntactically
// broken blobs are rejected at write time rather than failing
// mysteriously at substitution. The funnel-shape validation is the
// agent funnel's responsibility.
func (b *Broker) handleAdminMCPProfileSet(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials store not configured")
		return
	}
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body unreadable")
		return
	}
	// Validate the body is well-formed JSON without binding to a shape.
	// The agent funnel owns the shape contract; the broker only enforces
	// parseability so a typo doesn't sit in the row until first connect.
	var probe any
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	if err := b.cfg.Credentials.SetMCPProfile(r.Context(), aspect, string(body)); err != nil {
		b.log.Error("admin mcp profile set", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	b.log.Info("admin mcp profile set", "aspect", aspect, "size", len(body))
	writeJSON(w, http.StatusOK, adminMCPProfileResponse{Aspect: aspect, Profile: string(body)})
}
