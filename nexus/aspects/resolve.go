// Persona/config resolution for an ALREADY-AUTHENTICATED aspect name —
// the JWT-boot path for hands (NEX-571 Task D).
//
// A spawned hand boots with a broker-minted session JWT (CW_SESSION_JWT)
// and no keyfile, so it cannot run the keyfile Validate handshake. But
// it still needs its persona bundle + provider/model to start the
// funnel. ResolveByName serves exactly that, keyed on the verified
// aspect name (the JWT's sub, already signature-checked by the caller):
// it does the SAME persona/provider/central resolution Validate's
// steps 7-8 do — including the derived-name → BASE-aspect fallback — but
// skips the seal-open + keyfile-version + pubkey crypto, because the JWT
// already proved identity.
//
// This keeps one resolution path: a hand sees the same persona, the
// same central nexus_md, and the PARENT's provider binding (provider
// inheritance) as the base aspect would on a keyfile boot.

package aspects

import (
	"context"
	"errors"
	"fmt"
)

// ResolveConfigByName packages the dependencies ResolveByName needs.
// It is the read-only subset of ValidateConfig — no signing keys,
// because the JWT is already minted and verified by the caller.
type ResolveConfigByName struct {
	// Store is the aspects backend.
	Store Store

	// Settings is the nexus_settings backend (Part 9). Optional — nil
	// leaves CentralNexusMD empty (legacy shape).
	Settings SettingsStore
}

// ResolvedIdentity is the persona/config bundle ResolveByName returns:
// the non-credential subset of ValidatedSession (no JWT — the caller
// already holds it).
type ResolvedIdentity struct {
	// AspectName is the verified name as presented (the derived name for
	// a hand, NOT the base — lineage stays truthful on the wire).
	AspectName string

	// Provider/Model are the BASE aspect's binding (provider inheritance
	// for hands).
	Provider string
	Model    string

	// Personality is the BASE aspect's bundle (derived names share the
	// parent's persona). nil when no row exists.
	Personality *Personality

	// CentralNexusMD / CentralVersion mirror Validate's step 8.
	CentralNexusMD string
	CentralVersion int64
}

// ResolveByName resolves the persona + provider/model + central content
// for an already-authenticated aspect name. For a derived hand name
// (`<base>.<word>`) every lookup keys on BaseName(name): the hand shares
// the parent's persona, config, and provider binding (inheritance). A
// retired base aspect is rejected. Unknown aspect → ErrUnknownAspect.
func ResolveByName(ctx context.Context, cfg ResolveConfigByName, name string) (*ResolvedIdentity, error) {
	if cfg.Store == nil {
		return nil, errors.New("aspects.ResolveByName: Store nil")
	}
	if name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrMalformedPayload)
	}

	// The identity whose persona/config/provider applies. For a hand
	// this is the parent; for a base aspect it's the name itself.
	base := BaseName(name)

	a, err := cfg.Store.Get(ctx, base)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrUnknownAspect
	}
	if err != nil {
		return nil, fmt.Errorf("aspects.ResolveByName: lookup %q: %w", base, err)
	}
	if a.Status == StatusRetired {
		return nil, ErrRetired
	}

	personality, err := cfg.Store.PersonalityGet(ctx, base)
	if errors.Is(err, ErrNotFound) {
		personality = nil
	} else if err != nil {
		return nil, fmt.Errorf("aspects.ResolveByName: personality lookup: %w", err)
	}

	var centralContent string
	var centralVersion int64
	if cfg.Settings != nil {
		if ns, sErr := cfg.Settings.Get(ctx); sErr == nil {
			centralContent = ns.NexusMD
			centralVersion = ns.Version
		}
		// Soft-fail on a transient settings read, mirroring Validate.
	}

	return &ResolvedIdentity{
		AspectName:     name,
		Provider:       a.Provider,
		Model:          a.Model,
		Personality:    personality,
		CentralNexusMD: centralContent,
		CentralVersion: centralVersion,
	}, nil
}
