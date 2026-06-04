# credential custodian — design

**Date:** 2026-06-04
**Status:** design
**Scope:** AI-usable password, 2FA, and third-party credential storage for Carried World. Defines **Custodian** as a CWB service beside Herald: a place to store credentials that should not be given to AI directly, and a broker for credentialed acts that can be requested by CWB clients such as Nexus.

## Goal

Allow authenticated CWB clients — including Nexus-hosted aspects and tools — to complete credentialed work without placing raw passwords, TOTP seeds, recovery codes, refresh tokens, cookies, or passkeys in model context.

The design principle:

> The AI may request a credentialed act; it does not receive the credential by default.

Credential use becomes an authenticated, policy-controlled, audited act. Herald authenticates the caller. Custodian decides whether and how a stored credential may be exercised. Ledger supplies work context and records the request/approval/use timeline. Nexus/bridle/toolrunner receive only the narrow handle or result needed to continue the task.

## CWB model

```text
CWB platform
├── herald       identity, auth, assertions, CWB tokens
├── custodian    external credential storage + credential acts
├── ledger       issue tracking, work records, approvals, audit timeline
├── cairn        git hosting, PRs, code review
├── commonplace  knowledge store
└── clients      cw, cwb-client, nexus, agents, operator tools
```

### Existing pieces

- **Herald** authenticates humans and agents, issues CWB tokens, and anchors agent assertions.
- **Nexus** is a CWB consumer/runtime. It already has local credential/config handling for provider keys, but it should not become the platform credential authority.
- **`nexus/cwb/custodian`** already mints, holds, and refreshes per-aspect Herald/CWB access tokens from casket assertions. That package remains a Nexus-side token cache for CWB access; it is not the broader Custodian service defined here.
- **Ledger** is the issue/work/audit plane: tickets, approvals, actor attribution, append-only timeline, and operator-reviewed state transitions.
- **Cairn** is the CWB git/PR/code-review surface — the GitHub analogue.
- **Commonplace** is the CWB knowledge store.
- **Bridle/toolrunner/browser surfaces** execute work but should not be trusted as long-term secret holders.

This spec defines Custodian as the CWB service for third-party credentials and 2FA.

## Boundary

```text
Herald authenticates the caller.
Custodian protects and exercises external secrets.
Ledger records work context, approvals, and audit events.
Nexus requests credentialed acts as a CWB client.
Bridle/toolrunner/browser executes with delegated handles.
```

### Herald responsibilities

- Authenticate humans, agents, and services.
- Issue and refresh CWB tokens.
- Provide caller identity and claims to Custodian.
- Revoke or block callers at the CWB identity layer.

Herald does not store third-party passwords, OTP seeds, recovery codes, browser cookies, or OAuth refresh tokens.

### Custodian responsibilities

- Store encrypted retrievable secrets.
- Register credential records and ownership metadata.
- Store credential policy for which authenticated callers may request which credential acts.
- Generate OTPs, fill forms, exchange OAuth grants, refresh third-party tokens, and create short-lived session handles.
- Check Herald-authenticated caller identity and Custodian policy at the moment of use.
- Return handles/results, not raw secret material, unless policy explicitly permits a break-glass release.

### Ledger responsibilities

- Store issue/work context that justifies a credential-use request.
- Store approval state when a Custodian policy requires a human/operator decision.
- Record immutable audit events for credential requests, decisions, approvals, and use.
- Model credential-use approvals and break-glass flows as first-class workflow/timeline events.

Ledger does **not** store raw passwords, TOTP seeds, recovery codes, session cookies, OAuth refresh tokens, or encrypted secret blobs.

### Nexus responsibilities

- Authenticate to CWB through Herald as a human/agent/aspect.
- Attach task/aspect context to credential requests when it consumes Custodian.
- Route broker decisions into the active execution surface.
- Hold short-lived delegated handles for the current run only.
- Forget/expire handles on turn/session/task completion.

Nexus is a consumer of CWB, not the owner of global credential authority.

### Bridle/toolrunner responsibilities

- Execute with the handle Nexus provides.
- Never log, summarize, or place credential material into model-visible messages.
- Emit observable events that reference credential-use ids, not secret values.

