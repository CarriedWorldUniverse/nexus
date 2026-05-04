// Per-aspect bearer-token authentication and identity resolution.
//
// Implements hand-dispatch v0.1 §5.3 (admin flag for override gestures)
// and §5.4 (identity enforcement on dispatch). Mirrors the pattern
// proven in agent-network's broker/auth.js + reconcileAgentTokens:
//
//   - Each aspect holds its own bearer token, persisted in agent_tokens.
//   - Token → identity resolution gives caller's aspect_id + admin flag.
//   - Dispatch handlers verify caller identity matches payload.Aspect
//     and reject with `identity_mismatch` on drift.
//   - Override handlers (Drift D) check the admin flag and reject with
//     `admin_required` if false.
//
// The reserved id `frame` is the orchestrator/operator-substrate
// identity; its token carries admin=true. Aspect tokens never carry
// admin=true.
//
// Bootstrap path: ReconcileAgentTokens is called at broker startup with
// the known aspect ids (autospawn discovery list). Each id either
// loads its existing token from agent_tokens or mints a fresh one and
// persists. ReconcileFrameToken does the same for the special `frame`
// identity, with admin=true.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
)

// FrameAgentID is the reserved identity for the Frame role. Its token
// carries admin=true; it is the only identity authorized to invoke
// override gestures (abort, kill worker, force-shutdown, take-surface-
// offline) per §5.3.
const FrameAgentID = "frame"

// TokenInfo is the resolved identity for a presented bearer token.
// Returned from ResolveToken; cached in the per-connection state and
// the per-request authUser context so downstream handlers can read
// caller identity without re-resolving.
type TokenInfo struct {
	AgentID string
	Admin   bool
	// ViaLegacy is true when the bearer was matched via the legacy
	// master fallback (vs a per-aspect token). Connect handlers WARN
	// on this so operators can track migration progress; once all
	// aspects rotate to per-aspect tokens, AllowLegacyMaster gets
	// flipped off and ViaLegacy becomes unreachable.
	ViaLegacy bool
}

// TokenStore holds the in-memory token map and provides the resolve /
// mint / reconcile API. Concurrency-safe; safe to share across
// goroutines.
//
// One TokenStore lives per Broker. The store's source of truth is
// the agent_tokens table; the in-memory map is a hot cache populated
// at reconcile time and on subsequent loads.
type TokenStore struct {
	mu sync.RWMutex
	// byToken maps the bearer string → its TokenInfo. Lookup is by
	// presented token; reverse direction (agent → token) lives in
	// byAgent for the test/debug helpers.
	byToken map[string]TokenInfo
	byAgent map[string]string // agent_id → token

	// legacyMaster is an optional shared token kept for back-compat
	// with the pre-drift-C single-AuthToken model. When set, presenting
	// this token resolves to {AgentID: FrameAgentID, Admin: true} —
	// the operator/orchestrator master path. Empty means no legacy
	// fallback.
	legacyMaster string
}

// NewTokenStore returns an empty store. Use ReconcileAgentTokens and
// ReconcileFrameToken to populate.
func NewTokenStore() *TokenStore {
	return &TokenStore{
		byToken: make(map[string]TokenInfo),
		byAgent: make(map[string]string),
	}
}

// SetLegacyMaster registers a shared master token that resolves to
// the Frame identity (admin=true). Used as a back-compat shim during
// the per-aspect-auth migration so existing callers (autospawn
// passing NEXUS_TOKEN, outpost) keep working until they rotate to
// per-aspect tokens. Pass empty string to disable.
func (s *TokenStore) SetLegacyMaster(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyMaster = token
}

// GenerateAgentToken returns a fresh 32-byte random hex token. Mirrors
// agent-network's crypto.randomBytes(32).toString('hex').
func GenerateAgentToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: token rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ReconcileAgentTokens loads or mints a token for each id in ids,
// persisting any newly minted ones to agent_tokens. After return, the
// in-memory map covers every id requested. Calling twice with the same
// ids is a no-op (idempotent).
//
// admin defaults to false for these ids — aspect tokens never carry
// admin=true. Use ReconcileFrameToken for the Frame identity.
func (s *TokenStore) ReconcileAgentTokens(ctx context.Context, db *sql.DB, ids []string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := s.reconcileOne(ctx, db, id, false); err != nil {
			return fmt.Errorf("reconcile %q: %w", id, err)
		}
	}
	return nil
}

