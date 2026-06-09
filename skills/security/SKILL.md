---
name: security
description: Secure-coding rules for nexus + the vulnerability/secret/SAST scan gate run at review and merge.
when_to_use: While writing or reviewing code, and as the gate before merge.
---

# security

## Rules (always)
1. Never print or commit secrets. Pipe keyfiles/tokens via stdin; never echo their contents. Raw production-DB writes need explicit operator authorization.
2. Brokered creds, not raw. Service credentials come scoped via the broker (herald/CWB), resolved lazily through `mcp_profile`. Don't embed raw tokens or static admin tokens — authz is identity-derived.
3. Escape LLM-supplied input that flows into a URL path, query string, or command. For URLs use `url.PathEscape` / `url.Values.Encode` (the `jira.go` pattern).
4. Capability URLs: an unguessable random id can be the gate for an un-authable fetch (e.g. an image the browser `<img>` loads). Generate it with crypto/rand; never make it guessable.
5. Validate and bound inputs (size caps, content-type checks) on anything that accepts external data.

## Dual-use posture
Assist authorized security work: pentest engagements, CTF, defensive security, security research, education. Refuse destructive techniques, denial-of-service, mass-targeting, supply-chain compromise, and detection-evasion for malicious use.

## The scan gate (run at review; must be green to merge)
1. `govulncheck ./...` — dependency CVEs, and whether the vulnerable symbol is actually reachable.
2. `gosec ./...` — SAST: hardcoded creds, weak crypto, injection, unchecked errors on security paths.
3. `gitleaks`/`trufflehog` on the diff — no secret lands in a commit.
4. `osv-scanner` — dependency advisories.
5. The dashboard is no-build, but `DOMPurify` is the XSS boundary — any new HTML-from-content path must go through it.

## Triage a finding
- Is the vulnerable code reachable? `govulncheck` tells you. Unreachable CVEs can be deferred with a note.
- Fix it, or suppress with a written justification — never suppress silently.
- A real injection / secret-leak / authz gap is a blocker. Don't merge past it.

(If a scanner isn't yet wired into CI, it still names the required gate — running it is part of getting to green.)
