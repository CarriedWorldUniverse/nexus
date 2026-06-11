// Derived-identity ("hand") session minting — NEX-571 Task B.
//
// The keyfile validate path strictly requires an aspects row + a
// sealed keyfile, and the broker never holds aspect privkeys — so a
// hand cannot present a keyfile of its own, and it must NOT carry its
// parent's (the credential would then claim the parent's identity).
// The pragmatic v1 credential is therefore a broker-signed session JWT
// for the derived name, minted through the SAME signing path that
// validate (Validate step 6) and session.refresh (MintSessionFor) use:
// jwt.Sign over {iss, sub, iat, exp, kfv, ses} with the broker's
// SessionSigningSecret. The broker's WS upgrade already accepts any
// such JWT (tryVerifyAspectJWT), so no new crypto and no schema change
// — the only delta is the sub being `<parent>.sub-N` and kfv mirroring
// the PARENT's keyfile version, so kfv-based revocation enforcement
// (not yet wired anywhere — see session.refresh, which currently
// re-reads the row without comparing kfv) will fence hands alongside
// their parent when it lands.
//
// When herald-rooted boot lands, DeriveAgentKey replaces this mint;
// the derived-name contract and the persona fallback stay.

package aspects

import (
	"errors"
	"fmt"
)

// MintDerivedSession issues a session JWT for a hand of parent. The
// derived name must be `<parent.Name>.sub-N`; parents that are
// themselves derived are rejected (no sub-of-sub in v1), as are
// retired parents. The hand's claims are wire-indistinguishable from a
// validate-issued aspect JWT apart from the sub naming.
func MintDerivedSession(cfg RefreshConfig, parent *Aspect, derived string) (*ValidatedSession, error) {
	if parent == nil || parent.Name == "" {
		return nil, errors.New("aspects.MintDerivedSession: parent required")
	}
	if parent.Status == StatusRetired {
		return nil, ErrRetired
	}
	if IsDerivedName(parent.Name) {
		return nil, fmt.Errorf("aspects.MintDerivedSession: %q is itself derived (no sub-of-sub)", parent.Name)
	}
	if !IsDerivedName(derived) || BaseName(derived) != parent.Name {
		return nil, fmt.Errorf("aspects.MintDerivedSession: %q is not a hand of %q", derived, parent.Name)
	}
	// Scope is inherited, never widened: the synthetic row carries only
	// the derived NAME plus the parent's keyfile version — everything
	// else (persona, config, credentials) resolves through BaseName at
	// the serving seams.
	return MintSessionFor(cfg, &Aspect{
		Name:                  derived,
		CurrentKeyfileVersion: parent.CurrentKeyfileVersion,
	})
}
