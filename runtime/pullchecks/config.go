package pullchecks

import (
	"log/slog"
	"os"

	"google.golang.org/grpc"
)

// NewRecorderFromEnv dials cairn-server and builds a Recorder when
// CW_PULL_SERVER_ADDR is set. Dark by default — unset means nil, and the
// caller must treat nil as "feature off, make zero PullService calls" (this
// is the back-compat contract: every existing gate path that never checks
// CW_PULL_* behaves byte-identically to before this package existed). Any
// dial/config failure logs and returns nil so the run is never blocked on
// pull-checks wiring — recording verdicts is best-effort, never load-bearing
// for the gate's own pass/fail decision.
//
// Env:
//
//	CW_PULL_SERVER_ADDR   cairn-server gRPC address. Unset → recorder not built (dark).
//	CW_PULL_ORG           cwb-org the repo lives under.
//	CW_PULL_SLUG          the cairn repo slug pull checks are recorded against.
//	CW_PULL_PROJECT       default ledger project key EnsurePull opens pulls under.
//	CW_PULL_TLS_CERT/_KEY/_CA   mTLS material (see DialCreds).
//	CW_PULL_DEV_INSECURE=1      dial without mTLS (local dev only).
//
// CW_PULL_SERVER_ADDR set but CW_PULL_ORG/CW_PULL_SLUG/CW_PULL_PROJECT unset
// is also treated as unconfigured (logged, nil) — OpenPull/RecordPullCheck
// require org and slug path params, and cairn-server's OpenPull additionally
// rejects an empty project with InvalidArgument (every EnsurePull call would
// then fail permanently), so a Recorder missing any of the three could never
// make a valid call. Requiring all three up front turns that into a clear
// boot-time "DISABLED" log instead of a silent, permanent per-call failure.
func NewRecorderFromEnv(log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	addr := os.Getenv("CW_PULL_SERVER_ADDR")
	if addr == "" {
		return nil
	}
	org := os.Getenv("CW_PULL_ORG")
	slug := os.Getenv("CW_PULL_SLUG")
	if org == "" || slug == "" {
		log.Warn("pullchecks: CW_PULL_SERVER_ADDR set but CW_PULL_ORG/CW_PULL_SLUG missing — recorder DISABLED",
			"addr", addr, "org", org, "slug", slug)
		return nil
	}
	project := os.Getenv("CW_PULL_PROJECT")
	if project == "" {
		log.Warn("pullchecks: CW_PULL_SERVER_ADDR/_ORG/_SLUG set but CW_PULL_PROJECT unset — recorder DISABLED "+
			"(cairn-server's OpenPull rejects an empty project; every EnsurePull would fail)",
			"addr", addr, "org", org, "slug", slug)
		return nil
	}
	dialCreds, err := DialCreds()
	if err != nil {
		log.Warn("pullchecks: TLS config error — recorder DISABLED", "err", err)
		return nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		log.Warn("pullchecks: dial failed — recorder DISABLED", "addr", addr, "err", err)
		return nil
	}
	log.Info("pullchecks: recorder ENABLED", "addr", addr, "org", org, "slug", slug, "project", project)
	return New(conn, org, slug, project, log)
}
