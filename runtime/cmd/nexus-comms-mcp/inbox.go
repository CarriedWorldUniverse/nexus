// Inbox buffer + chat.deliver capture for nexus-comms-mcp.
//
// Why a buffer: MCP's stdio model is request/response. Tool clients
// call read_chat when they want chat; we can't push to them. So we
// hold incoming chat.deliver frames in a bounded ring until they're
// drained. New frames overwrite oldest when full — chat is best-effort
// and the broker has authoritative state; if the buffer overflows the
// caller can re-fetch via since_msg_id on the next register cycle.
//
// Concurrency: WS read goroutine writes; MCP tool goroutines read.
// One mutex guards the slice. Operations are short (append / copy a
// slice header), so contention is negligible at chat-message rates.

package main

import (
	"sync"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// inboxBuffer is a bounded FIFO of chat.deliver payloads keyed by
// arrival order. Capacity is fixed at construction; when full, the
// oldest entry drops to make room.
type inboxBuffer struct {
	mu       sync.Mutex
	capacity int
	items    []frames.ChatDeliverPayload
	// highestID tracks the largest msg_id ever observed so register-
	// frames on reconnect can request since_msg_id replay. Persists
	// across drains.
	highestID int64
	// dropped counts entries evicted because the buffer was full. Pure
	// diagnostics — surfaced in read_chat output so callers can detect
	// they fell behind.
	dropped int
}

func newInboxBuffer(capacity int) *inboxBuffer {
	if capacity <= 0 {
		capacity = 500
	}
	return &inboxBuffer{
		capacity: capacity,
		items:    make([]frames.ChatDeliverPayload, 0, capacity),
	}
}

// add appends one delivered chat message. If capacity is reached, the
// oldest entry is dropped (FIFO eviction). Updates highestID.
func (b *inboxBuffer) add(p frames.ChatDeliverPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if int64(p.ID) > b.highestID {
		b.highestID = int64(p.ID)
	}

	if len(b.items) >= b.capacity {
		// Drop the oldest: shift left by one (cheap at small N; if we
		// ever push the cap above a few thousand, swap for a real ring).
		copy(b.items, b.items[1:])
		b.items = b.items[:len(b.items)-1]
		b.dropped++
	}
	b.items = append(b.items, p)
}

// snapshot returns a copy of all items currently buffered. Does NOT
// drain; the caller decides whether to consume.
//
// Why a copy: the slice can be modified concurrently by add(). Returning
// a copy means the tool handler can iterate without holding the mutex
// (which would block the WS read goroutine for the JSON-serialization
// duration).
func (b *inboxBuffer) snapshot() (items []frames.ChatDeliverPayload, dropped int, highest int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]frames.ChatDeliverPayload, len(b.items))
	copy(out, b.items)
	return out, b.dropped, b.highestID
}

// drainAfter returns all items with ID > sinceID and removes them from
// the buffer. Used by read_chat with since_id semantics — caller passes
// the highest id it has seen, gets only newer items, and the buffer
// shrinks accordingly. Items with ID <= sinceID are retained: a caller
// might want to read history backward via a different cursor.
//
// In practice for v1, callers will pass sinceID=0 to get everything,
// which fully drains.
func (b *inboxBuffer) drainAfter(sinceID int64) (items []frames.ChatDeliverPayload, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]frames.ChatDeliverPayload, 0, len(b.items))
	keep := make([]frames.ChatDeliverPayload, 0, len(b.items))
	for _, it := range b.items {
		if int64(it.ID) > sinceID {
			out = append(out, it)
		} else {
			keep = append(keep, it)
		}
	}
	b.items = keep

	d := b.dropped
	b.dropped = 0
	return out, d
}

// seedHighest pre-seeds the highestID watermark before any frames have
// arrived. Used by the --since-msg-id flag (and future persistence) so
// the first register frame after connect can request Lock-6 replay.
func (b *inboxBuffer) seedHighest(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id > b.highestID {
		b.highestID = id
	}
}

// highest reports the max msg_id seen by the buffer, regardless of
// current contents. Used by the register-on-reconnect path to request
// replay since the last delivery — even if the buffer was drained.
func (b *inboxBuffer) highest() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.highestID
}
