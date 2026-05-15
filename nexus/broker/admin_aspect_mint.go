// Admin-mediated aspect minting (NEX-134).
//
// Operator's CLI generates an aspect keypair locally, POSTs the pubkey
// + metadata to the broker over admin REST. The broker validates state,
// runs the same INSERT/BumpKeyfileVersion path the offline CLI uses,
// and returns the data the CLI needs to seal the aspect's privkey
// against the server pubkey + write the keyfile locally.
//
// The aspect privkey never leaves the operator's machine. The broker
// only ever sees the pubkey + the resulting aspect row state.
//
// Pre-#134 the CLI was direct-DB which (a) bypassed the broker as the
// authority for who can join the cluster and (b) couldn't be run safely
// while the broker held the database open. This admin path makes the
// broker the single writer.

package broker

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// AdminMintRequest is the JSON body the operator's CLI POSTs to
// /api/admin/aspects/mint. AspectPubkeyB64 is the CLI-generated Ed25519
// public key (32 bytes, base64-std encoded). Provider/Model land on the
// aspects row; Force suppresses the "re-minting invalidates the prior
// keyfile" warning that the CLI surfaces, mirroring the offline flag.
type AdminMintRequest struct {
	Name            string `json:"name"`
	AspectPubkeyB64 string `json:"aspect_pubkey_b64"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	Force           bool   `json:"force,omitempty"`
}

// AdminMintResponse is the JSON the broker returns. CLI uses these to
// call aspects.Mint(...) and build the keyfile on its end.
type AdminMintResponse struct {
	Name              string `json:"name"`
	KeyfileVersion    int64  `json:"keyfile_version"`
	NexusID           string `json:"nexus_id"`
	ServerPubkeyB64   string `json:"server_pubkey_b64"`
	PreviouslyExisted bool   `json:"previously_existed"`
}

// MintAspect runs the broker-side half of an admin-mediated aspect
// mint. It does not touch any keyfile bytes — the CLI handles the
// privkey + sealing entirely. Returns the data the CLI needs to
// complete the keyfile.
func (v *KeyfileValidator) MintAspect(ctx context.Context, req AdminMintRequest) (*AdminMintResponse, error) {
	if v == nil {
		return nil, errors.New("keyfile validator not configured")
	}
	if v.Store == nil {
		return nil, errors.New("keyfile validator: aspects store not configured")
	}
	if req.Name == "" {
		return nil, errors.New("mint: name required")
	}
	pub, err := base64.StdEncoding.DecodeString(req.AspectPubkeyB64)
	if err != nil {
		return nil, fmt.Errorf("mint: decode pubkey: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mint: pubkey wrong size (got %d, want %d)", len(pub), ed25519.PublicKeySize)
	}

	existing, err := v.Store.Get(ctx, req.Name)
	previouslyExisted := false
	switch {
	case errors.Is(err, aspects.ErrNotFound):
		// First mint — INSERT below.
	case err != nil:
		return nil, fmt.Errorf("mint: lookup %q: %w", req.Name, err)
	default:
		previouslyExisted = true
		if existing.Status == aspects.StatusRetired {
			return nil, fmt.Errorf("mint: %q is retired; resurrect first", req.Name)
		}
		// Re-mint always proceeds; --force is the CLI-side suppress for
		// the operator-warning print, not a server-side gate.
	}

	var version int64
	if !previouslyExisted {
		row := aspects.Aspect{
			Name:         req.Name,
			AspectPubkey: ed25519.PublicKey(pub),
			Provider:     req.Provider,
			Model:        req.Model,
		}
		if err := v.Store.Insert(ctx, row); err != nil {
			return nil, fmt.Errorf("mint: insert: %w", err)
		}
		version = 1
	} else {
		bumped, err := v.Store.BumpKeyfileVersion(ctx, req.Name, pub)
		if err != nil {
			return nil, fmt.Errorf("mint: bump version: %w", err)
		}
		version = bumped
	}

	return &AdminMintResponse{
		Name:              req.Name,
		KeyfileVersion:    version,
		NexusID:           v.NexusID,
		ServerPubkeyB64:   base64.StdEncoding.EncodeToString(v.ServerEd25519Pubkey),
		PreviouslyExisted: previouslyExisted,
	}, nil
}

// handleAdminAspectMint is the HTTP entry point for
// POST /api/admin/aspects/mint. Body shape: AdminMintRequest. Response
// shape: AdminMintResponse. Wrapped in b.requireAdmin upstream.
func (b *Broker) handleAdminAspectMint(w http.ResponseWriter, r *http.Request) {
	if b.cfg.KeyfileValidator == nil {
		writeError(w, http.StatusServiceUnavailable, "keyfile_validator_not_configured")
		return
	}
	var req AdminMintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid_json: %v", err))
		return
	}
	resp, err := b.cfg.KeyfileValidator.MintAspect(r.Context(), req)
	if err != nil {
		// Surface the human-readable error message; keyfileValidator
		// errors are already self-descriptive ("retired; resurrect
		// first", "pubkey wrong size", etc).
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		b.log.Warn("admin mint: encode response", "err", err)
	}
}
