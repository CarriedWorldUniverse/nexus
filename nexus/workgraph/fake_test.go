package workgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeLedger is an in-memory stand-in for the sovereign ledger's
// IssueService/ProjectService/AdminService, covering exactly the RPCs
// workgraph.Client calls. It satisfies issueClient/projectClient/
// adminClient structurally (see client.go).
type fakeLedger struct {
	mu       sync.Mutex
	seq      int
	issues   map[string]*fakeIssue
	links    []fakeLink
	comments map[string][]fakeComment
	claimed  map[string]string
	orgs     map[string]bool
	projects map[string]bool
}

type fakeIssue struct {
	key, project, typ, status, summary, description, dod, parentKey string
	assigneeAspect                                                  string
	skills                                                          []string
}

type fakeLink struct{ from, to, typ string }

type fakeComment struct{ actor, body string }

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		issues:   map[string]*fakeIssue{},
		comments: map[string][]fakeComment{},
		claimed:  map[string]string{},
		orgs:     map[string]bool{},
		projects: map[string]bool{},
	}
}

// --- IssueService ---

func (f *fakeLedger) CreateIssue(_ context.Context, in *cwbv1.CreateIssueRequest, _ ...grpc.CallOption) (*cwbv1.CreateIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	key := fmt.Sprintf("NET-%d", f.seq)
	f.issues[key] = &fakeIssue{
		key: key, project: in.GetProject(), typ: in.GetType(),
		status: "To Do", summary: in.GetSummary(), description: in.GetDescription(),
		dod: in.GetDefinitionOfDone(), parentKey: in.GetParentKey(), skills: in.GetSkills(),
		assigneeAspect: in.GetAssigneeAspect(),
	}
	return &cwbv1.CreateIssueResponse{Issue: f.toProto(f.issues[key])}, nil
}

func (f *fakeLedger) GetIssue(_ context.Context, in *cwbv1.GetIssueRequest, _ ...grpc.CallOption) (*cwbv1.GetIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[in.GetKey()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such issue")
	}
	return &cwbv1.GetIssueResponse{Issue: f.toProto(iss)}, nil
}

func (f *fakeLedger) toProto(iss *fakeIssue) *cwbv1.Issue {
	return &cwbv1.Issue{
		Key: iss.key, Project: iss.project, Type: iss.typ, Status: iss.status,
		Summary: iss.summary, Description: iss.description, DefinitionOfDone: iss.dod,
		ParentKey: iss.parentKey, Skills: iss.skills, AssigneeAspect: iss.assigneeAspect,
	}
}

func (f *fakeLedger) TransitionIssue(_ context.Context, in *cwbv1.TransitionIssueRequest, _ ...grpc.CallOption) (*cwbv1.TransitionIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[in.GetKey()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such issue")
	}
	// Mirrors the live ledger's DoD gate on Done: reject if any
	// definition_of_done checklist line is still unticked (see
	// adapter.go's dodUnticked/dodTicked, tickDoD).
	if in.GetStatus() == "Done" && strings.Contains(iss.dod, dodUnticked) {
		return nil, status.Error(codes.InvalidArgument, `cannot transition to "Done": definition of done has unticked items`)
	}
	iss.status = in.GetStatus()
	return &cwbv1.TransitionIssueResponse{}, nil
}

func (f *fakeLedger) UpdateIssue(_ context.Context, in *cwbv1.UpdateIssueRequest, _ ...grpc.CallOption) (*cwbv1.UpdateIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[in.GetKey()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such issue")
	}
	// Empty string = no change, mirroring the live UpdateIssueRequest's
	// *string-pointer-like semantics (see cwb-proto's comment on the field).
	if in.GetDefinitionOfDone() != "" {
		iss.dod = in.GetDefinitionOfDone()
	}
	if in.GetSummary() != "" {
		iss.summary = in.GetSummary()
	}
	if in.GetDescription() != "" {
		iss.description = in.GetDescription()
	}
	return &cwbv1.UpdateIssueResponse{}, nil
}

func (f *fakeLedger) CommentIssue(_ context.Context, in *cwbv1.CommentIssueRequest, _ ...grpc.CallOption) (*cwbv1.CommentIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[in.GetKey()]; !ok {
		return nil, status.Error(codes.NotFound, "no such issue")
	}
	f.comments[in.GetKey()] = append(f.comments[in.GetKey()], fakeComment{actor: in.GetActor(), body: in.GetBody()})
	return &cwbv1.CommentIssueResponse{}, nil
}

