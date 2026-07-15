//go:build ctxmap_llama

package ctxmapwire

import (
	"log/slog"
	"sync"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/adapter"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/embed"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/extractor"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/memory"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/render"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/store"
)

// Build (ctxmap_llama build): construct the working-memory engine. Fail-open —
// any init error logs and returns the Nop handle so the runtime runs without
// memory rather than not at all.
func Build(cfg Config, log *slog.Logger) Handle {
	if log == nil {
		log = slog.Default()
	}
	if !cfg.Enabled {
		return nopHandle{}
	}
	ex, err := extractor.New(extractor.Config{
		ExtractModelPath: cfg.Extract,
		KindModelPath:    cfg.Kind,
		Threads:          cfg.Threads,
	})
	if err != nil {
		log.Warn("ctxmap: extractor init failed; working memory disabled", "err", err)
		return nopHandle{}
	}
	st, err := store.Open(cfg.StorePath)
	if err != nil {
		log.Warn("ctxmap: store open failed; working memory disabled", "path", cfg.StorePath, "err", err)
		ex.Close()
		return nopHandle{}
	}
	rend, err := render.New(st)
	if err != nil {
		log.Warn("ctxmap: renderer init failed; working memory disabled", "err", err)
		st.Close()
		ex.Close()
		return nopHandle{}
	}
	var emb embed.Embedder
	if e, err := embed.NewLlama(cfg.Embed, 8); err == nil {
		emb = e
	} else {
		log.Info("ctxmap: embedder unavailable; reconciliation falls back to token heuristics", "err", err)
	}
	eng := memory.New(memory.Config{SessionID: cfg.Session}, st, rend, ex, emb, ex)
	log.Info("ctxmap: working memory engine ready", "store", cfg.StorePath, "session", cfg.Session, "threads", cfg.Threads)
	return &llamaHandle{eng: eng, ex: ex, emb: emb, st: st}
}

type llamaHandle struct {
	eng *memory.Engine
	ex  *extractor.Extractor
	emb embed.Embedder
	st  *store.Store

	mu      sync.Mutex
	detach  []func()
	closed  bool
}

// AttachTo wires the shared engine to a harness. Safe to call for each harness
// the runtime builds; the engine (and its authoritative turn counter) is shared,
// so a mid-session harness rebuild keeps a continuous memory.
func (h *llamaHandle) AttachTo(harness *bridle.Harness) {
	if harness == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.detach = append(h.detach, adapter.Attach(harness, h.eng))
}

// Close detaches from every harness, drains the extraction worker, then frees
// the models and the store.
func (h *llamaHandle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	detach := h.detach
	h.detach = nil
	h.mu.Unlock()

	for _, d := range detach {
		d()
	}
	h.eng.Close() // drains the async extraction worker
	h.ex.Close()
	if c, ok := h.emb.(interface{ Close() }); ok && c != nil {
		c.Close()
	}
	h.st.Close()
	return nil
}
