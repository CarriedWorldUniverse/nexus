package broker

// Admin REST endpoints — §6.5 P7 implementation of #79 lock
// (docs/2026-05-01-frame-stop-decisions.md).
//
// All endpoints under /api/admin/* are admin-flag-gated: only tokens
// reconciled with admin=true (the Frame's identity, or the legacy
// fallback) may invoke them. Aspect tokens are rejected with
// admin_required.
//
// Long-running ops follow the 202 + operation-id + status-poll pattern:
// kick off → return 202 with op_id → callers GET /api/admin/op/{id} for
// progress. v1 ops are short enough that this matters mainly for shape;
// when real long-running operations land (cross-thread rewind, large
// session compact), the seam is ready.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// AdminCallbacks injects the broker's admin-action implementations.
// Each callback is optional: a nil callback returns 501 not_implemented
// for the corresponding endpoint, so the REST shape stays locked while
// individual operations land in later parts.
type AdminCallbacks struct {
	// Shutdown asks the Nexus to begin graceful shutdown. Returns when
	// the request is accepted (server shutdown happens asynchronously).
	Shutdown func(ctx context.Context) error

	// Compact triggers a session-storage compaction sweep.
	Compact func(ctx context.Context) error

	// Rewind walks back N turns in a thread/session. Future-spec.
	Rewind func(ctx context.Context, threadID string, turns int) error

	// DispatchStatus returns current handqueue occupancy / queue depth.
	DispatchStatus func(ctx context.Context) (DispatchStatusReport, error)
}

// DispatchStatusReport is the shape returned from /api/admin/dispatch-status.
// Mirrors what the handqueue exposes; broker fans it out to JSON.
type DispatchStatusReport struct {
	ActiveWorkers int      `json:"active_workers"`
	SoftCap       int      `json:"soft_cap"`
	HardCeiling   int      `json:"hard_ceiling"`
	QueueDepth    int      `json:"queue_depth"`
	BusyAspects   []string `json:"busy_aspects"`
}

// adminOp tracks an in-flight long-running admin operation. The
// op-store is in-memory; restart loses op history. Acceptable for
// admin tooling (operator can re-poll if Nexus restarts mid-op).
type adminOp struct {
	ID        string
	Action    string // "shutdown" | "compact" | "rewind"
	Status    string // "running" | "ok" | "error"
	StartedAt time.Time
	EndedAt   time.Time
	Err       string
}

// adminOpStore guards in-flight + completed op records.
type adminOpStore struct {
	mu  sync.RWMutex
	ops map[string]*adminOp
}

func newAdminOpStore() *adminOpStore {
	return &adminOpStore{ops: make(map[string]*adminOp)}
}

// start mints a new running op and returns a value copy so callers
// can read the immutable initial fields (ID, Action, started timestamp,
// Status="running") without holding the store lock or risking a race
// with the finish() write.
func (s *adminOpStore) start(action string) adminOp {
	op := &adminOp{
		ID:        newOpID(),
		Action:    action,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.ops[op.ID] = op
	s.mu.Unlock()
	return *op
}

// finish marks the op complete (or error). Always takes the write lock.
func (s *adminOpStore) finish(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return
	}
	op.EndedAt = time.Now().UTC()
	if err != nil {
		op.Status = "error"
		op.Err = err.Error()
	} else {
		op.Status = "ok"
	}
}

// get returns a snapshot of the op's current state. Returns by value so
// the caller can safely read fields without locking — the goroutine
// running the op may mutate the canonical record concurrently.
func (s *adminOpStore) get(id string) (adminOp, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	op, ok := s.ops[id]
	if !ok {
		return adminOp{}, false
	}
	return *op, true
}