## Threat model

Primary threats:

- Prompt injection asking the model to reveal or misuse credentials.
- Tool output accidentally leaking secrets into chat/session JSONL.
- A compromised aspect requesting broad credential access.
- Stolen session cookie or OAuth refresh token.
- Silent use of a password/TOTP seed without operator intent.
- Confused-deputy use: right credential, wrong task/action.

Design responses:

- Raw secrets are non-model data.
- Every use request is task-bound and actor-bound.
- High-risk acts require human approval or presence.
- Grants are short-lived, scoped, and revocable.
- Audit events are append-only and queryable.
- Break-glass raw release is exceptional, explicit, and noisy.

## Credential classes

| Class | Stored by Custodian | Default AI exposure | Notes |
|---|---:|---|---|
| Password | yes | never | Used by broker-assisted form fill. |
| TOTP seed | yes | never | Broker generates OTP under policy; seed is password-equivalent. |
| Recovery code | yes | never | One-time, high-risk, human approval required. |
| OAuth refresh token | yes | no raw token | Prefer exchanging to short-lived scoped access tokens. |
| API token | yes | scoped release possible | Release only if service has no better delegation path. |
| Session cookie | yes, with expiry | handle only | Dangerous; bind to target/session where possible. |
| Passkey/WebAuthn credential | no raw export | human ceremony | Use as human presence/approval, not as an agent-owned secret. |

## Policy tiers

Credential policy should return one of these tiers:

| Tier | Name | Behavior |
|---|---|---|
| 0 | deny | No credential touched. |
| 1 | request human | Custodian asks Ledger to create/track an approval item; Custodian waits. |
| 2 | broker-assisted use | Custodian fills login fields, generates OTP, or refreshes token outside model context. |
| 3 | delegated handle | Custodian returns a short-lived browser/session/API handle. |
| 4 | scoped raw release | Custodian returns a raw API token/password only for an explicit policy case. |
| 5 | break glass | Operator-approved emergency release; always noisy and time-boxed. |

Default for passwords/TOTP/recovery codes is tier 2 or lower. Default for OAuth/API credentials is tier 3 where possible.

## Request shape

A credential-use request is an intent statement, not a secret lookup.

```json
{
  "request_id": "credreq_...",
  "actor": {
    "kind": "agent",
    "subject": "herald-agent-uuid",
    "aspect": "keel"
  },
  "task": {
    "ledger_ref": "NEX-123",
    "run_id": "nexus-run-...",
    "reason": "comment on the linked GitHub pull request"
  },
  "target": {
    "service": "github",
    "account": "carriedworld-bot",
    "resource": "CarriedWorldUniverse/bridle#50"
  },
  "action": "github.pr.comment",
  "requested_mode": "delegated_handle",
  "risk": {
    "writes": true,
    "destructive": false,
    "billing": false,
    "external_visibility": true
  }
}
```

Custodian evaluates this against Herald-authenticated caller identity, Custodian
credential policy, target account metadata, and any Ledger work/approval state
referenced by the request.

## Decision shape

```json
{
  "decision_id": "creddec_...",
  "request_id": "credreq_...",
  "decision": "allow",
  "mode": "delegated_handle",
  "constraints": {
    "expires_at": "2026-06-04T01:30:00Z",
    "max_uses": 1,
    "resource": "CarriedWorldUniverse/bridle#50",
    "allowed_actions": ["github.pr.comment"]
  },
  "approval": {
    "required": false,
    "approved_by": null
  }
}
```

Custodian must verify that the decision is fresh, bound to the request, and
consistent with any Ledger approval or task state it depends on before
performing the credential act.

## Credential act shape

Credential acts are the things Custodian can actually do.

| Act | Input | Output |
|---|---|---|
| `login.form_fill` | browser session id, target credential id | form fields filled; no secret returned |
| `otp.generate` | credential id, decision id | OTP injected or returned only to trusted runner |
| `oauth.exchange` | credential id, scope, resource | short-lived access token or handle |
| `api_token.issue_handle` | credential id, scope | handle that signs/adds auth server-side |
| `session.attach` | browser session id, cookie credential | browser/session handle |
| `secret.release` | credential id | raw secret; policy tier 4/5 only |

