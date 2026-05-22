package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// refreshSender abstracts the wsclient request path so the loop can
// be unit-tested with a fake. Production wiring passes wsasp.Client.
type refreshSender interface {
	Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error)
}

// sessionRefreshLoop schedules an in-protocol JWT refresh some lead
// time before the current expiry. On success it updates sessionState;
// on repeated failure it gives up for the current window and lets
// jwtExpiryMonitor's safety-net restart kick in 1 minute before
// expiry. Each successful refresh reschedules off the *new* expiry.
//
// Jitter (±10% of lead) spreads refresh load across aspects so a
// coordinated startup doesn't hammer the broker.
func sessionRefreshLoop(
	ctx context.Context,
	state *sessionState,
	sender refreshSender,
	lead time.Duration,
	log *slog.Logger,
) {
	const maxAttempts = 3
	const retryDelay = 5 * time.Minute

	for {
		snap := state.Snapshot()
		wakeAt := snap.Expires.Add(-lead).Add(jitter(lead))
		d := time.Until(wakeAt)
		if d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}

		ok := false
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if ctx.Err() != nil {
				return
			}
			fresh, err := requestRefresh(ctx, sender)
			if err == nil {
				state.Set(fresh)
				log.Info("agentfunnel: session refreshed",
					"expires", fresh.Expires.Format(time.RFC3339),
					"attempt", attempt)
				ok = true
				break
			}
			log.Warn("agentfunnel: session refresh failed",
				"attempt", attempt, "err", err)
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryDelay):
				}
			}
		}
		if !ok {
			log.Warn("agentfunnel: session refresh giving up; jwtExpiryMonitor will restart",
				"current_expires", state.Snapshot().Expires.Format(time.RFC3339))
			// Don't tight-loop — sleep until close to expiry so the
			// safety-net monitor can do its job.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(state.Snapshot().Expires)):
			}
		}
	}
}

func requestRefresh(ctx context.Context, sender refreshSender) (sessionSnapshot, error) {
	env, err := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{
		Reason: "lead_time",
	})
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("compose: %w", err)
	}
	resp, err := sender.Request(ctx, env)
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("request: %w", err)
	}
	if resp.Kind != frames.KindSessionRefreshResult {
		// Broker rejected with an error frame (kind = "<req>.error");
		// surface its message instead of failing later on empty fields.
		var errPayload map[string]string
		_ = frames.PayloadAs(resp, &errPayload)
		msg := errPayload["error"]
		if msg == "" {
			msg = "(no error message)"
		}
		return sessionSnapshot{}, fmt.Errorf("broker rejected (kind=%s): %s", resp.Kind, msg)
	}
	var p frames.SessionRefreshResultPayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		return sessionSnapshot{}, fmt.Errorf("decode result: %w", err)
	}
	exp, err := time.Parse(time.RFC3339, p.SessionExpiresAt)
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("parse expiry %q: %w", p.SessionExpiresAt, err)
	}
	return sessionSnapshot{JWT: p.SessionJWT, Expires: exp}, nil
}

// jitter returns a random duration in [-10%, +10%] of d.
func jitter(d time.Duration) time.Duration {
	const pct = 0.1
	span := float64(d) * pct
	return time.Duration((rand.Float64()*2 - 1) * span)
}
