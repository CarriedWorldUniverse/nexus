// Surface switching — admin REST endpoint + WS frame handler.
//
// Two paths to flip an aspect's primary_surface:
//
//   1. Admin REST: PUT /api/admin/aspects/{name}/switch-surface
//      Operator-initiated. Admin-gated.
//
//   2. WS frame: aspect sends switch.surface → broker validates,
//      updates DB, sends switch.surface.result, closes connection.
//      Aspect-initiated self-switch.
//
// Both paths update the aspects DB metadata column (json_set) and
// close the aspect's WS connection. The aspect exits, the supervisor
// restarts it, and autospawn picks the new binary based on the
// updated DB row.

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// SwitchSurfaceRequest is the PUT body for the admin endpoint.
type SwitchSurfaceRequest struct {
	PrimarySurface string `json:"primary_surface"`
}

// SwitchSurfaceResponse is returned from the switch-surface admin endpoint.
type SwitchSurfaceResponse struct {
	Aspect          string `json:"aspect"`
	PrimarySurface  string `json:"primary_surface"`
	PreviousSurface string `json:"previous_surface,omitempty"`
	RestartNeeded   bool   `json:"restart_needed"`
}

// handleAdminSwitchSurface implements PUT /api/admin/aspects/{name}/switch-surface.
func (b *Broker) handleAdminSwitchSurface(w http.ResponseWriter, r *http.Request) {
	aspectName := r.PathValue("name")
	if aspectName == "" {
		writeError(w, http.StatusBadRequest, "aspect name required")
		return
	}

	var req SwitchSurfaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid_json: %v", err))
		return
	}

	surface := req.PrimarySurface
	switch surface {
	case "funnel", "agora":
	default:
		writeError(w, http.StatusBadRequest, "primary_surface must be 'funnel' or 'agora'")
		return
	}

	resp, err := b.switchAspectSurface(r.Context(), aspectName, surface)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			writeError(w, http.StatusNotFound, "aspect not found")
		} else {
			b.log.Error("switch-surface: db write failed", "aspect", aspectName, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	b.log.Info("switch-surface: admin flipped surface",
		"aspect", aspectName, "surface", surface, "previous", resp.PreviousSurface)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleSwitchSurfaceFrame handles an incoming switch.surface WS frame
// from an aspect. The aspect identity is taken from the connection's
// authenticated registration, not from the payload.
func (c *wsConn) handleSwitchSurfaceFrame(env frames.Envelope) {
	if c.registeredAs == "" {
		c.log.Warn("switch.surface from unregistered conn — dropped")
		return
	}

	var p frames.SwitchSurfacePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("switch.surface: payload decode failed", "err", err, "aspect", c.registeredAs)
		return
	}

	surface := p.PrimarySurface
	switch surface {
	case "funnel", "agora":
	default:
		c.log.Warn("switch.surface: invalid surface", "surface", surface, "aspect", c.registeredAs)
		return
	}

	resp, err := c.broker.switchAspectSurface(context.Background(), c.registeredAs, surface)
	if err != nil {
		c.log.Error("switch.surface: db write failed",
			"aspect", c.registeredAs, "surface", surface, "err", err)
		return
	}

	c.log.Info("switch.surface: aspect self-flipped surface",
		"aspect", c.registeredAs, "surface", surface, "previous", resp.PreviousSurface)

	// Send ack back.
	resultEnv, _ := frames.New(frames.KindSwitchSurfaceResult, frames.SwitchSurfaceResultPayload{
		Aspect:          resp.Aspect,
		PrimarySurface:  resp.PrimarySurface,
		PreviousSurface: resp.PreviousSurface,
	})
	c.send(resultEnv)

	// Close the WS so the aspect exits and the supervisor restarts it
	// with the new binary.
	c.log.Info("switch.surface: closing connection for restart",
		"aspect", c.registeredAs)
	_ = c.conn.Close(websocket.StatusNormalClosure, "surface switch")
}

// switchAspectSurface performs the DB update and returns the response.
// Shared by both the admin REST and WS frame paths.
func (b *Broker) switchAspectSurface(ctx context.Context, aspectName, surface string) (*SwitchSurfaceResponse, error) {
	// Read current surface from the roster if the aspect is connected.
	previousSurface := ""
	if entry, ok := b.roster.Get(aspectName); ok {
		previousSurface = string(entry.PrimarySurface)
	}

	// Persist the new surface preference in the aspects DB.
	store, ok := b.keyfileStore()
	if !ok {
		return nil, errors.New("aspects store not configured")
	}
	if err := store.SetPrimarySurface(ctx, aspectName, surface); err != nil {
		return nil, err
	}

	// Close the aspect's WS connection if it's live. The exit triggers
	// supervisor restart; autospawn reads the new surface from the DB.
	restartNeeded := false
	if entry, ok := b.roster.Get(aspectName); ok && entry.Status == "live" {
		restartNeeded = true
		b.closeAspectConn(aspectName)
	}

	return &SwitchSurfaceResponse{
		Aspect:          aspectName,
		PrimarySurface:  surface,
		PreviousSurface: previousSurface,
		RestartNeeded:   restartNeeded,
	}, nil
}

// keyfileStore returns the aspects Store from the KeyfileValidator, if
// configured. Returns nil, false when the validator or its store is nil.
func (b *Broker) keyfileStore() (aspects.Store, bool) {
	if b.cfg.KeyfileValidator == nil || b.cfg.KeyfileValidator.Store == nil {
		return nil, false
	}
	return b.cfg.KeyfileValidator.Store, true
}

// closeAspectConn finds and closes the WS connection for the named aspect
// via the dispatcher. No-op when the aspect isn't connected.
func (b *Broker) closeAspectConn(aspectName string) {
	c := b.dispatcher.connFor(aspectName)
	if c != nil {
		_ = c.conn.Close(websocket.StatusNormalClosure, "surface switch")
	}
}
