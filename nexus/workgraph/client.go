package workgraph

import (
	"context"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"strings"

	"google.golang.org/grpc/metadata"
)

// issueClient is the narrow slice of cwbv1.IssueServiceClient this adapter
// uses. Declared locally (rather than depending on the full generated
// interface) so fakes in tests only need to implement what we call.
// *cwbv1.issueServiceClient (built by cwbv1.NewIssueServiceClient) already
// satisfies this structurally.
type issueClient interface {
	CreateIssue(ctx context.Context, in *cwbv1.CreateIssueRequest, opts ...grpc.CallOption) (*cwbv1.CreateIssueResponse, error)
	GetIssue(ctx context.Context, in *cwbv1.GetIssueRequest, opts ...grpc.CallOption) (*cwbv1.GetIssueResponse, error)
	UpdateIssue(ctx context.Context, in *cwbv1.UpdateIssueRequest, opts ...grpc.CallOption) (*cwbv1.UpdateIssueResponse, error)
	TransitionIssue(ctx context.Context, in *cwbv1.TransitionIssueRequest, opts ...grpc.CallOption) (*cwbv1.TransitionIssueResponse, error)
	CommentIssue(ctx context.Context, in *cwbv1.CommentIssueRequest, opts ...grpc.CallOption) (*cwbv1.CommentIssueResponse, error)
	ListComments(ctx context.Context, in *cwbv1.ListCommentsRequest, opts ...grpc.CallOption) (*cwbv1.ListCommentsResponse, error)
	ClaimIssue(ctx context.Context, in *cwbv1.ClaimIssueRequest, opts ...grpc.CallOption) (*cwbv1.ClaimIssueResponse, error)
	AddLink(ctx context.Context, in *cwbv1.AddLinkRequest, opts ...grpc.CallOption) (*cwbv1.AddLinkResponse, error)
	ListLinks(ctx context.Context, in *cwbv1.ListLinksRequest, opts ...grpc.CallOption) (*cwbv1.ListLinksResponse, error)
	ListReadyIssues(ctx context.Context, in *cwbv1.ListReadyIssuesRequest, opts ...grpc.CallOption) (*cwbv1.ListReadyIssuesResponse, error)
	SetProjectWorkflow(ctx context.Context, in *cwbv1.SetProjectWorkflowRequest, opts ...grpc.CallOption) (*cwbv1.SetProjectWorkflowResponse, error)
	GetProjectWorkflow(ctx context.Context, in *cwbv1.GetProjectWorkflowRequest, opts ...grpc.CallOption) (*cwbv1.GetProjectWorkflowResponse, error)
}

// projectClient is the narrow slice of cwbv1.ProjectServiceClient used here.
type projectClient interface {
	CreateProject(ctx context.Context, in *cwbv1.CreateProjectRequest, opts ...grpc.CallOption) (*cwbv1.CreateProjectResponse, error)
	ListProjects(ctx context.Context, in *cwbv1.ListProjectsRequest, opts ...grpc.CallOption) (*cwbv1.ListProjectsResponse, error)
}

// adminClient is the narrow slice of cwbv1.AdminServiceClient used here
// (EnsureProject's org bootstrap).
type adminClient interface {
	CreateOrg(ctx context.Context, in *cwbv1.CreateOrgRequest, opts ...grpc.CallOption) (*cwbv1.CreateOrgResponse, error)
	GetOrg(ctx context.Context, in *cwbv1.GetOrgRequest, opts ...grpc.CallOption) (*cwbv1.GetOrgResponse, error)
	CreateUser(ctx context.Context, in *cwbv1.CreateUserRequest, opts ...grpc.CallOption) (*cwbv1.CreateUserResponse, error)
	AddMember(ctx context.Context, in *cwbv1.AddMemberRequest, opts ...grpc.CallOption) (*cwbv1.AddMemberResponse, error)
}

// Client is the work-graph adapter: a gRPC client to the sovereign ledger,
// scoped to one cwb-org and one ledger project (see EnsureProject).
type Client struct {
	issue   issueClient
	project projectClient
	admin   adminClient

	// Org is the cwb-org presented on every RPC (X-CWB-Org / metadata
	// "cwb-org", mirroring nexus/cmd/nexus's almanacReader).
	Org string
	// Subject is the cwb-subject presented on every RPC (accountability,
	// not the ledger actor field — actor is passed per-call).
	Subject string
	// Project is the ledger project key work items are created under
	// (see EnsureProject).
	Project string
	// Scopes are the cwb-scopes asserted on every RPC. On the direct mesh
	// path (no gateway) the client self-asserts scopes; the mTLS cert is
	// the trust boundary. The orchestrator manages the whole graph
	// (create/transition/cancel + project bootstrap) so it holds
	// issue:admin, the superset for ordinary issue scopes.
	Scopes []string
}

// New builds a Client from an established gRPC connection to the sovereign
// ledger (see DialCreds for the mTLS dial). org/subject are the cwb-org and
// cwb-subject presented on every RPC; project is the ledger project key work
// items are created under — call EnsureProject first if it may not exist yet.
func New(conn grpc.ClientConnInterface, org, subject, project string) *Client {
	return &Client{
		issue:   cwbv1.NewIssueServiceClient(conn),
		project: cwbv1.NewProjectServiceClient(conn),
		admin:   cwbv1.NewAdminServiceClient(conn),
		Org:     org,
		Subject: subject,
		Project: project,
		Scopes:  []string{"issue:admin"},
	}
}

// ctx attaches the cwb-subject/cwb-org identity metadata every RPC needs
// (project creation in particular reads organisation from this context, not
// the request body — see cwb-proto's CreateProjectRequest comment).
func (c *Client) ctx(ctx context.Context) context.Context {
	return c.ctxAs(ctx, c.Subject)
}

// ctxAs is ctx but with the cwb-subject overridden. ListReady needs this:
// the live ledger's ListReadyIssues filters `assignee_aspect = <caller's
// cwb-subject>` (see README.md's "ready is aspect-scoped" note), so querying
// "as" a role means presenting that role as cwb-subject for that one call,
// not c.Subject (the adapter's own accountable identity).
func (c *Client) ctxAs(ctx context.Context, subject string) context.Context {
	ctx = metadata.AppendToOutgoingContext(ctx, "cwb-subject", subject, "cwb-org", c.Org)
	if len(c.Scopes) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, "cwb-scopes", strings.Join(c.Scopes, " "))
	}
	return ctx
}

const defaultActor = "nexus-workgraph"

// Comment tags: the JSON body of a cwb:handoff / cwb:result comment is
// prefixed with "<tag>\n" so GetWorkItem can pick it out of the issue's
// comment timeline (ledger has no first-class column for either blob).
const (
	commentTagHandoff = "cwb:handoff"
	commentTagResult  = "cwb:result"
)
