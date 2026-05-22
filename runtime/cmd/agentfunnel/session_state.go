package main

import (
	"sync/atomic"
	"time"
)

// sessionState holds the current session JWT + expiry. Updated by
// the refresh loop on successful refresh, by the boot path on
// startup, and by tokenProvider when it falls back to keyfile
// re-validate. Read by tokenProvider on each WS dial and by
// jwtExpiryMonitor for its safety-net wakeup.
//
// Backed by atomic.Pointer so each write is individually atomic.
// The three writers race only at JWT-near-expiry boundaries where
// any of their writes carries an equally-valid new JWT, so
// last-write-wins is fine.
type sessionState struct {
	v atomic.Pointer[sessionSnapshot]
}

type sessionSnapshot struct {
	JWT     string
	Expires time.Time
}

func newSessionState(initial sessionSnapshot) *sessionState {
	s := &sessionState{}
	s.v.Store(&initial)
	return s
}

func (s *sessionState) Snapshot() sessionSnapshot {
	return *s.v.Load()
}

func (s *sessionState) Set(snap sessionSnapshot) {
	s.v.Store(&snap)
}
