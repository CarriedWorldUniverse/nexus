// In-protocol session.refresh handler.
//
// Aspects send session.refresh over their existing authenticated WS
// to swap an aging JWT for a fresh one without re-running the keyfile
// validate handshake. Identity is taken from the WS bearer that
// authenticated the upgrade (c.auth.AgentID): the keyfile already
// proved the aspect's identity at validate time, and the JWT carries
// that identity for the WS lifetime, so reissuing is just a signing
// op against the same secret.
//
// Rate limit: 1 refresh per aspect per 60s. Defends against a buggy
// or hostile peer pinning a goroutine on this path; legitimate
// refreshes happen at lead-time intervals far longer than the bucket.
//
// Note on session-row expires_at extension: there is no session row
// table in this codebase — JWTs are stateless and the only place an
// expiry surfaces is inside the token's exp claim. The plan's
// session-row update step is therefore N/A; the fresh JWT alone is
// the durable update.

package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// sessionRefreshMinInterval is the rate-limit window. Refreshes
// arriving within this window after the previous accepted refresh
// for the same aspect are rejected with a session.refresh.error
// frame correlated to the request.
const sessionRefreshMinInterval = 60 * time.Second

// handleSessionRefreshFrame mints a fresh session JWT for the aspect
// bound to this connection. Reuses aspects.MintSessionFor so the
// resulting JWT is wire-indistinguishable from a validate-issued one.
func (c *wsConn) handleSessionRefreshFrame(env frames.Envelope) {
	// Caller identity is taken from the authenticated WS — not the
	// payload. Aspects can only refresh their own session; operator
	// connections have no aspect identity here.
	aspectName := c.auth.AgentID
	if aspectName == "" || c.auth.Operator {
		c.respondError(env, "no aspect identity bound to connection")
		return
	}

	v := c.broker.cfg.KeyfileValidator
	if v == nil || v.Store == nil || len(v.SessionSigningSecret) == 0 || v.NexusID == "" {
		c.respondError(env, "session refresh not configured on this broker")
		return
	}

	// Rate-limit: 1/aspect/60s. Check + update under the same lock so
	// two concurrent refreshes can't both pass the gate.
	now := c.broker.refreshNow()
	c.broker.sessionRefreshMu.Lock()
	if last, ok := c.broker.lastSessionRefreshAt[aspectName]; ok {
		if now.Sub(last) < sessionRefreshMinInterval {
			c.broker.sessionRefreshMu.Unlock()
			remaining := sessionRefreshMinInterval - now.Sub(last)
			c.respondError(env, fmt.Sprintf(
				"rate limited: retry in %s", remaining.Round(time.Second)))
			return
		}
	}
	c.broker.lastSessionRefreshAt[aspectName] = now
	c.broker.sessionRefreshMu.Unlock()

	// Lineage (NEX-571): a hand (`<base>.sub-N`) has no aspects row of
	// its own — its credential was broker-minted at spawn time against
	// the parent's row. Refresh resolves the BASE row for kfv/retired
	// state; the reissued sub stays the derived name.
	lookupName := aspectName
	if aspects.IsDerivedName(aspectName) {
		lookupName = aspects.BaseName(aspectName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := v.Store.Get(ctx, lookupName)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			c.respondError(env, "aspect not found")
			return
		}
		c.log.Warn("session.refresh: store lookup failed", "aspect", aspectName, "err", err)
		c.respondError(env, "internal error")
		return
	}
	if a.Status == aspects.StatusRetired {
		c.respondError(env, "aspect retired")
		return
	}

	jwtTTL := v.JWTTTL
	if jwtTTL <= 0 {
		// Match the production default in cmd/nexus/main.go:217 so a
		// misconfigured broker doesn't silently issue 1h refresh JWTs
		// against 24h initial JWTs, which would cause the refresh loop
		// to fire 24× more often than expected.
		jwtTTL = 24 * time.Hour
	}
	cfg := aspects.RefreshConfig{
		NexusID:              v.NexusID,
		SessionSigningSecret: v.SessionSigningSecret,
		NewSessionID:         c.broker.refreshNewSessionID,
		Now:                  c.broker.refreshNow,
		JWTTTL:               jwtTTL,
	}
	var sess *aspects.ValidatedSession
	if lookupName != aspectName {
		// Derived identity: mint for the hand's name against the base row
		// (same path as the spawn-time mint).
		sess, err = aspects.MintDerivedSession(cfg, a, aspectName)
	} else {
		sess, err = aspects.MintSessionFor(cfg, a)
	}
	if err != nil {
		c.log.Warn("session.refresh: mint failed", "aspect", aspectName, "err", err)
		c.respondError(env, "internal error")
		return
	}

	// Telemetry: payload.Reason is free-form and we keep it for logs only.
	var p frames.SessionRefreshPayload
	_ = frames.PayloadAs(env, &p)
	c.log.Info("session.refresh issued",
		"aspect", aspectName, "reason", p.Reason, "expires_at", sess.ExpiresAt)

	resp, rerr := frames.NewResponse(frames.KindSessionRefreshResult, env.ID, frames.SessionRefreshResultPayload{
		SessionJWT:       sess.SessionJWT,
		SessionExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if rerr != nil {
		c.log.Error("session.refresh: build response failed", "err", rerr)
		return
	}
	c.send(resp)
}

// refreshNow returns the broker's effective clock. Prefers the
// OperatorLogin-configured clock (tests pin time there); falls back to
// time.Now in legacy boots without OperatorLogin.
func (b *Broker) refreshNow() time.Time {
	if b.cfg.OperatorLogin != nil && b.cfg.OperatorLogin.Now != nil {
		return b.cfg.OperatorLogin.Now()
	}
	return time.Now()
}

// refreshNewSessionID returns a fresh session-id for a refreshed JWT's
// ses claim. Mirrors OperatorLogin's generator so tests can pin it.
func (b *Broker) refreshNewSessionID() string {
	if b.cfg.OperatorLogin != nil && b.cfg.OperatorLogin.NewSessionID != nil {
		return b.cfg.OperatorLogin.NewSessionID()
	}
	return uuid.NewString()
}