func (f *fakeLedger) ListComments(_ context.Context, in *cwbv1.ListCommentsRequest, _ ...grpc.CallOption) (*cwbv1.ListCommentsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*cwbv1.Event
	for _, c := range f.comments[in.GetKey()] {
		payload, _ := json.Marshal(map[string]string{"body": c.body, "actor": c.actor})
		out = append(out, &cwbv1.Event{IssueKey: in.GetKey(), Kind: "comment", Actor: c.actor, Payload: string(payload)})
	}
	return &cwbv1.ListCommentsResponse{Comments: out}, nil
}

func (f *fakeLedger) ClaimIssue(_ context.Context, in *cwbv1.ClaimIssueRequest, _ ...grpc.CallOption) (*cwbv1.ClaimIssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[in.GetKey()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such issue")
	}
	if existing, claimed := f.claimed[in.GetKey()]; claimed && existing != in.GetActor() {
		return nil, status.Error(codes.FailedPrecondition, "already claimed")
	}
	f.claimed[in.GetKey()] = in.GetActor()
	return &cwbv1.ClaimIssueResponse{Issue: f.toProto(iss)}, nil
}

func (f *fakeLedger) AddLink(_ context.Context, in *cwbv1.AddLinkRequest, _ ...grpc.CallOption) (*cwbv1.AddLinkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[in.GetKey()]; !ok {
		return nil, status.Error(codes.NotFound, "no such from issue")
	}
	if _, ok := f.issues[in.GetToKey()]; !ok {
		return nil, status.Error(codes.NotFound, "no such to issue")
	}
	f.links = append(f.links, fakeLink{from: in.GetKey(), to: in.GetToKey(), typ: in.GetType()})
	return &cwbv1.AddLinkResponse{FromKey: in.GetKey(), ToKey: in.GetToKey(), Type: in.GetType()}, nil
}

func (f *fakeLedger) ListLinks(_ context.Context, in *cwbv1.ListLinksRequest, _ ...grpc.CallOption) (*cwbv1.ListLinksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*cwbv1.LinkRow
	for _, l := range f.links {
		switch in.GetKey() {
		case l.from:
			out = append(out, &cwbv1.LinkRow{FromKey: l.from, ToKey: l.to, Type: l.typ, Direction: "outgoing"})
		case l.to:
			out = append(out, &cwbv1.LinkRow{FromKey: l.from, ToKey: l.to, Type: l.typ, Direction: "incoming"})
		}
	}
	return &cwbv1.ListLinksResponse{Links: out}, nil
}

// terminalBlockerStatuses are the blocker statuses that stop a "blocks" edge
// from holding its dependent back. Only "Done" is confirmed against the live
// ledger (unit-1's e2e didn't exercise a cancelled blocker) — see README.md.
var terminalBlockerStatuses = map[string]bool{"Done": true}

// readyStatuses are the ledger workflow states ListReadyIssues considers
// live (per the live-ledger audit: grpcserver_issue.go -> search.go
// ListReady, `status IN ('To Do', 'In Progress')`).
var readyStatuses = map[string]bool{"To Do": true, "In Progress": true}

