package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

type adminDispatchEnabledBody struct {
	Aspect  string `json:"aspect,omitempty"`
	Enabled bool   `json:"enabled"`
}

func (b *Broker) handleAdminDispatchEnabledGet(w http.ResponseWriter, r *http.Request) {
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	writeJSON(w, http.StatusOK, adminDispatchEnabledBody{
		Aspect:  aspect,
		Enabled: b.aspectDispatchEnabled(aspect),
	})
}

func (b *Broker) handleAdminDispatchEnabledSet(w http.ResponseWriter, r *http.Request) {
	aspect := r.PathValue("name")
	if aspect == "" {
		writeError(w, http.StatusBadRequest, "aspect name required in path")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req adminDispatchEnabledBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return
	}
	if err := b.setAspectDispatchEnabled(r.Context(), aspect, req.Enabled); err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "aspect not found")
			return
		}
		b.log.Error("admin dispatch-enabled set", "aspect", aspect, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, adminDispatchEnabledBody{
		Aspect:  aspect,
		Enabled: req.Enabled,
	})
}

func (b *Broker) aspectDispatchEnabled(aspect string) bool {
	store, ok := b.keyfileStore()
	if !ok {
		return true
	}
	enabled, err := store.DispatchEnabled(b.ctxOrBackground(), aspect)
	if err != nil {
		if !errors.Is(err, aspects.ErrNotFound) {
			b.log.Warn("dispatch-enabled lookup failed; defaulting enabled", "aspect", aspect, "err", err)
		}
		return true
	}
	return enabled
}

func (b *Broker) setAspectDispatchEnabled(ctx context.Context, aspect string, enabled bool) error {
	store, ok := b.keyfileStore()
	if !ok {
		return errors.New("aspects store not configured")
	}
	return store.SetDispatchEnabled(ctx, aspect, enabled)
}

func (b *Broker) ctxOrBackground() context.Context {
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}
