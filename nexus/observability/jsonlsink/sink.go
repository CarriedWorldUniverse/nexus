// Package jsonlsink persists observability frames as JSONL files for
// retrospective debugging. Each aspect gets its own daily-rotated file
// at <root>/<aspect>/<YYYY-MM-DD>.jsonl; one frame per line, JSON-
// encoded. Append-only, no schema migration, easy to grep/jq/tail.
//
// Co-exists with the existing in-memory Hub fan-out: the Sink is just
// another onFrame subscriber, chained into the Hub's broadcast callback
// alongside the live WS broadcaster. Live subscribers see no change.
//
// Concurrency: one goroutine per aspect file, fed via a buffered
// channel. Writes are non-blocking from the caller's perspective up to
// the channel capacity; if a single aspect's channel saturates (e.g.
// disk stall), writes drop with a logged warning rather than blocking
// the broker fan-out.
package jsonlsink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// channelCap bounds each per-aspect write channel. Sized for short
// transient disk stalls without blocking emit; if exceeded, frames
// drop with a counter rather than backpressure into the broker fan-out.
const channelCap = 256

// Sink writes observability frames to per-aspect daily-rotated JSONL
// files. Construct with New, wire .OnFrame into the Hub's broadcast
// chain, call Close on shutdown to flush + close all open files.
type Sink struct {
	root string
	log  *slog.Logger

	mu      sync.Mutex
	writers map[string]*aspectWriter // key: aspect name
	closed  bool
	wg      sync.WaitGroup
}

// New constructs a Sink rooted at the given directory. The directory
// is created (with parents) if absent. Returns an error only if the
// directory cannot be created.
func New(root string, log *slog.Logger) (*Sink, error) {
	if root == "" {
		return nil, errors.New("jsonlsink: root required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("jsonlsink: mkdir %q: %w", root, err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sink{
		root:    root,
		log:     log,
		writers: make(map[string]*aspectWriter),
	}, nil
}

// OnFrame is the callback to plug into observability.Hub.SetOnFrame.
// Routes the frame to the per-aspect writer goroutine. Returns
// immediately; write completion is async.
//
// Frames for empty-aspect (broker-level) are dropped — observability
// Frames always carry an aspect today. If that changes, add a "_broker"
// bucket here.
func (s *Sink) OnFrame(aspect string, frame observability.Frame) {
	if aspect == "" {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	w, ok := s.writers[aspect]
	if !ok {
		w = s.newAspectWriter(aspect)
		s.writers[aspect] = w
	}
	s.mu.Unlock()

	select {
	case w.ch <- frame:
	default:
		// Channel full — disk stall or pathological burst. Drop the
		// frame and bump the counter. Avoiding backpressure here is a
		// deliberate choice: a stuck disk should not block the broker's
		// live fan-out path that's also reading from the same Hub.
		w.recordDrop()
	}
}

// Close flushes pending writes and closes all open files. Safe to
// call multiple times. Blocks until every aspect's queue drains.
func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, w := range s.writers {
		close(w.ch)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// aspectWriter owns one aspect's file handle + the goroutine that
// drains its channel. The current file is rotated on day boundary.
type aspectWriter struct {
	aspect string
	ch     chan observability.Frame
	parent *Sink

	mu       sync.Mutex
	file     *os.File
	day      string // YYYY-MM-DD of currently-open file
	drops    int64
	lastWarn time.Time
}

func (s *Sink) newAspectWriter(aspect string) *aspectWriter {
	w := &aspectWriter{
		aspect: aspect,
		ch:     make(chan observability.Frame, channelCap),
		parent: s,
	}
	s.wg.Add(1)
	go w.run()
	return w
}

func (w *aspectWriter) run() {
	defer w.parent.wg.Done()
	for frame := range w.ch {
		w.write(frame)
	}
	// Channel closed — flush + close the file.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

func (w *aspectWriter) write(frame observability.Frame) {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := frame.TS.UTC().Format("2006-01-02")
	if day == "" {
		// Defensive: zero-time frame. Use today.
		day = time.Now().UTC().Format("2006-01-02")
	}
	if w.file == nil || w.day != day {
		if err := w.rotateLocked(day); err != nil {
			w.parent.log.Warn("jsonlsink: rotate failed",
				"aspect", w.aspect, "day", day, "err", err)
			return
		}
	}

	line, err := json.Marshal(frame)
	if err != nil {
		w.parent.log.Warn("jsonlsink: marshal failed",
			"aspect", w.aspect, "kind", frame.Kind, "err", err)
		return
	}
	line = append(line, '\n')
	if _, err := w.file.Write(line); err != nil {
		w.parent.log.Warn("jsonlsink: write failed",
			"aspect", w.aspect, "day", day, "err", err)
	}
}

// rotateLocked opens (or re-opens) the file for the given day. Caller
// must hold w.mu.
func (w *aspectWriter) rotateLocked(day string) error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	dir := filepath.Join(w.parent.root, w.aspect)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	path := filepath.Join(dir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	w.file = f
	w.day = day
	return nil
}

// recordDrop bumps the drop counter and emits a rate-limited warning.
// One warning per minute per aspect — enough signal to know the sink
// is dropping without spamming logs during a real outage.
func (w *aspectWriter) recordDrop() {
	w.mu.Lock()
	w.drops++
	drops := w.drops
	now := time.Now()
	shouldWarn := now.Sub(w.lastWarn) > time.Minute
	if shouldWarn {
		w.lastWarn = now
	}
	w.mu.Unlock()
	if shouldWarn {
		w.parent.log.Warn("jsonlsink: channel full, dropping frame",
			"aspect", w.aspect, "total_drops", drops)
	}
}
