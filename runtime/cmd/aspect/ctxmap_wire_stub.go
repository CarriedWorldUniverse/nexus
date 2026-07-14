//go:build !ctxmap_llama

package main

import (
	"io"
	"log/slog"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// attachCtxmap (default build): the llama-backed extractor is not compiled in,
// so working memory cannot attach. If an operator asked for it, say so loudly
// once — a silent no-op would look like memory is on when it is not — then run
// exactly as before.
func attachCtxmap(_ *bridle.Harness, cfg ctxmapWireConfig, log *slog.Logger) io.Closer {
	if cfg.Enabled && log != nil {
		log.Warn("ctxmap: CTXMAP_ENABLED set but this binary was built without the ctxmap_llama tag — working memory NOT attached (rebuild with -tags ctxmap_llama)")
	}
	return nopCloser{}
}
