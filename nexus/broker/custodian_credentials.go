// Custodian-routed git credential fetch (custodian M1 wiring).
//
// For kind="git", the broker can route the agent credential.fetch to the CWB
// custodian pillar instead of serving from its own in-memory credential store.
// This is additive + fail-safe: when no custodian client is configured
// (CUSTODIAN_GRPC_ADDR unset), the broker keeps serving ALL kinds — including
// git — from the local store, so there is no regression. Non-git kinds
// (provider/jira/imap) ALWAYS stay on the local store regardless of custodian
// configuration; only git is routed.
//
// custodian is the canonical credential vault (the security design's
// brokered-use model): it seals credentials under a per-org DEK, returns
// plaintext only over mTLS, and audits every fetch. The broker calls it as a
// trusted internal client — it presents a cwb-ca client cert and injects the
// cwb-subject / cwb-org / cwb-scopes metadata custodian's identity gate reads.
// (Interchange is the public metadata-injector; for this internal broker→pillar
// hop the broker injects directly. The org is the configured interim
// management org until herald per-agent org binding lands — see CustodianOrg.)
package broker

import (
	"context"
	"fmt"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GitCredentialSource fetches a git credential bundle for an acting agent in a
// given org. It abstracts the custodian gRPC client so the routing logic is
// testable without a live custodian. Implementations must scope the fetch to
// org (never trust a caller-supplied org beyond what the broker configures).
type GitCredentialSource interface {
	// FetchGit returns the {username,password,host} bundle for the git host.
	// identity is the acting agent (audited by custodian); org is the tenant
	// the broker presents. An error means the fetch failed or was denied —
	// the caller must NOT fall back to the local store (that would defeat the
	// custodian routing), it surfaces the error.
	FetchGit(ctx context.Context, identity, org, host string) (username, password, gotHost string, err error)
}

// custodianClient is the gRPC-backed GitCredentialSource. It dials custodian
// (mTLS — the dial credentials are supplied by the constructor) and injects the
// cwb-* identity metadata custodian's gate reads.
type custodianClient struct {
	cc  *grpc.ClientConn
	svc cwbv1.CredentialServiceClient
}

// NewCustodianClient builds a GitCredentialSource over an existing gRPC
// connection (the caller owns mTLS dial options + the conn lifetime). Returns
// an error if conn is nil.
func NewCustodianClient(conn *grpc.ClientConn) (GitCredentialSource, error) {
	if conn == nil {
		return nil, fmt.Errorf("custodian client: nil grpc conn")
	}
	return &custodianClient{cc: conn, svc: cwbv1.NewCredentialServiceClient(conn)}, nil
}

// FetchGit issues a custodian Fetch for kind="git", name=host. It injects the
// herald-style identity metadata (cwb-subject = the acting agent, cwb-org = the
// configured org, cwb-scopes = "cred:read") so custodian's org-scoped,
// scope-gated, audited fetch applies.
func (c *custodianClient) FetchGit(ctx context.Context, identity, org, host string) (string, string, string, error) {
	md := metadata.New(map[string]string{
		"cwb-subject": identity,
		"cwb-org":     org,
		"cwb-scopes":  "cred:read",
	})
	ctx = metadata.NewOutgoingContext(ctx, md)
	resp, err := c.svc.Fetch(ctx, &cwbv1.FetchRequest{
		Identity: identity,
		Kind:     "git",
		Name:     host,
	})
	if err != nil {
		return "", "", "", err
	}
	gb := resp.GetGitBundle()
	if gb == nil {
		return "", "", "", fmt.Errorf("custodian: fetch git %q: empty bundle", host)
	}
	return gb.GetUsername(), gb.GetPassword(), gb.GetHost(), nil
}
