//go:build !ctxmap_llama

package ctxmapwire

import "log/slog"

// Build (default build): the llama-backed extractor is not compiled in. If an
// operator asked for memory, warn once — a silent no-op would look like memory
// is on when it is not — then run without it.
func Build(cfg Config, log *slog.Logger) Handle {
	if cfg.Enabled {
		if log == nil {
			log = slog.Default()
		}
		log.Warn("ctxmap: CTXMAP_ENABLED set but this binary was built without the ctxmap_llama tag — working memory NOT attached (rebuild with -tags ctxmap_llama)")
	}
	return nopHandle{}
}
