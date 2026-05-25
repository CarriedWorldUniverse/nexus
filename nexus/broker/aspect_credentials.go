// Aspect-side credential fetch handler (NEX-77). Parallel to the
// aspect knowledge handlers — same auth pattern (conn JWT identity),
// same response shape (correlation-id-keyed result frame), same
// audit-row discipline (every fetch writes a row).
//
// Use case: aspects on remote/unsafe hosts (forge in WSL, plumb on
// <operator-host>, etc.) need third-party creds (Jira tokens, IMAP
// passwords, raw API keys for non-proxy paths). The broker holds
// them encrypted-at-rest in the credentials store (#218 + NEX-75);
// this frame lets the aspect fetch the bundle without the key
// living on the remote host's disk.
//
// Per the work-routing in NEX-74:
//   - NEX-79 (nexus-jira-mcp) and NEX-80 (nexus-imap-mcp) consume this
//     frame on startup, shifting away from reading creds out of the
//     keyfile's .jira / .imap blocks.
//   - Provider creds keep flowing through the funnel's ProviderEnv
//     resolver (#218) for turn-time injection — this frame is the
//     fallback for plaintext-fetch (mode=fetch|both) callers that
//     need the raw key.

package broker

import (
	"context"
	"errors"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// handleAspectCredentialFetch answers an aspect-issued credential.fetch.
//
// Required: Kind. Optional: Name. Name unset → broker resolves the
// aspect's default credential for that kind. Name set → broker fetches
// that specific credential, gates on the aspect's allowed_aspects list.
//
// Every successful fetch writes an audit row. Denied fetches (wrong
// allowed_aspects, wrong kind, etc.) write an audit row with
// action=denied so the operator can see attempted-but-rejected access
// in the same audit feed.
func (c *wsConn) handleAspectCredentialFetch(env frames.Envelope) {
	cstore := c.broker.cfg.Credentials
	if cstore == nil {
		c.operatorError(env, "credentials store not configured")
		return
	}
	// Resolve aspect identity from the connection. Two paths produce
	// it: (a) register frame sets c.registeredAs (the legacy chat-MCP
	// path that holds the WS open for push frames), and (b) JWT auth
	// at upgrade time sets c.auth.AgentID (the request/response-only
	// path the NEX-79/80 MCPs use — nexus-jira-mcp + nexus-imap-mcp
	// dial the WS for a single credential.fetch round-trip and never
	// register). Either identity is authoritative. The original gate
	// required c.registeredAs, which broke the MCP startup probe
	// (NEX-74 epic verification, 2026-05-15) — that's what this fix
	// addresses.
	aspect := c.registeredAs
	if aspect == "" {
		aspect = c.auth.AgentID
	}
	if aspect == "" {
		c.operatorError(env, "credential.fetch: no aspect identity (neither registered nor JWT-bound)")
		return
	}
	var p frames.CredentialFetchPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	kindStr := strings.TrimSpace(p.Kind)
	if kindStr == "" {
		c.operatorError(env, "kind is required")
		return
	}
	kind := credentials.Kind(kindStr)
	if !credentials.IsKnownKind(kind) {
		c.operatorError(env, "unknown kind: "+kindStr)
		return
	}

	ctx, cancel := c.opCtx()
	defer cancel()

	// Resolve the credential. By-name takes precedence; absent name we
	// fall through to the aspect's configured default for this kind.
	var (
		cred credentials.Credential
		err  error
	)
	if strings.TrimSpace(p.Name) == "" {
		// For provider kind, ResolveDefaultForAspect needs the shape
		// (anthropic/openai). credential.fetch is meant for non-provider
		// kinds where the resolver doesn't apply (jira/imap have only
		// one shape per kind). Provider-kind default resolution stays
		// on the existing funnel-side path (ProviderEnvResolver). If a
		// caller explicitly requests kind=provider via fetch, they MUST
		// pass a name — the funnel has the shape context this frame
		// lacks.
		if kind == credentials.KindProvider {
			c.operatorError(env, "credential.fetch: name required for kind=provider (provider default resolution happens in the funnel turn path)")
			c.recordCredentialAuditDenied(ctx, aspect, "", kindStr, "provider-default-via-fetch-not-supported")
			return
		}
		cred, err = cstore.ResolveDefaultBundle(ctx, aspect, kind)
		if err != nil {
			if errors.Is(err, credentials.ErrNoDefault) {
				c.operatorError(env, "no default credential configured for aspect for kind="+kindStr)
				return
			}
			if errors.Is(err, credentials.ErrPermission) {
				c.operatorError(env, "aspect not allowed for resolved default credential")
				c.recordCredentialAuditDenied(ctx, aspect, "", kindStr, "not-allowed-for-default")
				return
			}
			c.log.Warn("credential.fetch: resolve default failed",
				"aspect", aspect, "kind", kindStr, "err", err)
			c.operatorError(env, "internal error resolving default credential")
			return
		}
	} else {
		cred, err = cstore.Get(ctx, p.Name)
		if err != nil {
			if errors.Is(err, credentials.ErrNotFound) {
				c.operatorError(env, "credential not found: "+p.Name)
				c.recordCredentialAuditDenied(ctx, aspect, p.Name, kindStr, "not-found")
				return
			}
			c.log.Warn("credential.fetch: get failed",
				"aspect", aspect, "name", p.Name, "err", err)
			c.operatorError(env, "internal error fetching credential")
			return
		}
		if !cred.AllowedFor(aspect) {
			c.operatorError(env, "aspect not allowed for credential "+p.Name)
			c.recordCredentialAuditDenied(ctx, aspect, p.Name, kindStr, "not-allowed")
			return
		}
		if cred.Kind != kind {
			c.operatorError(env, "credential kind mismatch (requested "+kindStr+", stored "+string(cred.Kind)+")")
			c.recordCredentialAuditDenied(ctx, aspect, p.Name, kindStr, "kind-mismatch")
			return
		}
	}

	// Mode gate. The fetch path requires mode=fetch or mode=both —
	// mode=proxy means "key never leaves nexus", which forbids
	// returning the bundle in plaintext over the wire. Provider creds
	// default to mode=proxy in the funnel path; for raw-key fetch the
	// operator opts in by setting mode explicitly. Non-provider kinds
	// (jira/imap) are inherently fetch-shape — the broker has no
	// proxy implementation for them — so the operator-configured mode
	// should be fetch or both. Enforce both rules here.
	if cred.Mode != credentials.ModeFetch && cred.Mode != credentials.ModeBoth {
		c.operatorError(env, "credential mode forbids fetch (mode="+string(cred.Mode)+")")
		c.recordCredentialAuditDenied(ctx, aspect, cred.Name, kindStr, "mode-forbids-fetch")
		return
	}

	bundle, err := cstore.Bundle(cred)
	if err != nil {
		c.log.Warn("credential.fetch: decrypt failed",
			"aspect", aspect, "name", cred.Name, "err", err)
		c.operatorError(env, "internal error decrypting credential")
		return
	}

	// Audit + touch last_used. Best-effort — a failure on either
	// shouldn't block the legitimate fetch from succeeding.
	if err := cstore.RecordAudit(ctx, credentials.AuditEvent{
		CredentialName: cred.Name,
		Aspect:         aspect,
		Action:         credentials.AuditFetch,
		Details:        map[string]any{"kind": kindStr},
	}); err != nil {
		c.log.Warn("credential.fetch: audit write failed",
			"aspect", aspect, "name", cred.Name, "err", err)
	}
	if err := cstore.TouchLastUsed(ctx, cred.Name); err != nil {
		c.log.Debug("credential.fetch: touch last_used failed",
			"aspect", aspect, "name", cred.Name, "err", err)
	}

	resp, err := frames.NewResponse(frames.KindCredentialFetchResult, env.ID, frames.CredentialFetchResultPayload{
		Name:   cred.Name,
		Kind:   string(cred.Kind),
		Bundle: bundle,
	})
	if err != nil {
		c.log.Warn("credential.fetch: build response failed", "err", err)
		return
	}
	c.send(resp)
}

// recordCredentialAuditDenied writes a denied-action audit row.
// Best-effort: if the store write fails we log and move on — the
// denial itself has already been signalled to the caller via
// operatorError, so this is purely audit-trail hygiene.
//
// aspect is passed explicitly (rather than read from c.registeredAs)
// because callers may have resolved identity via c.auth.AgentID when
// the connection didn't register — see handleAspectCredentialFetch's
// identity-resolution comment for context.
func (c *wsConn) recordCredentialAuditDenied(ctx context.Context, aspect, name, kind, reason string) {
	cstore := c.broker.cfg.Credentials
	if cstore == nil {
		return
	}
	if err := cstore.RecordAudit(ctx, credentials.AuditEvent{
		CredentialName: name,
		Aspect:         aspect,
		Action:         credentials.AuditDenied,
		Details:        map[string]any{"kind": kind, "reason": reason},
	}); err != nil {
		c.log.Debug("credential.fetch: audit-denied write failed",
			"aspect", aspect, "kind", kind, "reason", reason, "err", err)
	}
}

// handleAspectModelConfigGet answers an aspect-issued
// aspect.model_config.get request. Returns the AspectModelConfig
// (NEX-263 per-aspect model + credential overrides) for the
// requesting aspect, resolved from the conn's authenticated
// identity. No payload fields — the conn's identity IS the
// authorization (aspects can only read their own row).
//
// NEX-293: out-of-process aspects (anvil, harrow, etc. via
// agentfunnel) need the same admin-override visibility the
// in-process Frame already has via cmd/nexus/main.go's
// applyAspectModelOverrides. This frame is the WS-side equivalent of
// that direct credentials.Store.GetAspectModelConfig call.
//
// Empty/missing aspect row → all-empty-string result (mirrors the
// store's all-nil-pointers semantics). Genuine errors (DB down, etc.)
// surface as operator-error frames so the aspect can fall back to
// keyfile-only config rather than spin on a poison response.
func (c *wsConn) handleAspectModelConfigGet(env frames.Envelope) {
	cstore := c.broker.cfg.Credentials
	if cstore == nil {
		c.operatorError(env, "credentials store not configured")
		return
	}
	aspect := c.registeredAs
	if aspect == "" {
		aspect = c.auth.AgentID
	}
	if aspect == "" {
		c.operatorError(env, "aspect.model_config.get: no aspect identity (neither registered nor JWT-bound)")
		return
	}

	ctx, cancel := c.opCtx()
	defer cancel()

	cfg, err := cstore.GetAspectModelConfig(ctx, aspect)
	if err != nil {
		c.log.Warn("aspect.model_config.get: store read failed",
			"aspect", aspect, "err", err)
		c.operatorError(env, "internal error reading aspect model config")
		return
	}

	resp, err := frames.NewResponse(frames.KindAspectModelConfigGetResult, env.ID, frames.AspectModelConfigGetResultPayload{
		Aspect:            cfg.Aspect,
		PrimaryModel:      strOrEmpty(cfg.PrimaryModel),
		PrimaryCredential: strOrEmpty(cfg.PrimaryCredential),
		JudgeModel:        strOrEmpty(cfg.JudgeModel),
		JudgeCredential:   strOrEmpty(cfg.JudgeCredential),
		CompactModel:      strOrEmpty(cfg.CompactModel),
		CompactCredential: strOrEmpty(cfg.CompactCredential),
	})
	if err != nil {
		c.log.Warn("aspect.model_config.get: build response failed", "err", err)
		return
	}
	c.send(resp)
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
