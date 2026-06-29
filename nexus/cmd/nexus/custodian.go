package main

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// buildCustodianGit constructs the broker's custodian git-credential client
// from the environment. Dark by default: when CUSTODIAN_GRPC_ADDR is unset it
// returns (nil, "") and the broker serves git credentials from its local store
// (no regression). Any dial/config failure is logged and degrades to nil — the
// broker still boots and serves git locally rather than failing.
//
// Env:
//
//	CUSTODIAN_GRPC_ADDR   custodian gRPC address (e.g. custodian.cwb.svc:8085). Unset → disabled.
//	CUSTODIAN_GRPC_ORG    tenant org presented to custodian (cwb-org). Required when ADDR set.
//	CUSTODIAN_TLS_CERT    broker client cert (PEM) for mTLS to custodian
//	CUSTODIAN_TLS_KEY     broker client key (PEM)
//	CUSTODIAN_TLS_CA      cwb CA (PEM) to verify custodian's server cert
//	CUSTODIAN_DEV_INSECURE=1  dial without mTLS (local dev only)
func buildCustodianGit(logger *slog.Logger) (broker.GitCredentialSource, string) {
	addr := os.Getenv("CUSTODIAN_GRPC_ADDR")
	if addr == "" {
		return nil, ""
	}
	org := os.Getenv("CUSTODIAN_GRPC_ORG")
	if org == "" {
		logger.Warn("CUSTODIAN_GRPC_ADDR set but CUSTODIAN_GRPC_ORG empty — custodian git routing DISABLED (git stays local)")
		return nil, ""
	}

	dialCreds, err := custodianDialCreds()
	if err != nil {
		logger.Warn("custodian git routing DISABLED — TLS config error (git stays local)", "err", err)
		return nil, ""
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		logger.Warn("custodian git routing DISABLED — dial failed (git stays local)", "addr", addr, "err", err)
		return nil, ""
	}
	src, err := broker.NewCustodianClient(conn)
	if err != nil {
		logger.Warn("custodian git routing DISABLED — client build failed (git stays local)", "err", err)
		_ = conn.Close()
		return nil, ""
	}
	logger.Info("custodian git routing ENABLED", "addr", addr, "org", org)
	return src, org
}

// custodianDialCreds builds the gRPC transport credentials for the custodian
// dial: mTLS with the broker's client cert, or insecure under the explicit dev
// opt-in.
func custodianDialCreds() (credentials.TransportCredentials, error) {
	certFile := os.Getenv("CUSTODIAN_TLS_CERT")
	keyFile := os.Getenv("CUSTODIAN_TLS_KEY")
	caFile := os.Getenv("CUSTODIAN_TLS_CA")
	if certFile == "" || keyFile == "" || caFile == "" {
		if os.Getenv("CUSTODIAN_DEV_INSECURE") == "1" {
			return insecure.NewCredentials(), nil
		}
		return nil, errMissingCustodianTLS
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
		return nil, errBadCustodianCA
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

var (
	errMissingCustodianTLS = osErr("custodian: mTLS required — set CUSTODIAN_TLS_CERT/_KEY/_CA (or CUSTODIAN_DEV_INSECURE=1)")
	errBadCustodianCA      = osErr("custodian: no certs parsed from CUSTODIAN_TLS_CA")
)

// osErr is a tiny error helper to keep the var block readable.
type osErr string

func (e osErr) Error() string { return string(e) }
