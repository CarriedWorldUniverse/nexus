// Package pullchecks is the broker's client to cairn-server's PullService —
// the first PullService client in nexus (cairn#99). It lets the builder
// gates (pr-exists, pr-substantial, acceptance-judge, test-evidence) record
// their verdicts as cairn pull checks, dark by default: unless a run carries
// cairn-pull addressing (CW_PULL_* env), the recorder is never built and the
// gate path makes zero PullService calls — see
// docs/network/ACCEPTANCE-GATE-HARDENING.md.
package pullchecks

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// sentinelErr is a tiny error helper, mirroring nexus/workgraph's sentinelErr.
type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const (
	// ErrMissingTLS is returned by DialCreds when mTLS material is absent
	// and dev-insecure is not explicitly opted into.
	ErrMissingTLS = sentinelErr("pullchecks: mTLS required — set CW_PULL_TLS_CERT/_KEY/_CA (or CW_PULL_DEV_INSECURE=1)")
	// ErrBadCA is returned when the configured CA file has no parseable certs.
	ErrBadCA = sentinelErr("pullchecks: no certs parsed from CW_PULL_TLS_CA")
)

// DialCreds builds the gRPC transport credentials for the cairn-server dial:
// mTLS with the caller's cwb mesh client cert, or insecure under the
// explicit dev opt-in. Mirrors nexus/workgraph.DialCreds — same env-var
// convention, pullchecks-scoped names.
//
// Env:
//
//	CW_PULL_TLS_CERT      cwb mesh client cert (PEM) for mTLS to cairn-server
//	CW_PULL_TLS_KEY       cwb mesh client key (PEM)
//	CW_PULL_TLS_CA        cwb CA (PEM) to verify cairn-server's server cert
//	CW_PULL_DEV_INSECURE=1  dial without mTLS (local dev only)
func DialCreds() (credentials.TransportCredentials, error) {
	certFile := os.Getenv("CW_PULL_TLS_CERT")
	keyFile := os.Getenv("CW_PULL_TLS_KEY")
	caFile := os.Getenv("CW_PULL_TLS_CA")
	if certFile == "" || keyFile == "" || caFile == "" {
		if os.Getenv("CW_PULL_DEV_INSECURE") == "1" {
			return insecure.NewCredentials(), nil
		}
		return nil, ErrMissingTLS
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, ErrBadCA
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