// ListReadyIssues mirrors the live sovereign ledger's real behavior
// (confirmed by the unit-1 live e2e against ledger.cwb.svc), NOT a
// convenient approximation:
//   - assignee_aspect must equal the CALLER's cwb-subject (metadata on the
//     outgoing/here-also-incoming ctx) — ledger's ListReadyIssues is
//     aspect-assigned, the request's Aspect field is not what gates it live
//     (kept here as an additional filter for parity, but the subject match
//     is what the real server enforces)
//   - status must be "To Do" or "In Progress" (readyStatuses)
//   - "definition of ready": summary, definition_of_done and type must all
//     be non-empty
//   - not blocked: no incoming "blocks" edge from a blocker whose status
//     isn't terminal (terminalBlockerStatuses)
func (f *fakeLedger) ListReadyIssues(ctx context.Context, in *cwbv1.ListReadyIssuesRequest, _ ...grpc.CallOption) (*cwbv1.ListReadyIssuesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	subject := callerSubject(ctx)

	var out []*cwbv1.IssueRef
	for key, iss := range f.issues {
		if iss.assigneeAspect == "" || iss.assigneeAspect != subject {
			continue
		}
		if in.GetAspect() != "" && iss.assigneeAspect != in.GetAspect() {
			continue
		}
		if !readyStatuses[iss.status] {
			continue
		}
		if iss.summary == "" || iss.dod == "" || iss.typ == "" {
			continue // fails definition-of-ready
		}
		blocked := false
		for _, l := range f.links {
			if l.to != key || l.typ != "blocks" {
				continue
			}
			blocker, ok := f.issues[l.from]
			if !ok || !terminalBlockerStatuses[blocker.status] {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, &cwbv1.IssueRef{Key: key, Status: iss.status})
	}
	return &cwbv1.ListReadyIssuesResponse{Issues: out}, nil
}

// callerSubject reads the cwb-subject the client attached via
// metadata.AppendToOutgoingContext (client.go's ctx/ctxAs). This is an
// in-process fake with no real gRPC transport in between, so the outgoing
// metadata the caller set is still readable here — a real server would read
// it from the incoming side after the wire round-trip.
func callerSubject(ctx context.Context) string {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("cwb-subject")
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (f *fakeLedger) SetProjectWorkflow(_ context.Context, _ *cwbv1.SetProjectWorkflowRequest, _ ...grpc.CallOption) (*cwbv1.SetProjectWorkflowResponse, error) {
	return &cwbv1.SetProjectWorkflowResponse{}, nil
}

func (f *fakeLedger) GetProjectWorkflow(_ context.Context, _ *cwbv1.GetProjectWorkflowRequest, _ ...grpc.CallOption) (*cwbv1.GetProjectWorkflowResponse, error) {
	return &cwbv1.GetProjectWorkflowResponse{}, nil
}

// --- ProjectService ---

func (f *fakeLedger) CreateProject(_ context.Context, in *cwbv1.CreateProjectRequest, _ ...grpc.CallOption) (*cwbv1.CreateProjectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[in.GetKey()] = true
	return &cwbv1.CreateProjectResponse{Key: in.GetKey(), Name: in.GetName()}, nil
}

func (f *fakeLedger) ListProjects(_ context.Context, _ *cwbv1.ListProjectsRequest, _ ...grpc.CallOption) (*cwbv1.ListProjectsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*cwbv1.Project
	for key := range f.projects {
		out = append(out, &cwbv1.Project{Key: key})
	}
	return &cwbv1.ListProjectsResponse{Projects: out}, nil
}

// --- AdminService ---

func (f *fakeLedger) CreateOrg(_ context.Context, in *cwbv1.CreateOrgRequest, _ ...grpc.CallOption) (*cwbv1.CreateOrgResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[in.GetSlug()] = true
	return &cwbv1.CreateOrgResponse{Org: &cwbv1.Organisation{Slug: in.GetSlug(), Name: in.GetName()}}, nil
}

func (f *fakeLedger) GetOrg(_ context.Context, in *cwbv1.GetOrgRequest, _ ...grpc.CallOption) (*cwbv1.GetOrgResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.orgs[in.GetSlug()] {
		return nil, status.Error(codes.NotFound, "no such org")
	}
	return &cwbv1.GetOrgResponse{Org: &cwbv1.Organisation{Slug: in.GetSlug()}}, nil
}

func (f *fakeLedger) CreateUser(_ context.Context, _ *cwbv1.CreateUserRequest, _ ...grpc.CallOption) (*cwbv1.CreateUserResponse, error) {
	return &cwbv1.CreateUserResponse{}, nil
}

func (f *fakeLedger) AddMember(_ context.Context, _ *cwbv1.AddMemberRequest, _ ...grpc.CallOption) (*cwbv1.AddMemberResponse, error) {
	return &cwbv1.AddMemberResponse{}, nil
}

// newTestClient builds a Client wired directly to a fresh fakeLedger — no
// network/dial involved (fakeLedger satisfies issueClient/projectClient/
// adminClient structurally).
func newTestClient() (*Client, *fakeLedger) {
	f := newFakeLedger()
	return &Client{issue: f, project: f, admin: f, Org: DefaultOrg, Subject: "test", Project: DefaultProject}, f
}
