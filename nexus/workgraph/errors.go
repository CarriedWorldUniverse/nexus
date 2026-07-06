package workgraph

// sentinelErr is a tiny error helper, mirroring nexus/cmd/nexus's osErr.
type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const (
	// ErrAlreadyClaimed surfaces ledger's atomic claim-lease conflict —
	// another agent claimed the issue first.
	ErrAlreadyClaimed = sentinelErr("workgraph: issue already claimed")
	// ErrNoLedgerStatus is returned by Transition when the requested Status
	// has no ledger workflow-state mapping (rejected has none — use Rework).
	ErrNoLedgerStatus = sentinelErr("workgraph: status has no ledger workflow-state mapping")
	// ErrMissingTLS is returned by DialCreds when mTLS material is absent
	// and dev-insecure is not explicitly opted into.
	ErrMissingTLS = sentinelErr("workgraph: mTLS required — set WORKGRAPH_TLS_CERT/_KEY/_CA (or WORKGRAPH_DEV_INSECURE=1)")
	// ErrBadCA is returned when the configured CA file has no parseable certs.
	ErrBadCA = sentinelErr("workgraph: no certs parsed from WORKGRAPH_TLS_CA")
)
