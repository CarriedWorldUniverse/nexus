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
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type agentCredFetchRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Host string `json:"host,omitempty"` // git: helper-supplied host for scope resolution
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

	// custodian routing (custodian M1): kind="git" goes to the CWB custodian
	// pillar when a client is configured. Additive + fail-safe — when
	// b.cfg.CustodianGit is nil (CUSTODIAN_GRPC_ADDR unset) this branch is
	// skipped and git (like every other kind) falls through to the local
	// store, so there is no regression. Non-git kinds never route here.
	if credentials.Kind(req.Kind) == credentials.KindGit && b.cfg.CustodianGit != nil {
		b.fetchGitViaCustodian(w, r, agentID, req.Host)
		return
	}

	var cred credentials.Credential
	switch {
	case req.Name != "":
		cred, err = cstore.Get(ctx, req.Name)
	case credentials.Kind(req.Kind) == credentials.KindGit:
		// git has no default column; resolve by scope (+ host) instead.
		cred, err = cstore.ResolveGitForAspect(ctx, agentID, req.Host)
	default:
		cred, err = cstore.ResolveDefaultBundle(ctx, agentID, credentials.Kind(req.Kind))
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

	// Mode gate: plaintext fetch requires mode=fetch or mode=both. A
	// proxy-mode credential (e.g. a provider key meant to stay broker-side)
	// must never be returned in plaintext. Mirrors the WS fetch path.
	if cred.Mode != credentials.ModeFetch && cred.Mode != credentials.ModeBoth {
		_ = cstore.RecordAudit(ctx, credentials.AuditEvent{
			CredentialName: cred.Name, Aspect: agentID, Action: credentials.AuditDenied,
		})
		writeError(w, http.StatusForbidden, "credential mode forbids fetch")
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

// fetchGitViaCustodian serves a kind="git" credential.fetch from the CWB
// custodian pillar. The response shape matches the local-store path exactly
// (the cw git-credential-helper consumes {bundle:{username,password,host}}), so
// routing is transparent to the caller. custodian performs its own org-scoped,
// scope-gated, AUDITED fetch — the audit row lives in custodian, not the broker
// store. A custodian error surfaces as the matching HTTP status; there is no
// fall-back to the local store (that would defeat the routing).
func (b *Broker) fetchGitViaCustodian(w http.ResponseWriter, r *http.Request, agentID, host string) {
	if b.cfg.CustodianOrg == "" {
		writeError(w, http.StatusServiceUnavailable, "custodian org not configured")
		return
	}
	if host == "" {
		// git's credential protocol always supplies the host; without it
		// custodian has no credential coordinate (name = host).
		writeError(w, http.StatusBadRequest, "git credential fetch requires a host")
		return
	}
	username, password, gotHost, err := b.cfg.CustodianGit.FetchGit(r.Context(), agentID, b.cfg.CustodianOrg, host)
	if err != nil {
		b.cfg.Logger.Warn("credential.fetch: custodian git fetch failed",
			"agent", agentID, "host", host, "err", err)
		// NotFound / PermissionDenied map to the same outward shape the local
		// path uses (404/403); anything else is a 502 (custodian unreachable).
		writeError(w, custodianHTTPStatus(err), "custodian git fetch failed")
		return
	}
	if gotHost == "" {
		gotHost = host
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentCredFetchResponse{
		Name: gotHost,
		Kind: string(credentials.KindGit),
		Bundle: map[string]any{
			"username": username,
			"password": password,
			"host":     gotHost,
		},
	})
}

// custodianHTTPStatus maps a custodian gRPC error to the broker's outward HTTP
// status, mirroring the local-store path's 404/403 semantics.
func custodianHTTPStatus(err error) int {
	switch grpcstatus.Code(err) {
	case grpccodes.NotFound:
		return http.StatusNotFound
	case grpccodes.PermissionDenied, grpccodes.Unauthenticated:
		return http.StatusForbidden
	case grpccodes.InvalidArgument, grpccodes.FailedPrecondition:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