// ReconcileFrameToken loads or mints the default Frame identity token
// (FrameAgentID) and persists with admin=true. Used by callers that
// don't yet know the operator-chosen Frame name (legacy startup paths
// that pre-date §6.5's data-driven Frame identity). New callers should
// prefer ReconcileFrameTokenFor with the actual name from frame.Detect.
func (s *TokenStore) ReconcileFrameToken(ctx context.Context, db *sql.DB) (string, error) {
	return s.ReconcileFrameTokenFor(ctx, db, FrameAgentID)
}

// ReconcileFrameTokenFor loads or mints an admin-flagged token for the
// supplied identity. Returns the token string so the caller can wire it
// into the embedded Frame's auth context.
//
// Per §6.5, the Frame's name is operator-chosen at first-boot and lives
// in aspect.json, not as a hardcoded constant. Callers in P5+ pass that
// name here so the persisted token row matches the Frame's actual id.
//
// Failure mode: reconcileOne enforces "stored value wins" — if a row
// already exists with admin=0 (e.g., the operator promoted a previously
// non-admin aspect to be the Frame), the loaded token resolves with
// admin=false, which silently breaks every Frame admin call downstream.
// Surface this loudly here rather than letting the failure surface at
// the first /api/admin gate. Operator must clear the offending row by
// hand.
func (s *TokenStore) ReconcileFrameTokenFor(ctx context.Context, db *sql.DB, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("auth: frame name required")
	}
	if err := s.reconcileOne(ctx, db, name, true); err != nil {
		return "", err
	}
	s.mu.RLock()
	tok := s.byAgent[name]
	info := s.byToken[tok]
	s.mu.RUnlock()
	if !info.Admin {
		return "", fmt.Errorf("auth: frame token for %q resolved admin=false; agent_tokens row predates Frame promotion — clear the row and re-run", name)
	}
	return tok, nil
}

