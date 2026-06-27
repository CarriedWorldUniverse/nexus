package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"strings"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"github.com/CarriedWorldUniverse/nexus/nexus/pbreconcile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// buildAlmanacReader constructs the broker's READ-ONLY almanac client used to
// reconcile aspect provider-bindings live from configuration truth (INC-4a).
// Dark by default: ALMANAC_GRPC_ADDR unset → nil, and the broker keeps
// resolving bindings from its local sqld store (no regression; boots-standalone
// preserved). Any config/dial failure logs and degrades to nil — bindings stay
// local rather than the broker failing to boot.
//
// Env:
//
//	ALMANAC_GRPC_ADDR     almanac gRPC (e.g. almanac.cwb.svc:8083). Unset → disabled.
//	ALMANAC_GRPC_ORG      tenant org presented to almanac (cwb-org). Required when ADDR set.
//	ALMANAC_GRPC_SUBJECT  cwb-subject presented to almanac (default "nexus-broker").
//	ALMANAC_TLS_CERT      broker client cert (PEM) for mTLS to almanac
//	ALMANAC_TLS_KEY       broker client key (PEM)
//	ALMANAC_TLS_CA        cwb CA (PEM) to verify almanac's server cert
//	ALMANAC_DEV_INSECURE=1  dial without mTLS (local dev only)
func buildAlmanacReader(logger *slog.Logger) pbreconcile.Reader {
	addr := os.Getenv("ALMANAC_GRPC_ADDR")
	if addr == "" {
		return nil
	}
	org := os.Getenv("ALMANAC_GRPC_ORG")
	if org == "" {
		logger.Warn("ALMANAC_GRPC_ADDR set but ALMANAC_GRPC_ORG empty — live provider-binding reconcile DISABLED (bindings stay local)")
		return nil
	}
	sub := os.Getenv("ALMANAC_GRPC_SUBJECT")
	if sub == "" {
		sub = "nexus-broker"
	}
	dialCreds, err := almanacDialCreds()
	if err != nil {
		logger.Warn("provider-binding reconcile DISABLED — TLS config error (bindings stay local)", "err", err)
		return nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		logger.Warn("provider-binding reconcile DISABLED — dial failed (bindings stay local)", "addr", addr, "err", err)
		return nil
	}
	logger.Info("provider-binding reconcile ENABLED", "addr", addr, "org", org, "subject", sub, "prefix", pbreconcile.Prefix)
	return &almanacReader{client: cwbv1.NewConfigServiceClient(conn), org: org, sub: sub}
}

// almanacReader implements pbreconcile.Reader against almanac's ConfigService,
// mirroring mason's snapshot semantics: list the binding prefix, fall back to
// GetConfig for any item whose List value is empty, and abort the WHOLE
// snapshot on any error so a partial view never zeroes a binding.
type almanacReader struct {
	client cwbv1.ConfigServiceClient
	org    string
	sub    string
}

func (a *almanacReader) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"cwb-subject", a.sub, "cwb-org", a.org, "cwb-scopes", "config:read")
}

func (a *almanacReader) Snapshot(ctx context.Context) (map[string]string, error) {
	resp, err := a.client.ListConfig(a.ctx(ctx), &cwbv1.ListConfigRequest{Prefix: pbreconcile.Prefix})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.GetItems()))
	for _, it := range resp.GetItems() {
		name := strings.TrimPrefix(it.GetPath(), pbreconcile.Prefix)
		if name == "" || strings.Contains(name, "/") {
			continue // only direct children of the prefix = aspect names
		}
		v := it.GetValue()
		if v == "" {
			g, err := a.client.GetConfig(a.ctx(ctx), &cwbv1.GetConfigRequest{Path: it.GetPath()})
			if err != nil {
				return nil, err
			}
			v = g.GetItem().GetValue()
		}
		out[name] = v
	}
	return out, nil
}

// almanacDialCreds builds gRPC transport credentials for the almanac dial: mTLS
// with the broker's client cert, or insecure under the explicit dev opt-in.
func almanacDialCreds() (credentials.TransportCredentials, error) {
	certFile := os.Getenv("ALMANAC_TLS_CERT")
	keyFile := os.Getenv("ALMANAC_TLS_KEY")
	caFile := os.Getenv("ALMANAC_TLS_CA")
	if certFile == "" || keyFile == "" || caFile == "" {
		if os.Getenv("ALMANAC_DEV_INSECURE") == "1" {
			return insecure.NewCredentials(), nil
		}
		return nil, osErr("almanac: mTLS required — set ALMANAC_TLS_CERT/_KEY/_CA (or ALMANAC_DEV_INSECURE=1)")
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
		return nil, osErr("almanac: no certs parsed from ALMANAC_TLS_CA")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
