// Central nexus_md admin endpoint (Part 9c).
//
// Per agent-network/docs/2026-05-08-personality-decomposition-spec.md §4.1:
//
//	PUT /api/admin/nexus-md
//	  body: { "nexus_md": "..." }
//	  auth: admin-flagged session token
//	  response: { "old_version": N, "new_version": N+1 }
//
// Writes to nexus_settings.nexus_md (Part 9a). Bumps version on every
// write. Fires Config.OnNexusMDChange so follow-up broadcast or cache
// invalidation wiring can react to the new version.
//
// Distinct from `PUT /api/admin/aspect/<name>/personality` (Part 7b)
// which writes to aspect_personalities — that's the per-aspect delta;
// this is the network-wide central scope.

package broker

import (
	"encoding/json"
	"net/http"
)

// adminNexusMDRequest is the PUT body shape.
type adminNexusMDRequest struct {
	NexusMD string `json:"nexus_md"`
}

// adminNexusMDResponse carries the version transition.
type adminNexusMDResponse struct {
	OldVersion int64 `json:"old_version"`
	NewVersion int64 `json:"new_version"`
}

func (b *Broker) handleAdminNexusMDEdit(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "nexus_settings store not configured")
		return
	}

	// Same body cap as the per-aspect endpoint — operational doc
	// content shouldn't exceed a couple hundred KB.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)

	var req adminNexusMDRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body malformed", 0)
		return
	}

	// Capture prior version for the response. Get materialises the
	// row at version=0 if absent (Part 9a default), so this is safe
	// even on the very first call.
	prior, err := v.Settings.Get(r.Context())
	if err != nil {
		b.log.Error("admin nexus-md: read prior", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	newVersion, err := v.Settings.SetNexusMD(r.Context(), req.NexusMD)
	if err != nil {
		b.log.Error("admin nexus-md: write", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	b.log.Info("admin nexus-md edit",
		"old_version", prior.Version,
		"new_version", newVersion,
		"bytes", len(req.NexusMD))

	// Part 9d hook: fire OnNexusMDChange so remote-aspect broadcast or
	// cache invalidation can land here once the WS frame protocol ships.
	if b.cfg.OnNexusMDChange != nil {
		b.cfg.OnNexusMDChange(newVersion)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adminNexusMDResponse{
		OldVersion: prior.Version,
		NewVersion: newVersion,
	})
}

// adminNexusMDWorkerRequest is the PUT body shape for the headless-run
// variant (NEX-826). Separate type from adminNexusMDRequest so
// DisallowUnknownFields keeps posting to the wrong endpoint an error
// rather than a silent no-op write of "".
type adminNexusMDWorkerRequest struct {
	NexusMDWorker string `json:"nexus_md_worker"`
}

// handleAdminNexusMDWorkerEdit writes nexus_settings.nexus_md_worker —
// the central policy served to derived hands and pool workers.
//
//	PUT /api/admin/nexus-md-worker
//	  body: { "nexus_md_worker": "..." }
//	  auth: admin-flagged session token
//	  response: { "old_version": N, "new_version": N+1 }
//
// Deliberately a sibling of handleAdminNexusMDEdit rather than a mode
// flag on it: these are two different documents with different
// audiences, and a caller that muddles them should get a 400 from the
// decoder, not a prompt swap. An empty body value clears the variant,
// which returns every identity to the single-prompt shape — that is a
// supported operation (it is the rollback), so it is not rejected.
func (b *Broker) handleAdminNexusMDWorkerEdit(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "nexus_settings store not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)

	var req adminNexusMDWorkerRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body malformed", 0)
		return
	}

	prior, err := v.Settings.Get(r.Context())
	if err != nil {
		b.log.Error("admin nexus-md-worker: read prior", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	newVersion, err := v.Settings.SetNexusMDWorker(r.Context(), req.NexusMDWorker)
	if err != nil {
		b.log.Error("admin nexus-md-worker: write", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	b.log.Info("admin nexus-md-worker edit",
		"old_version", prior.Version,
		"new_version", newVersion,
		"bytes", len(req.NexusMDWorker),
		"cleared", req.NexusMDWorker == "")

	// Same Part 9d hook as the interactive variant: one settings row, one
	// "central changed" signal.
	if b.cfg.OnNexusMDChange != nil {
		b.cfg.OnNexusMDChange(newVersion)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adminNexusMDResponse{
		OldVersion: prior.Version,
		NewVersion: newVersion,
	})
}
