//go:build ctxmap_llama

package main

import (
	"io"
	"log/slog"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/adapter"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/embed"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/extractor"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/memory"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/render"
	"github.com/CarriedWorldUniverse/bridle/ctxmap/store"
)

// attachCtxmap (ctxmap_llama build): construct the working-memory engine and
// attach it to the harness. Fail-open — any init error logs and returns a no-op
// closer so the aspect runs without memory rather than not at all.
func attachCtxmap(h *bridle.Harness, cfg ctxmapWireConfig, log *slog.Logger) io.Closer {
	if log == nil {
		log = slog.Default()
	}
	if !cfg.Enabled {
		return nopCloser{}
	}
	ex, err := extractor.New(extractor.Config{
		ExtractModelPath: cfg.Extract,
		KindModelPath:    cfg.Kind,
		Threads:          cfg.Threads,
	})
	if err != nil {
		log.Warn("ctxmap: extractor init failed; working memory disabled", "err", err)
		return nopCloser{}
	}
	st, err := store.Open(cfg.StorePath)
	if err != nil {
		log.Warn("ctxmap: store open failed; working memory disabled", "path", cfg.StorePath, "err", err)
		ex.Close()
		return nopCloser{}
	}
	rend, err := render.New(st)
	if err != nil {
		log.Warn("ctxmap: renderer init failed; working memory disabled", "err", err)
		st.Close()
		ex.Close()
		return nopCloser{}
	}
	var emb embed.Embedder
	if e, err := embed.NewLlama(cfg.Embed, 8); err == nil {
		emb = e
	} else {
		log.Info("ctxmap: embedder unavailable; reconciliation falls back to token heuristics", "err", err)
	}
	eng := memory.New(memory.Config{SessionID: cfg.Session}, st, rend, ex, emb, ex)
	detach := adapter.Attach(h, eng)
	log.Info("ctxmap: working memory attached", "store", cfg.StorePath, "session", cfg.Session, "threads", cfg.Threads)
	return &ctxmapCloser{detach: detach, eng: eng, ex: ex, emb: emb, st: st}
}

type ctxmapCloser struct {
	detach func()
	eng    *memory.Engine
	ex     *extractor.Extractor
	emb    embed.Embedder
	st     *store.Store
}

// Close tears down in dependency order: stop feeding the harness, drain the
// extraction worker, then free the models and the store.
func (c *ctxmapCloser) Close() error {
	if c.detach != nil {
		c.detach()
	}
	if c.eng != nil {
		c.eng.Close() // drains the async extraction worker
	}
	if c.ex != nil {
		c.ex.Close()
	}
	if closer, ok := c.emb.(io.Closer); ok && closer != nil {
		closer.Close()
	}
	if c.st != nil {
		c.st.Close()
	}
	return nil
}
