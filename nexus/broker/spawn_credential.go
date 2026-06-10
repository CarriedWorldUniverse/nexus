// Derived hand credential minting — NEX-571 Task B.
//
// Ticket-dispatch Jobs receive their identity as the pre-existing k8s
// Secret aspect-keyfile-<agent> (dispatch.K8s.EnsureKeyfileSecret only
// CHECKS it exists — keyfiles are operator-minted and the broker never
// holds privkeys). A hand has no keyfile and must not carry its
// parent's, so the mint path sits BESIDE the keyfile path: the broker
// signs a session JWT for `<parent>.sub-N` (aspects.MintDerivedSession
// — the validate/refresh signing seam, no new crypto) and the Runner
// injects it into the hand's Job env instead of a keyfile volume.
//
// The hand JWT's kfv mirrors the parent's keyfile version so that
// kfv-based revocation enforcement — not yet wired on any verify path;
// see session.refresh — will fence hands together with their parent
// when it lands.

package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// MintDerivedCredential mints the scoped session credential for a hand
// of parent. The parent must be a known, non-retired aspect; lineage
// and no-sub-of-sub rules are enforced by aspects.MintDerivedSession.
// Returns the signed session JWT. Wired into the dispatch Runner's
// MintHandCredential hook by cmd/nexus.
func (v *KeyfileValidator) MintDerivedCredential(ctx context.Context, parent, derived string) (string, error) {
	if v == nil || v.Store == nil {
		return "", fmt.Errorf("derived credential mint: keyfile validator not configured")
	}
	if len(v.SessionSigningSecret) == 0 || v.NexusID == "" {
		return "", fmt.Errorf("derived credential mint: signing material not configured")
	}
	a, err := v.Store.Get(ctx, parent)
	if err != nil {
		return "", fmt.Errorf("derived credential mint: parent %q lookup: %w", parent, err)
	}
	ttl := v.JWTTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	sess, err := aspects.MintDerivedSession(aspects.RefreshConfig{
		NexusID:              v.NexusID,
		SessionSigningSecret: v.SessionSigningSecret,
		NewSessionID:         uuid.NewString,
		Now:                  time.Now,
		JWTTTL:               ttl,
	}, a, derived)
	if err != nil {
		return "", err
	}
	return sess.SessionJWT, nil
}
