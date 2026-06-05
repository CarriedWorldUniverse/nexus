// Agent credential seam (NEX-435).
//
//	POST /api/agent/credential.fetch
//	  body: { "kind": "git" | "provider" | ..., "name": "<optional>" }
//	  auth: the agent's own session JWT (sub claim) — NOT admin
//	  response: { "name", "kind", "bundle", "expires_at" }
//
// The HTTP counterpart of the WS credential.fetch frame
// (handleAspectCredentialFetch): a process holding the agent's session
// JWT (e.g. the cw git-credential-helper on a k3s worker) fetches a
// scoped, audited credential bundle without an aspect WS connection.
// Scope + audit are identical — AllowedFor gate, RecordAudit on
// fetch/deny. This is the "custodian seam" contract; M1 serves it from
// the broker store, a later milestone serves the same shape from the
// CWB custodian service (NEX-438).
package broker

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

type agentCredFetchRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type agentCredFetchResponse struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Bundle    map[string]any `json:"bundle"`
	ExpiresAt string         `json:"expires_at,omitempty"`
}

func (b *Broker) handleAgentCredentialFetch(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil {
		writeError(w, http.StatusServiceUnavailable, "validator not configured")
		return
	}
	cstore := b.cfg.Credentials
	if cstore == nil {
		writeError(w, http.StatusServiceUnavailable, "credential store not configured")
		return
	}

	// Auth: the agent's own session JWT. The sub claim is the only
	// source of the caller identity — never a body field. Mirrors
	// handleAspectSelfEdit's load-bearing invariant.
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	claims, err := jwt.Verify(v.SessionSigningSecret, token, time.Now())
	if err != nil || claims.Sub == "" {
		writeError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	agentID := claims.Sub

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var req agentCredFetchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Kind == "" {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}

	ctx := r.Context()
	var cred credentials.Credential
	if req.Name == "" {
		cred, err = cstore.ResolveDefaultBundle(ctx, agentID, credentials.Kind(req.Kind))
	} else {
		cred, err = cstore.Get(ctx, req.Name)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}

	// Scope gate: kind must match and the credential must be allowed for
	// this agent. Denials are audited.
	if string(cred.Kind) != req.Kind || !cred.AllowedFor(agentID) {
		_ = cstore.RecordAudit(ctx, credentials.AuditEvent{
			CredentialName: cred.Name, Aspect: agentID, Action: credentials.AuditDenied,
		})
		writeError(w, http.StatusForbidden, "credential not allowed for this agent")
		return
	}

	bundle, err := cstore.Bundle(cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "decrypt failed")
		return
	}
	_ = cstore.RecordAudit(ctx, credentials.AuditEvent{
		CredentialName: cred.Name, Aspect: agentID, Action: credentials.AuditFetch,
	})
	_ = cstore.TouchLastUsed(ctx, cred.Name)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentCredFetchResponse{
		Name: cred.Name, Kind: string(cred.Kind), Bundle: bundle,
	})
}
