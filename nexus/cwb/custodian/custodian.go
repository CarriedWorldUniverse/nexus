// Package custodian mints, holds, and refreshes per-aspect herald tokens (casket
// jwt-bearer + refresh_token grants) and yields a cwb-client authed AS an aspect.
// In-memory: tokens are ephemeral derived state (nexus owns the persistent state).
// Bootstrap step 2 — fed assertions by the WS register handshake (step 3).
package custodian

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
)

// skew refreshes a little before expiry to avoid races.
const skew = 60 * time.Second

// Custodian holds per-aspect herald tokens keyed by subject (agent UUID).
type Custodian struct {
	edge string
	oc   *oidc.Client
	mu   sync.RWMutex
	by   map[string]*entry
}

type entry struct {
	mu      sync.Mutex // serialises this subject's refresh
	access  string
	refresh string
	exp     time.Time
}

// New builds a Custodian targeting one herald edge.
func New(edge string) *Custodian {
	return &Custodian{edge: edge, oc: oidc.New(edge), by: map[string]*entry{}}
}

// Redeem exchanges a casket assertion for a herald token, custodies it, and
// returns the subject (agent UUID).
func (c *Custodian) Redeem(ctx context.Context, assertion string) (string, error) {
	tok, err := c.oc.JWTBearerGrant(ctx, assertion)
	if err != nil {
		return "", fmt.Errorf("custodian: redeem: %w", err)
	}
	claims, err := identity.DecodeAccessClaims(tok.AccessToken)
	if err != nil {
		return "", fmt.Errorf("custodian: decode redeemed token: %w", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("custodian: redeemed token has no subject")
	}
	c.mu.Lock()
	c.by[sub] = &entry{
		access:  tok.AccessToken,
		refresh: tok.RefreshToken,
		exp:     time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
	}
	c.mu.Unlock()
	return sub, nil
}

// Client returns a cwb-client authed AS subject. Error if subject was not Redeem'd.
func (c *Custodian) Client(subject string) (*client.Client, error) {
	if _, ok := c.lookup(subject); !ok {
		return nil, fmt.Errorf("custodian: no token for %q (not redeemed)", subject)
	}
	return client.New(c.edge, &source{cust: c, subject: subject}), nil
}

// Forget drops a subject's custodied token (on disconnect).
func (c *Custodian) Forget(subject string) {
	c.mu.Lock()
	delete(c.by, subject)
	c.mu.Unlock()
}

func (c *Custodian) lookup(subject string) (*entry, bool) {
	c.mu.RLock()
	e, ok := c.by[subject]
	c.mu.RUnlock()
	return e, ok
}

// source is the per-aspect client.TokenSource.
type source struct {
	cust    *Custodian
	subject string
}

func (s *source) Token(ctx context.Context) (string, error) {
	e, ok := s.cust.lookup(s.subject)
	if !ok {
		return "", client.ErrReauth
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Until(e.exp) > skew {
		return e.access, nil
	}
	return s.refreshLocked(ctx, e)
}

func (s *source) Refresh(ctx context.Context) (string, error) {
	e, ok := s.cust.lookup(s.subject)
	if !ok {
		return "", client.ErrReauth
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return s.refreshLocked(ctx, e)
}

// refreshLocked runs the refresh_token grant; the caller holds e.mu. The herald
// HTTP call happens here under the per-entry mutex only (never the map lock).
func (s *source) refreshLocked(ctx context.Context, e *entry) (string, error) {
	if e.refresh == "" {
		return "", client.ErrReauth
	}
	tok, err := s.cust.oc.RefreshGrant(ctx, e.refresh)
	if err != nil {
		return "", client.ErrReauth
	}
	e.access = tok.AccessToken
	if tok.RefreshToken != "" {
		e.refresh = tok.RefreshToken
	}
	e.exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return e.access, nil
}
