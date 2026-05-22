// In-protocol JWT refresh helper. The validate flow proves identity
// from a sealed keyfile payload; once a WebSocket is authenticated
// against the resulting JWT, we don't need to re-decrypt to issue a
// fresh token — the connection's bound aspect identity is already
// trustworthy. MintSessionFor reuses the validate-time signing path
// (jwt.Sign with the same secret and claim shape) so refreshed tokens
// are wire-indistinguishable from validate-issued ones.

package aspects

import (
	"errors"
	"fmt"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

// RefreshConfig is the subset of ValidateConfig used by MintSessionFor.
// Carries the signing material + clock; no Store or decryption deps
// because the aspect identity is already established by the caller.
type RefreshConfig struct {
	NexusID              string
	SessionSigningSecret []byte
	NewSessionID         func() string
	Now                  func() time.Time
	JWTTTL               time.Duration
}

// MintSessionFor issues a fresh session JWT for an already-identified
// aspect. Used by the broker's session.refresh handler — the WebSocket
// is authenticated, so we skip the keyfile decode/validate dance and
// re-enter the same signing path used at validate time.
//
// The sub claim is the aspect Name verbatim; kfv mirrors the aspect's
// CurrentKeyfileVersion. Caller must supply RefreshConfig with all
// fields populated and a non-nil aspect.
func MintSessionFor(cfg RefreshConfig, a *Aspect) (*ValidatedSession, error) {
	if a == nil || a.Name == "" {
		return nil, errors.New("aspects.MintSessionFor: aspect required")
	}
	if cfg.NexusID == "" || len(cfg.SessionSigningSecret) == 0 ||
		cfg.NewSessionID == nil || cfg.Now == nil || cfg.JWTTTL <= 0 {
		return nil, errors.New("aspects.MintSessionFor: config incomplete")
	}

	now := cfg.Now()
	exp := now.Add(cfg.JWTTTL)
	claims := jwt.Claims{
		Iss: "nexus://" + cfg.NexusID,
		Sub: a.Name,
		Iat: now.Unix(),
		Exp: exp.Unix(),
		Kfv: a.CurrentKeyfileVersion,
		Ses: cfg.NewSessionID(),
	}
	tok, err := jwt.Sign(cfg.SessionSigningSecret, claims)
	if err != nil {
		return nil, fmt.Errorf("aspects.MintSessionFor: jwt sign: %w", err)
	}
	return &ValidatedSession{
		AspectName:     a.Name,
		KeyfileVersion: a.CurrentKeyfileVersion,
		SessionJWT:     tok,
		Claims:         claims,
		ExpiresAt:      exp,
	}, nil
}
