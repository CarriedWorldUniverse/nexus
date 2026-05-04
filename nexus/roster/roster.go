// Package roster holds the live set of registered aspects.
//
// The roster is in-memory for v1. Persistence is deliberately not here —
// aspects re-register on every boot, so there is no crash-recovery story
// other than "Nexus restarts, aspects re-announce". See spec §2.5.
package roster

import (
	"errors"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

var (
	ErrAlreadyRegistered = errors.New("aspect already registered with different session")
	ErrNotRegistered     = errors.New("aspect not registered")
	ErrSessionMismatch   = errors.New("session id does not match current registration")
	ErrPortConflict      = errors.New("port already claimed by another live aspect")
)

// Roster is the thread-safe live set of aspects.
type Roster struct {
	mu      sync.RWMutex
	aspects map[string]*schemas.AspectState
}

func New() *Roster {
	return &Roster{aspects: make(map[string]*schemas.AspectState)}
}

// Register adds (or replaces) an aspect entry. Replaces only if the caller's
// session id matches — otherwise returns ErrAlreadyRegistered so a stale
// duplicate can't evict a live session. Returns the previous session ID
// (if any) so the caller can log displacement for observability.
func (r *Roster) Register(req *schemas.RegisterRequest) (state *schemas.AspectState, displacedSession string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.aspects[req.Name]; ok {
		if existing.SessionID != req.SessionID && existing.Status == "live" {
			return nil, "", ErrAlreadyRegistered
		}
		if existing.SessionID != req.SessionID {
			displacedSession = existing.SessionID
		}
	}

	// Port conflict check — only against *other* aspects, and only
	// for real port numbers. Port 0 is the WS-era convention for
	// "no inbound listener," so multiple aspects registering with
	// port=0 must coexist. Treating 0 as a real port would false-
	// positive as soon as the second WS-era aspect registered.
	if req.Port != 0 {
		for name, a := range r.aspects {
			if name != req.Name && a.Port == req.Port && a.Status == "live" {
				return nil, "", ErrPortConflict
			}
		}
	}

	state = &schemas.AspectState{
		Name:          req.Name,
		ContextMode:   req.ContextMode,
		Provider:      req.Provider,
		Port:          req.Port,
		PID:           req.PID,
		StartedAt:     req.StartedAt,
		Model:         req.Model,
		Capabilities:  req.Capabilities,
		Home:          req.Home,
		SessionID:     req.SessionID,
		Metadata:      req.Metadata,
		LastHeartbeat: time.Now().UTC(),
		Status:        "live",
	}
	r.aspects[req.Name] = state
	return state, displacedSession, nil
}

// Heartbeat updates last-seen for an aspect. Returns ErrNotRegistered if the
// aspect isn't in the roster; ErrSessionMismatch if a different session holds
// the slot (the caller should re-register rather than heartbeat).
func (r *Roster) Heartbeat(name, sessionID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.aspects[name]
	if !ok {
		return ErrNotRegistered
	}
	if a.SessionID != sessionID {
		return ErrSessionMismatch
	}
	a.LastHeartbeat = at
	if a.Status != "live" {
		a.Status = "live"
	}
	return nil
}

// Deregister removes an aspect entry, but only if the caller owns the
// current session. Idempotent — removing an absent aspect is a no-op.
func (r *Roster) Deregister(name, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.aspects[name]
	if !ok {
		return nil
	}
	if a.SessionID != sessionID {
		return ErrSessionMismatch
	}
	delete(r.aspects, name)
	return nil
}

// List returns a snapshot of all aspects. Safe to call concurrently; the
// returned slice is a copy so callers can't mutate the roster via it.
func (r *Roster) List() []schemas.AspectState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]schemas.AspectState, 0, len(r.aspects))
	for _, a := range r.aspects {
		out = append(out, *a)
	}
	return out
}

// AspectNames returns a snapshot of registered aspect names. Used by
// the broker's RecipientPolicy to expand @all into the live roster.
// Returns names in arbitrary order; callers that need stability sort
// the result.
func (r *Roster) AspectNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aspects))
	for name := range r.aspects {
		out = append(out, name)
	}
	return out
}

// Get returns a single aspect's state, or false if absent.
func (r *Roster) Get(name string) (schemas.AspectState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aspects[name]
	if !ok {
		return schemas.AspectState{}, false
	}
	return *a, true
}

// ReapStale transitions any aspect whose last heartbeat is older than
// staleAfter to "stale", and older than 2*staleAfter to "down". Returns the
// names that changed state so the caller can emit observability events.
func (r *Roster) ReapStale(now time.Time, staleAfter time.Duration) (stale, down []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, a := range r.aspects {
		age := now.Sub(a.LastHeartbeat)
		switch {
		case age > 2*staleAfter && a.Status != "down":
			a.Status = "down"
			down = append(down, name)
		case age > staleAfter && a.Status == "live":
			a.Status = "stale"
			stale = append(stale, name)
		}
	}
	return
}
