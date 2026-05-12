package observability

import "sync"

// Hub is the broker-side aggregation point for observability Frames.
// It owns one Grouper per aspect (lazy-instantiated on first use) and
// one shared Buffer for replay-on-subscribe. Each Grouper's emit
// callback appends to the Buffer and then invokes the Hub's onFrame
// broadcast callback so subscribed operators see the frame live.
//
// Concurrency: GrouperFor is the workhorse. RLock on the fast path
// (hit); Lock + double-check on the create path (miss). Tail
// delegates to Buffer which has its own mutex. The onFrame callback
// is invoked while NOT holding the Hub mutex — the broker fan-out
// is allowed to acquire its own locks.
type Hub struct {
	mu        sync.RWMutex
	groupers  map[string]*Grouper
	buffer    *Buffer
	onFrame   func(aspect string, f Frame)
	bufferCap int
}

// NewHub constructs a Hub with the given per-aspect Buffer capacity.
// onFrame is invoked synchronously for every emitted Frame; pass a
// no-op closure if the caller hasn't wired fan-out yet.
func NewHub(bufferCap int, onFrame func(aspect string, f Frame)) *Hub {
	if bufferCap <= 0 {
		bufferCap = 500
	}
	return &Hub{
		groupers:  make(map[string]*Grouper),
		buffer:    NewBuffer(bufferCap),
		onFrame:   onFrame,
		bufferCap: bufferCap,
	}
}

// GrouperFor returns the Grouper for aspect, creating one on first
// touch. Concurrent calls for the same aspect return the same
// instance — the double-check pattern under Lock guards against a
// rare double-create race.
func (h *Hub) GrouperFor(aspect string) *Grouper {
	h.mu.RLock()
	g, ok := h.groupers[aspect]
	h.mu.RUnlock()
	if ok {
		return g
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if g, ok = h.groupers[aspect]; ok {
		return g
	}
	g = NewGrouper(aspect, func(f Frame) {
		h.buffer.Append(f)
		if h.onFrame != nil {
			h.onFrame(aspect, f)
		}
	})
	h.groupers[aspect] = g
	return g
}

// SetOnFrame replaces the broadcast callback. Used by callers that
// need to construct the Hub before the consumer (e.g. the broker) is
// available — pass a nil/placeholder closure to NewHub, then call
// SetOnFrame once the consumer exists.
//
// Must be called BEFORE any goroutine can call into a Grouper —
// emit reads h.onFrame without a lock, so a write here racing a
// concurrent emit trips `-race`. In practice that means SetOnFrame
// must complete before the broker's WS listener starts AND before
// any in-process funnel issues a Deliberate (the embedded Frame
// could in principle run a proactive turn before any WS connect).
// Production wiring sequences this on the startup goroutine before
// ListenAndServe.
func (h *Hub) SetOnFrame(onFrame func(aspect string, f Frame)) {
	h.onFrame = onFrame
}

// Tail returns retained frames for aspect with Sequence > sinceSeq.
// sinceSeq=0 yields the full retained tail. Delegates to the
// underlying Buffer.
func (h *Hub) Tail(aspect string, sinceSeq int64) []Frame {
	return h.buffer.Tail(aspect, sinceSeq)
}