// reconcileOne is the shared mint-or-load core. If a row exists in
// agent_tokens for this id, load it. If admin disagrees with the
// stored value, the stored value wins (tokens don't get re-elevated
// silently — operator must update the row by hand to elevate). For
// brand-new ids, mint and persist with the requested admin flag.
func (s *TokenStore) reconcileOne(ctx context.Context, db *sql.DB, id string, admin bool) error {
	if db == nil {
		// Test/in-memory mode: mint a token without persistence so
		// non-DB-backed brokers can still exercise the auth path.
		return s.mintInMemory(id, admin)
	}

	var token string
	var dbAdmin int
	row := db.QueryRowContext(ctx,
		`SELECT token, admin FROM agent_tokens WHERE agent_id = ?`, id)
	err := row.Scan(&token, &dbAdmin)
	switch {
	case err == sql.ErrNoRows:
		// Mint and persist.
		fresh, gerr := GenerateAgentToken()
		if gerr != nil {
			return gerr
		}
		adminVal := 0
		if admin {
			adminVal = 1
		}
		if _, ierr := db.ExecContext(ctx,
			`INSERT INTO agent_tokens(agent_id, token, admin) VALUES(?, ?, ?)`,
			id, fresh, adminVal); ierr != nil {
			return fmt.Errorf("insert agent_token: %w", ierr)
		}
		token = fresh
		dbAdmin = adminVal
	case err != nil:
		return fmt.Errorf("select agent_token: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// If a previous reconcile cached a different token for this id,
	// drop the old mapping before installing the new one. (Shouldn't
	// happen in normal flow but cheap insurance.)
	if old, ok := s.byAgent[id]; ok && old != token {
		delete(s.byToken, old)
	}
	s.byAgent[id] = token
	s.byToken[token] = TokenInfo{AgentID: id, Admin: dbAdmin == 1}
	return nil
}

// mintInMemory is the no-DB path used by tests and embedded scenarios.
// Tokens minted here are not persisted; on broker restart they would
// be regenerated.
func (s *TokenStore) mintInMemory(id string, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byAgent[id]; ok {
		return nil // already cached
	}
	t, err := GenerateAgentToken()
	if err != nil {
		return err
	}
	s.byAgent[id] = t
	s.byToken[t] = TokenInfo{AgentID: id, Admin: admin}
	return nil
}

// SetTokenForTest installs a known token for an agent without going
// through the DB. Test-only; production paths use Reconcile*.
func (s *TokenStore) SetTokenForTest(agentID, token string, admin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byAgent[agentID]; ok {
		delete(s.byToken, old)
	}
	s.byAgent[agentID] = token
	s.byToken[token] = TokenInfo{AgentID: agentID, Admin: admin}
}

// TokenForAgent returns the bearer token currently mapped to agentID,
// or empty string if not found. Used by the autospawn pipeline to
// inject per-aspect NEXUS_TOKEN into harness env, and by tests.
func (s *TokenStore) TokenForAgent(agentID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byAgent[agentID]
}

// ResolveToken maps a presented bearer token to its TokenInfo. Returns
// (info, true) on hit, (zero, false) on miss.
//
// Compares against every registered token and the legacy-master
// fallback using subtle.ConstantTimeCompare, never short-circuiting on
// the first match. Total work is O(N) per resolve where N = registered
// aspects (~10 in practice); negligible.
//
// Threat model + residual leak: the previous primary path was
// `s.byToken[token]`, a Go map lookup. Map ops branch on hash bucket
// layout, so hit-vs-miss had a microsecond-class timing differential
// that leaked "is this token registered to anyone" to a remote prober.
// This implementation closes that channel — the loop body runs the
// same number of byte-compares regardless of match outcome, and
// ConstantTimeCompare itself doesn't branch on contents.
//
// What this code does NOT achieve: true constant-time STRUCT capture.
// The `if eq == 1 { hit = info }` line is a regular branch on the
// compare result. Strings in TokenInfo can't be selected via
// subtle.ConstantTimeSelect (which is integer-only), so a hit assigns
// a string while a miss does not — a few-nanosecond branch-predictor
// differential. That residue is far below the previous map-lookup
// channel and well below network jitter on any plausible attacker
// path; treating it as acceptable for this threat model. If we ever
// need to close it, the path is: store candidates in a slice and use
// fixed-width index arithmetic to pick the hit, eating a string copy
// on every call.
func (s *TokenStore) ResolveToken(token string) (TokenInfo, bool) {
	if token == "" {
		return TokenInfo{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	tokenBytes := []byte(token)
	var hit TokenInfo
	var found int // 1 if any constant-time compare succeeded

	for candidate, info := range s.byToken {
		// ConstantTimeCompare returns 0 on length mismatch without
		// branching on contents; ORing into found is constant-time.
		eq := subtle.ConstantTimeCompare(tokenBytes, []byte(candidate))
		if eq == 1 {
			// Branch on eq — not on token contents. See the residual-leak
			// note in the doc comment.
			hit = info
		}
		found |= eq
	}
	// Legacy-master compare runs only if no per-aspect token matched.
	// If a per-aspect token's bytes ever collide with the legacy
	// master string (impossible for production-minted random tokens,
	// but possible if an operator manually sets NEXUS_TOKEN to a
	// value that happens to match a registered aspect token), the
	// per-aspect identity wins. Without this guard, the legacy branch
	// would unconditionally overwrite `hit` with admin=true — a
	// silent privilege escalation. Trade-off: this branch runs only
	// on miss, which is a deviation from strict constant-time across
	// every call. The threat-model comment already accepts the
	// branch-predictor residue; correctness wins here.
	if found == 0 && s.legacyMaster != "" {
		eq := subtle.ConstantTimeCompare(tokenBytes, []byte(s.legacyMaster))
		if eq == 1 {
			hit = TokenInfo{AgentID: FrameAgentID, Admin: true, ViaLegacy: true}
		}
		found |= eq
	}
	return hit, found == 1
}

// ExtractBearer parses an Authorization header value, returning the
// token string after "Bearer " or empty if the header is missing or
// malformed. Uses constant-time compare on the prefix so a probe can't
// distinguish "missing header" from "wrong prefix" via timing.
func ExtractBearer(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(header[:len(prefix)]), []byte(prefix)) != 1 {
		return ""
	}
	return header[len(prefix):]
}

// RequireAdmin is the helper Drift D's override handlers will call
// after resolving the caller's TokenInfo. Returns nil if the caller is
// admin; otherwise an error suitable for translating into a 403
// admin_required response.
//
// Lives here (not in the Drift D files yet) per the task spec: "lay
// the groundwork — when a future override handler runs, it MUST check
// tokenInfo.Admin and reject 403 admin_required if false."
func RequireAdmin(info TokenInfo) error {
	if info.Admin {
		return nil
	}
	return ErrAdminRequired
}

// ErrAdminRequired is returned by RequireAdmin for non-admin callers.
// Sentinel so override handlers can errors.Is-test it in Drift D
// without importing this file's exact text.
var ErrAdminRequired = fmt.Errorf("admin_required")
