// Lock 6 replay: given an aspect and a since-cursor, walk the chat
// history forward and yield messages that should have been delivered
// to that aspect via the recipient policy.
//
// Per operator #9177: "give me all message for forge after id=678,
// and send each as a separate comm.send." The replay is server-side
// only — aspects don't see any difference between live and replayed
// frames except for the Replay flag on ChatDeliverPayload (set at
// the broker's emit layer, not here).
//
// This module is the query engine. F2.4+ wires it into the
// register-frame handler so reconnect with since_msg_id triggers a
// replay before resuming live delivery.

package broker

import (
	"context"
	"fmt"

	"github.com/nexus-cw/nexus/nexus/chat"
)

// ReplayPageSize is the chat.Store ListSince page size for replay
// scans. Bounded so the broker doesn't slurp the entire chat
// history in one query for an aspect that's been offline for weeks.
const ReplayPageSize = 500

// Replayer walks chat history forward from a cursor, returning
// messages addressed to the named aspect per the supplied
// RecipientPolicy. Used by the WS register-frame handler when an
// aspect reconnects with since_msg_id > 0.
type Replayer struct {
	Store    chat.Store
	Policy   RecipientPolicy
	PageSize int // 0 = ReplayPageSize default
}

// NewReplayer constructs a Replayer with sensible defaults.
func NewReplayer(store chat.Store, policy RecipientPolicy) *Replayer {
	return &Replayer{Store: store, Policy: policy}
}

// AddressedSince returns chat messages with id > sinceID that the
// recipient policy says should have been delivered to `aspect`.
// Oldest first. Caller (the WS handler) emits each as its own
// chat.deliver frame with Replay=true.
//
// Implementation: pages through chat.Store.ListSince in chunks of
// ReplayPageSize, applying the policy per-message. A bounded
// max-replay cap (32k messages) prevents pathological cases where a
// long-offline aspect would slurp the entire history; if the cap is
// hit the caller logs a warning but does not error — replay is
// best-effort, the aspect may miss the oldest messages but resumes
// from the most recent it can ingest, matching Lock 6's
// "graceful degradation" framing.
func (r *Replayer) AddressedSince(ctx context.Context, aspect string, sinceID int64) ([]chat.Message, error) {
	if r.Store == nil {
		return nil, fmt.Errorf("broker.Replayer: store nil")
	}
	if aspect == "" {
		return nil, fmt.Errorf("broker.Replayer: aspect required")
	}

	pageSize := r.PageSize
	if pageSize <= 0 {
		pageSize = ReplayPageSize
	}
	const maxTotal = 32_000

	cursor := sinceID
	out := make([]chat.Message, 0, 32)
	for {
		page, err := r.Store.ListSince(ctx, cursor, pageSize)
		if err != nil {
			return nil, fmt.Errorf("broker.Replayer: list: %w", err)
		}
		if len(page) == 0 {
			break
		}

		for _, msg := range page {
			cursor = msg.ID
			if shouldDeliver(r.Policy, aspect, msg) {
				out = append(out, msg)
				if len(out) >= maxTotal {
					return out, nil
				}
			}
		}

		// Short page = no more rows to read.
		if len(page) < pageSize {
			break
		}
	}
	return out, nil
}

// shouldDeliver applies the RecipientPolicy to a single message and
// returns true if `aspect` is in the recipient set. Sender is
// excluded by Compute already, so this method needs no additional
// filtering — but we re-check defensively because reading our own
// echo back during replay would surface stale text the model
// already produced.
func shouldDeliver(p RecipientPolicy, aspect string, msg chat.Message) bool {
	if msg.From == aspect {
		return false
	}
	for _, r := range p.Compute(msg.From, msg.Content, msg.ReplyTo) {
		if r == aspect {
			return true
		}
	}
	return false
}
