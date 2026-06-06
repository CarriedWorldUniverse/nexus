// Aspect self-edit endpoint (Part 9c).
//
// Per agent-network/docs/2026-05-08-personality-decomposition-spec.md §4.2:
//
//	PUT /api/aspect/personality
//	  body: { "nexus_md": "...", "soul_md": "...", "primer_md": "..." }
//	  auth: aspect's own session JWT (NOT admin)
//	  response: { "aspect": "...", "old_version": N, "new_version": N+1 }
//
// Aspect name comes from the JWT's `sub` claim — never from a request
// field. An aspect cannot edit another aspect's personality. Admin
// bearers cannot use this endpoint either; they have
// /api/admin/aspect/<name>/personality (Part 7b).
//
// Writes via aspects.EditPersonality (shared with admin path); fires
// OnPersonalityChange the same way.

package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

// aspectSelfEditRequest is the PUT body shape. Same three columns as
// the admin endpoint (Part 7b's adminPersonalityRequest); no
// nexus_settings.nexus_md field — agents have no path to edit central
// content.
type aspectSelfEditRequest struct {
	NexusMD  string `json:"nexus_md"`
	SoulMD   string `json:"soul_md"`
	PrimerMD string `json:"primer_md"`
}

// aspectSelfEditResponse mirrors aspects.PersonalityChange.
type aspectSelfEditResponse struct {
	Aspect     string `json:"aspect"`
	OldVersion int64  `json:"old_version"`
	NewVersion int64  `json:"new_version"`
}

func (b *Broker) handleAspectSelfEdit(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "personality store not configured")
		return
	}

	// Verify the aspect's own session JWT. The sub claim is the only
	// place the target aspect_name can come from — never from a body
	// field, header, or query param. This is the load-bearing
	// authorisation invariant: a stolen-and-edited request body
	// cannot edit a different aspect because the JWT signature would
	// fail re-verification at this point.
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	claims, err := jwt.Verify(v.SessionSigningSecret, token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	if claims.Sub == "" {
		writeError(w, http.StatusUnauthorized, "session token missing sub claim")
		return
	}
	aspectName := claims.Sub

	// Cap body size — three markdown sections, 256 KiB matches the
	// admin endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)

	var req aspectSelfEditRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body malformed", 0)
		return
	}

	change, err := aspects.EditPersonality(r.Context(), v.Store, aspectName,
		req.NexusMD, req.SoulMD, req.PrimerMD)
	if err != nil {
		switch {
		case errors.Is(err, aspects.ErrNotFound):
			// JWT was valid but the aspect row doesn't exist anymore.
			// Most likely the aspect was retired between mint and
			// edit. Surface as 404 — operator/agent should re-mint.
			writeJSONError(w, http.StatusNotFound, "aspect no longer exists", 0)
		default:
			b.log.Error("aspect self-edit", "aspect", aspectName, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	b.log.Info("aspect self-edit",
		"aspect", change.AspectName,
		"old_version", change.OldVersion,
		"new_version", change.NewVersion)

	// Same listener as admin edits, keeping future broadcast wiring
	// uniform regardless of edit origin.
	if b.cfg.OnPersonalityChange != nil {
		b.cfg.OnPersonalityChange(change.AspectName, change.NewVersion)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(aspectSelfEditResponse{
		Aspect:     change.AspectName,
		OldVersion: change.OldVersion,
		NewVersion: change.NewVersion,
	})
}