// newOpID mints a short hex id for an admin op. 8 bytes is enough for
// in-memory uniqueness across a single Nexus lifetime.
func newOpID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Crypto-rand failure is unrecoverable; falling back to time
		// would defeat the uniqueness guarantee. Panic is OK here —
		// admin endpoints can't operate without unique ids.
		panic(fmt.Sprintf("admin: opid rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// requireAdmin wraps a handler with admin-flag enforcement on top of
// the standard auth middleware. Rejects with 403 admin_required if the
// resolved TokenInfo doesn't carry Admin=true.
func (b *Broker) requireAdmin(next http.Handler) http.Handler {
	return b.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := AuthUserFromContext(r.Context())
		if !ok || !info.Admin {
			writeError(w, http.StatusForbidden, "admin_required")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// registerAdmin wires /api/admin/* routes onto mux. Called from
// ListenAndServe when the broker is configured with AdminCallbacks
// (the embedded Frame supplies them per P5).
func (b *Broker) registerAdmin(mux *http.ServeMux) {
	if b.cfg.Admin == nil {
		// No admin callbacks configured; skip registration. /api/admin
		// returns 404 in this state, which is correct — admin surface
		// requires a Frame to back it.
		return
	}
	if b.adminOps == nil {
		b.adminOps = newAdminOpStore()
	}
	mux.Handle("POST /api/admin/shutdown", b.requireAdmin(http.HandlerFunc(b.handleAdminShutdown)))
	mux.Handle("POST /api/admin/compact", b.requireAdmin(http.HandlerFunc(b.handleAdminCompact)))
	mux.Handle("POST /api/admin/rewind", b.requireAdmin(http.HandlerFunc(b.handleAdminRewind)))
	mux.Handle("GET /api/admin/dispatch-status", b.requireAdmin(http.HandlerFunc(b.handleAdminDispatchStatus)))
	mux.Handle("GET /api/admin/roster", b.requireAdmin(http.HandlerFunc(b.handleAdminRoster)))
	mux.Handle("GET /api/admin/op/{id}", b.requireAdmin(http.HandlerFunc(b.handleAdminOp)))
}

// handleAdminShutdown kicks off a graceful shutdown. Long-running by
// definition (the Nexus is shutting down), so use the 202+op-id pattern.
func (b *Broker) handleAdminShutdown(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Admin.Shutdown == nil {
		writeError(w, http.StatusNotImplemented, "shutdown_not_implemented")
		return
	}
	op := b.adminOps.start("shutdown")
	go func() {
		// Use background context, not request context — the request
		// completes quickly, but the shutdown itself outlives it.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := b.cfg.Admin.Shutdown(ctx)
		b.adminOps.finish(op.ID, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"op_id":  op.ID,
		"action": op.Action,
		"status": op.Status,
	})
}

// handleAdminCompact triggers a session-storage compaction sweep.
func (b *Broker) handleAdminCompact(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Admin.Compact == nil {
		writeError(w, http.StatusNotImplemented, "compact_not_implemented")
		return
	}
	op := b.adminOps.start("compact")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := b.cfg.Admin.Compact(ctx)
		b.adminOps.finish(op.ID, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"op_id":  op.ID,
		"action": op.Action,
		"status": op.Status,
	})
}

// handleAdminRewind walks back N turns in a thread. Body:
// {"thread_id": "...", "turns": 1}
func (b *Broker) handleAdminRewind(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Admin.Rewind == nil {
		writeError(w, http.StatusNotImplemented, "rewind_not_implemented")
		return
	}

	var body struct {
		ThreadID string `json:"thread_id"`
		Turns    int    `json:"turns"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body: "+err.Error())
		return
	}
	if body.ThreadID == "" {
		writeError(w, http.StatusBadRequest, "thread_id required")
		return
	}
	if body.Turns < 1 {
		writeError(w, http.StatusBadRequest, "turns must be >= 1")
		return
	}
	const maxRewindTurns = 1000
	if body.Turns > maxRewindTurns {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("turns must be <= %d", maxRewindTurns))
		return
	}

	op := b.adminOps.start("rewind")
	threadID, turns := body.ThreadID, body.Turns
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		err := b.cfg.Admin.Rewind(ctx, threadID, turns)
		b.adminOps.finish(op.ID, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"op_id":  op.ID,
		"action": op.Action,
		"status": op.Status,
	})
}

// handleAdminDispatchStatus returns current handqueue state. Synchronous
// (not 202) because the answer is a snapshot, not a long-running op.
func (b *Broker) handleAdminDispatchStatus(w http.ResponseWriter, r *http.Request) {
	if b.cfg.Admin.DispatchStatus == nil {
		writeError(w, http.StatusNotImplemented, "dispatch_status_not_implemented")
		return
	}
	report, err := b.cfg.Admin.DispatchStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleAdminRoster is an extended /api/aspects with admin-only
// metadata. v1 just returns the standard roster list — extension fields
// land when there's a concrete need.
func (b *Broker) handleAdminRoster(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"aspects": b.roster.List(),
	})
}

// handleAdminOp returns an op's current status. 404 for unknown ids.
func (b *Broker) handleAdminOp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "op_id required")
		return
	}
	op, ok := b.adminOps.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "op_not_found")
		return
	}
	resp := map[string]any{
		"op_id":      op.ID,
		"action":     op.Action,
		"status":     op.Status,
		"started_at": op.StartedAt.Format(time.RFC3339Nano),
	}
	if !op.EndedAt.IsZero() {
		resp["ended_at"] = op.EndedAt.Format(time.RFC3339Nano)
	}
	if op.Err != "" {
		resp["error"] = op.Err
	}
	writeJSON(w, http.StatusOK, resp)
}
