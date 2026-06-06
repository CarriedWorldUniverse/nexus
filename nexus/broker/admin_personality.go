// Personality edit admin endpoint (Part 7b).
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §8.2:
//
//	PUT /api/admin/aspect/<name>/personality
//	  body: { "nexus_md": "...", "soul_md": "...", "primer_md": "..." }
//	  auth: admin-flagged session token (existing requireAdmin gate)
//	  response: { "aspect": "...", "old_version": N, "new_version": N+1 }
//
// Uses aspects.EditPersonality so the CLI (Part 7a) and REST share the
// same write path — version bump, FK guard, atomic upsert all live in
// the storage layer.
//
// Part 7c will add a personality.refresh broadcast hook here once the
// WS-frame protocol lands.

package broker

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// adminPersonalityRequest is the PUT body shape.
type adminPersonalityRequest struct {
	NexusMD  string `json:"nexus_md"`
	SoulMD   string `json:"soul_md"`
	PrimerMD string `json:"primer_md"`
}

// adminPersonalityResponse mirrors aspects.PersonalityChange on the wire.
type adminPersonalityResponse struct {
	Aspect     string `json:"aspect"`
	OldVersion int64  `json:"old_version"`
	NewVersion int64  `json:"new_version"`
}

func (b *Broker) handleAdminPersonalityEdit(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "personality store not configured")
		return
	}

	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "aspect name required in path", 0)
		return
	}

	// Cap body size — three markdown sections shouldn't exceed a few
	// hundred KB even with elaborate content. 256 KiB is generous.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)

	var req adminPersonalityRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body malformed", 0)
		return
	}

	change, err := aspects.EditPersonality(r.Context(), v.Store, name, req.NexusMD, req.SoulMD, req.PrimerMD)
	if err != nil {
		switch {
		case errors.Is(err, aspects.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "unknown aspect", 0)
		default:
			b.log.Error("admin personality edit", "aspect", name, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	b.log.Info("admin personality edit",
		"aspect", change.AspectName,
		"old_version", change.OldVersion,
		"new_version", change.NewVersion)

	// Personality-change hook. Remote agentfunnels pick up at next JWT
	// re-validation (1h TTL) today; a future broadcast
	// (`personality.refresh` WS frame) will land here too.
	if b.cfg.OnPersonalityChange != nil {
		b.cfg.OnPersonalityChange(change.AspectName, change.NewVersion)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adminPersonalityResponse{
		Aspect:     change.AspectName,
		OldVersion: change.OldVersion,
		NewVersion: change.NewVersion,
	})
}
