package workgraph

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DialCreds builds the gRPC transport credentials for the sovereign-ledger
// dial: mTLS with the caller's cwb mesh client cert, or insecure under the
// explicit dev opt-in. Mirrors nexus/cmd/nexus's custodianDialCreds /
// almanacDialCreds — same env-var convention, workgraph-scoped names.
//
// Env:
//
//	WORKGRAPH_TLS_CERT      cwb mesh client cert (PEM) for mTLS to ledger
//	WORKGRAPH_TLS_KEY       cwb mesh client key (PEM)
//	WORKGRAPH_TLS_CA        cwb CA (PEM) to verify ledger's server cert
//	WORKGRAPH_DEV_INSECURE=1  dial without mTLS (local dev only)
func DialCreds() (credentials.TransportCredentials, error) {
	certFile := os.Getenv("WORKGRAPH_TLS_CERT")
	keyFile := os.Getenv("WORKGRAPH_TLS_KEY")
	caFile := os.Getenv("WORKGRAPH_TLS_CA")
	if certFile == "" || keyFile == "" || caFile == "" {
		if os.Getenv("WORKGRAPH_DEV_INSECURE") == "1" {
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