The preferred output is a **handle**:

```json
{
  "handle_id": "credhandle_...",
  "kind": "browser_session",
  "expires_at": "2026-06-04T01:30:00Z",
  "bound_to": {
    "request_id": "credreq_...",
    "run_id": "nexus-run-...",
    "resource": "github.com/CarriedWorldUniverse/bridle/pull/50"
  }
}
```

Handles are opaque to the model. Tooling can use them; chat and session logs should show only ids.

## Ledger data model extension

These tables are conceptual. They exist to record work context, approvals, and
audit, not secret material. Exact names can follow Ledger conventions when
implemented.

### `credential_records`

- `id`
- `service` (`github`, `atlassian`, `openai`, `aws`, `custom`)
- `account_label`
- `owner_subject`
- `credential_class` (`password`, `totp_seed`, `oauth_refresh`, `api_token`, `session_cookie`, `recovery_code`)
- `custodian_ref` (opaque pointer to encrypted material)
- `status` (`active`, `disabled`, `rotating`, `revoked`)
- `created_at`, `updated_at`, `rotated_at`

Credential records may live in Custodian rather than Ledger. If mirrored into
Ledger, the row is metadata-only and must not contain ciphertext or key
material.

### `credential_requests`

- `id`
- `actor_subject`, `actor_aspect`
- `task_ref`, `run_id`
- `target_json`
- `action`
- `requested_mode`
- `risk_json`
- `status` (`requested`, `approved`, `denied`, `used`, `expired`, `revoked`)
- `created_at`, `resolved_at`

### `credential_events`

Append-only event table:

- `id`
- `request_id`
- `credential_id` nullable until resolved
- `kind` (`requested`, `decision`, `approval_requested`, `approved`, `denied`, `act_started`, `act_succeeded`, `act_failed`, `expired`, `revoked`, `break_glass`)
- `actor_subject`
- `at`
- `payload_json`

No raw secret material, ciphertext, wrapped DEKs, or OTP values in any Ledger row.

## Custodian storage model

Custodian stores retrievable secrets with envelope encryption:

- `secret_id`
- `credential_id`
- `version`
- `class`
- `ciphertext`
- `nonce`
- `dek_wrapped`
- `dek_kms_key_id`
- `created_at`
- `rotated_at`
- `destroyed_at`

Encryption requirements:

- Per-secret data encryption key.
- Authenticated encryption.
- Master/wrapping key in OS keychain, KMS, HSM, or age/casket-backed root depending on deployment.
- Secret versions retained only while needed for rotation/rollback.
- Reads require a Herald-authenticated caller and a Custodian policy decision;
  Ledger approval state may be part of that policy decision.

## Browser integration

For website logins, the safest default path is a controlled browser session:

1. A CWB client such as Nexus starts or selects a browser session for the run.
2. Agent navigates but cannot inspect Custodian secrets.
3. Agent requests `login.form_fill` for a target.
4. Custodian checks policy and, if needed, asks Ledger to record/resolve human approval.
5. Custodian injects username/password into the browser.
6. If 2FA appears, Custodian either injects TOTP under policy or asks Ledger for human approval.
7. Browser returns an authenticated session handle.

The model can observe page state and continue work, but it never sees the password or TOTP seed. If an OTP code must be typed into an untrusted browser automation surface, it should be treated as a short-lived secret and redacted from logs.

## Passkeys and human presence

Passkeys/WebAuthn are not a good fit for raw AI-held credentials. Treat them as one of:

- Human approval/presence ceremony for high-risk acts.
- Operator-held authenticator invoked by the browser.
- Future hardware-backed agent authenticator, if Carried World creates an agent-specific key with explicit policy.

Do not design passkeys as exportable secrets in Custodian.

## API sketch

### CWB request shape

```go
type CredentialUseRequest struct {
	RequestID     string
	ActorSubject string
	Aspect       string
	TaskRef      string
	RunID        string
	Target       Target
	Action       string
	RequestedMode string
	Risk         Risk
}
```

### Custodian decision shape

```go
type CredentialDecision struct {
	DecisionID  string
	RequestID   string
	Decision    string // allow | deny | approval_required
	Mode        string
	Constraints Constraints
	ExpiresAt   time.Time
}
```

