// Package cwbproxy reverse-proxies CWB path prefixes to the CWB edge
// (interchange) over nexus's egress, pass-through: the caller's own bearer is
// forwarded verbatim and interchange authenticates. Serves human CLIs (cw /
// cw agent enroll) and the aspect bootstrap OIDC discovery. The host is pinned
// to the edge (callers cannot retarget).
package cwbproxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Prefixes proxied to the CWB edge.
var Prefixes = []string{"/herald/", "/cairn/", "/ledger/", "/knowledge/"}

// Register attaches the reverse-proxy routes for the CWB edge onto mux.
func Register(mux *http.ServeMux, edge string) error {
	edge = strings.TrimRight(edge, "/")
	if edge == "" {
		return fmt.Errorf("cwbproxy: empty CWB edge")
	}
	target, err := url.Parse(edge)
	if err != nil {
		return fmt.Errorf("cwbproxy: parse edge %q: %w", edge, err)
	}
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			// Custom Director intentionally omits X-Forwarded-* — this is a
			// pass-through to a trusted internal edge (interchange) where the
			// caller's own bearer is the auth signal, not client IP. Do not
			// switch to Rewrite/SetXForwarded without revisiting that.
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.Host = target.Host
			// path preserved as-is (already includes the /pillar prefix)
		},
	}
	for _, p := range Prefixes {
		mux.Handle(p, rp)
	}
	return nil
}
