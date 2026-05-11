package observability

import "sync"

// Buffer is a per-aspect ring of recent Frames. The broker holds
// one process-wide Buffer; subscribers replay the tail on connect
// (Tail) before joining the live stream. Frames are stored in
// append order; ordering across aspects is not guaranteed (each
// aspect has its own monotonic sequence).
type Buffer struct {
	cap   int
	mu    sync.Mutex
	rings map[string]*aspectRing
}

// aspectRing is a fixed-capacity ring with sequence-keyed lookup.
// Implemented as a slice + head index rather than a linked list
// for cheap iteration during Tail.
type aspectRing struct {
	cap   int
	items []Frame // length grows up to cap, then wraps
	start int     // index of the oldest frame when items is at cap
}

// NewBuffer constructs a Buffer with the given per-aspect capacity.
// cap must be > 0; the constructor panics on a non-positive value
// because there's no sensible recovery — the caller's invariant is
// broken.
func NewBuffer(cap int) *Buffer {
	if cap <= 0 {
		panic("observability: Buffer cap must be > 0")
	}
	return &Buffer{cap: cap, rings: make(map[string]*aspectRing)}
}

// Append adds a Frame to the aspect's ring, evicting the oldest if
// the ring is full. Safe for concurrent use.
func (b *Buffer) Append(f Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[f.Aspect]
	if !ok {
		r = &aspectRing{cap: b.cap, items: make([]Frame, 0, b.cap)}
		b.rings[f.Aspect] = r
	}
	r.append(f)
}

// Tail returns frames for aspect with Sequence > sinceSeq, oldest
// first. sinceSeq=0 yields the full retained tail. The returned
// slice is a fresh copy — callers may retain or mutate it without
// affecting the ring.
func (b *Buffer) Tail(aspect string, sinceSeq int64) []Frame {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[aspect]
	if !ok {
		return nil
	}
	return r.tail(sinceSeq)
}

// Drop releases an aspect's ring. The broker calls this when the
// last subscriber for an aspect disconnects so a dormant aspect
// doesn't hold memory indefinitely.
func (b *Buffer) Drop(aspect string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.rings, aspect)
}

func (r *aspectRing) append(f Frame) {
	if len(r.items) < r.cap {
		r.items = append(r.items, f)
		return
	}
	r.items[r.start] = f
	r.start = (r.start + 1) % r.cap
}

func (r *aspectRing) tail(sinceSeq int64) []Frame {
	out := make([]Frame, 0, len(r.items))
	// Walk in logical order: start, start+1, ..., wrapping.
	n := len(r.items)
	for i := 0; i < n; i++ {
		idx := i
		if n == r.cap {
			idx = (r.start + i) % r.cap
		}
		if r.items[idx].Sequence > sinceSeq {
			out = append(out, r.items[idx])
		}
	}
	return out
}