### CWB service surface

```go
type CustodianService interface {
	RequestCredentialUse(ctx context.Context, req CredentialUseRequest) (CredentialHandle, error)
	Perform(ctx context.Context, handle CredentialHandle, act CredentialAct) (CredentialActResult, error)
	Revoke(ctx context.Context, handleID string) error
	ForgetRun(ctx context.Context, runID string) error
}
```

Nexus consumes this through CWB like any other client. It may keep local helper
packages for browser/toolrunner binding, but Custodian itself is a CWB service.

## Audit rules

- Every request is logged, including denials.
- Every approval has an approver and reason.
- Every act has start and terminal event.
- Every handle has expiry and revocation state.
- Raw releases include a `break_glass` event and operator notification.
- Logs redact secret values and OTPs.
- Tool/session logs may include `request_id`, `decision_id`, `handle_id`, and credential label, never ciphertext/plaintext.

## Error handling

- No matching credential → Ledger denial or `not_found` visible to operator, not model-secret leakage.
- Policy denies → return a model-safe denial reason.
- Approval required → return pending state and notification target.
- Custodian decrypt failure → fail closed, emit `act_failed`, alert operator.
- OTP drift → retry within bounded window; then fail closed.
- Session handle expired → Nexus re-requests through Ledger.
- Refresh token revoked/exhausted → mark credential `rotating` or `revoked`, notify operator.

## Implementation phases

### Phase 0 — spec alignment

- Land this design.
- Treat Custodian as a CWB service beside Herald.
- Confirm Ledger's role as issue/work/audit/approval record, not auth or secret storage.

### Phase 1 — request/decision records

- Add Custodian request/decision primitives and Ledger audit/approval events.
- No secret storage yet.
- CWB clients can create dry-run credential-use requests and operator approvals.

### Phase 2 — encrypted Custodian MVP

- Store password + TOTP seed classes.
- Implement broker-assisted browser form fill and TOTP generation.
- No raw release path except disabled test-only fixtures.

### Phase 3 — delegated handles

- Add OAuth/API-token/session-cookie classes.
- Return short-lived handles bound to task/run/resource.
- Add revocation and expiry sweeps.

### Phase 4 — human-presence flows

- Passkey/WebAuthn handoff.
- Recovery-code approvals.
- Operator UI for pending credential requests.

### Phase 5 — policy hardening

- Risk scoring.
- Anomaly detection.
- Rotation workflows.
- Per-service adapters for GitHub, Atlassian, Google, AWS, OpenAI, and Cairn.

## Non-goals

- Replacing user password managers for humans.
- Letting the model read raw passwords/TOTP seeds by default.
- Making passkeys exportable.
- Storing user-login password hashes for Carried World apps; that is a verifier concern, not this retrievable-secret Custodian.
- Solving every third-party auth flow in v1.

## Resolved decisions

### Service home

Custodian is a **CWB service beside Herald**, not a Nexus package and not a Ledger subservice.

Reason: the credential custodian is authority-adjacent infrastructure, not a
runtime implementation detail. Herald attests identity; Custodian exercises
third-party credentials under policy; Ledger records work, approval, and
audit events. Nexus consumes Custodian through CWB as one client among others,
while still keeping short-lived handles local to its execution surfaces when a
run needs them.

This keeps the long-term boundary clear:

```text
Herald    = who you are
Custodian = what external credentials may be exercised
Ledger    = why/when/who authorized and audited the act
Nexus     = task/runtime context and execution
```

## Open questions

- Should Ledger mirror credential metadata, or should it store only request/approval/audit events that reference Custodian credential ids?
- What is the first target service: GitHub, Atlassian, OpenAI, or browser-generic login?
- Which actions require human presence by default?
- Should handles be usable only by a local toolrunner/browser process, or can remote aspects receive them over CWB?

## References

- `docs/2026-06-03-nexus-token-custodian-design.md`
- `nexus/cwb/custodian`
- Ledger specs and append-only event model
- OWASP Secrets Management / MFA / Password Storage guidance
- NIST SP 800-63B authenticator guidance
