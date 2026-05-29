// admin.js — REST wrappers for /api/admin/* endpoints (NEX-265 onward).
//
// These hit the admin REST surface mounted by registerAdmin in
// nexus/broker/admin.go, which is gated by requireAdmin server-side.
// Token comes from the same in-memory holder api.js uses for other
// REST + WS calls. A 403 here means the operator isn't admin
// (shouldn't happen since SettingsView is admin-gated, but surfaces
// cleanly if the gate is bypassed).
//
// Each wrapper returns the parsed JSON body on 2xx, throws on
// non-2xx with an Error carrying the status + best-effort message.

import { getAuthToken } from '../api.js';

function authHeaders(extra) {
  const h = { 'Authorization': 'Bearer ' + (getAuthToken() || '') };
  if (extra) Object.assign(h, extra);
  return h;
}

async function adminFetch(path, init) {
  const res = await fetch(path, {
    ...init,
    headers: authHeaders((init && init.headers) || {}),
    cache: 'no-store',
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.text();
      if (body) msg = body;
    } catch (_) { /* fall through */ }
    const err = new Error('admin: ' + res.status + ' ' + msg);
    err.status = res.status;
    throw err;
  }
  // 204 (no content) returns null; other 2xx returns parsed JSON.
  if (res.status === 204) return null;
  return res.json();
}

// GET /api/admin/aspects/all
// Returns every known aspect (active + retired, live + offline) with
// {name, status, provider, model, live}. Use this instead of the
// roster-only fetchAgents() when Settings needs to render rows for
// offline aspects too.
export function listAllAspects() {
  return adminFetch('/api/admin/aspects/all').then((b) => (b && b.aspects) || []);
}

// GET /api/admin/aspects/{name}/model-config
// Returns AspectModelConfig (per credentials.AspectModelConfig). All
// override fields are null when unset (inherit keyfile).
export function getModelConfig(aspect) {
  return adminFetch('/api/admin/aspects/' + encodeURIComponent(aspect) + '/model-config');
}

// PUT /api/admin/aspects/{name}/model-config
// payload is a partial object — only provided fields are written.
// Empty-string fields clear the override (write NULL).
//
//   setModelConfig('anvil', { primary_model: 'claude-opus-4-7' })
//     → set primary_model, leave others alone
//   setModelConfig('anvil', { primary_model: '' })
//     → clear primary_model override
export function setModelConfig(aspect, payload) {
  return adminFetch('/api/admin/aspects/' + encodeURIComponent(aspect) + '/model-config', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload || {}),
  });
}

// GET /api/admin/credentials
// Returns { credentials: [Metadata] }. Optional kind filter passes
// through as ?kind=<provider|jira|imap>. No bundle material returned.
export function listCredentials(kind) {
  const q = kind ? '?kind=' + encodeURIComponent(kind) : '';
  return adminFetch('/api/admin/credentials' + q);
}

// GET /api/admin/credentials/{name}
// Returns Metadata only (name, description, kind, allowed_aspects,
// mode, timestamps). Bundle is never returned — Edit flow must
// re-enter bundle fields.
export function getCredential(name) {
  return adminFetch('/api/admin/credentials/' + encodeURIComponent(name));
}

// PUT /api/admin/credentials/{name}
// Upsert (create or replace). Body accepts:
//   { kind: 'provider'|'jira'|'imap', bundle: {...kind-specific...},
//     description, allowed_aspects: [...], mode: 'proxy'|'fetch'|'both' }
// Bundle replaces entirely on update — operator must re-enter secret
// fields. Backend validates kind-specific shape and rejects with 400 +
// human-readable message.
export function upsertCredential(name, payload) {
  return adminFetch('/api/admin/credentials/' + encodeURIComponent(name), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload || {}),
  });
}

// DELETE /api/admin/credentials/{name}
// Removes the credential. Aspects with this as their default will fall
// back to the next resolution step (keyfile / env) or fail.
export function deleteCredential(name) {
  return adminFetch('/api/admin/credentials/' + encodeURIComponent(name), {
    method: 'DELETE',
  });
}

// GET /api/admin/aspects/{name}/credential-defaults
// Returns { aspect, default_anthropic_credential?, default_openai_credential?,
// default_jira_credential?, default_imap_credential? }. Fields are absent
// (or null) when the default is unset.
export function getCredentialDefaults(aspect) {
  return adminFetch('/api/admin/aspects/' + encodeURIComponent(aspect) + '/credential-defaults');
}

// PUT /api/admin/aspects/{name}/credential-defaults
// Partial update. payload uses the same field names as the GET response.
//   field omitted        → no change
//   field set to ""      → clear (NULL in column)
//   field set to "name"  → set to that credential name
// Backend validates that the named credential exists; 400 on mismatch.
export function setCredentialDefaults(aspect, payload) {
  return adminFetch('/api/admin/aspects/' + encodeURIComponent(aspect) + '/credential-defaults', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload || {}),
  });
}

// GET /api/admin/network-defaults (NEX-294 Slice 2)
// Returns NetworkDefaults: { judge_model, judge_credential,
// judge_provider, compact_model, compact_credential } — each an empty
// string when unset. Applies as fallback when per-aspect override is
// blank. Primary fields intentionally absent (primary is per-aspect).
// judge_provider (NEX-365 #3) selects the provider family the cheap-
// judge runs on (claude-api / claude-code / openai), independent of the
// aspect's primary provider — enables one cheap cross-provider judge
// endpoint network-wide.
export function getNetworkDefaults() {
  return adminFetch('/api/admin/network-defaults');
}

// PUT /api/admin/network-defaults (NEX-294 Slice 2)
// Partial update — same semantics as setModelConfig:
//   field omitted        → no change
//   field set to ""      → clear (NULL in column)
//   field set to "name"  → set to that credential name (or model id)
// Backend validates credential existence on judge_credential /
// compact_credential. Returns the post-update NetworkDefaults.
export function setNetworkDefaults(payload) {
  return adminFetch('/api/admin/network-defaults', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload || {}),
  });
}

// GET /api/admin/credentials/{name}/audit?limit=N
// Returns { audit: [AuditRow] } most-recent first. Backend caps limit
// at 1000; default 100. AuditRow shape: { id, credential_name, aspect,
// action, ts, details }.
export function getCredentialAudit(name, opts) {
  const limit = opts && opts.limit ? '?limit=' + encodeURIComponent(opts.limit) : '';
  return adminFetch('/api/admin/credentials/' + encodeURIComponent(name) + '/audit' + limit);
}
