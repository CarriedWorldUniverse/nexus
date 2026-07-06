package workgraph

import (
	"context"
	"fmt"
	"strings"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultOrg and DefaultProject are the sovereign ledger's work-graph home:
// the sovereign ledger starts fresh (no orgs/projects), so EnsureProject
// creates these idempotently before any issue can be filed.
const (
	DefaultOrg     = "carriedworld"
	DefaultProject = "NET"
)

// workflowStates is the fixed workflow this adapter requires on the project
// it owns, so Status<->ledger-state folding (see README.md) is exact
// regardless of the ledger's factory-default workflow. Permissive
// transitions (every state can move to every other) except the DoD gate on
// Done, which ledger enforces per its "required DoD on Done transition"
// audit note.
var workflowStates = []struct {
	name     string
	category cwbv1.StatusCategory
	dodGate  bool
}{
	{"To Do", cwbv1.StatusCategory_STATUS_CATEGORY_DRAFT, false},
	{"Ready to Start", cwbv1.StatusCategory_STATUS_CATEGORY_READY, false},
	{"In Progress", cwbv1.StatusCategory_STATUS_CATEGORY_ACTIVE, false},
	{"Blocked", cwbv1.StatusCategory_STATUS_CATEGORY_BLOCKED, false},
	{"Done", cwbv1.StatusCategory_STATUS_CATEGORY_DONE, true},
	{"Cancelled", cwbv1.StatusCategory_STATUS_CATEGORY_CANCELLED, false},
}

// EnsureProject idempotently creates the org+project this adapter's issues
// live under (c.Org / c.Project) and sets the project's workflow to the
// fixed state set workgraph relies on (see workflowStates). Safe to call on
// every boot: a NotFound org/project is created, an existing one is left
// alone (except the workflow, which is re-asserted so drift self-heals).
func (c *Client) EnsureProject(ctx context.Context) error {
	if err := c.ensureOrg(ctx); err != nil {
		return err
	}
	if err := c.ensureProjectKey(ctx); err != nil {
		return err
	}
	if err := c.ensureWorkflow(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Client) ensureOrg(ctx context.Context) error {
	// 1. Org exists (create if absent).
	_, err := c.admin.GetOrg(c.ctx(ctx), &cwbv1.GetOrgRequest{Slug: c.Org})
	if err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("workgraph.EnsureProject: get org %q: %w", c.Org, err)
		}
		if _, err := c.admin.CreateOrg(c.ctx(ctx), &cwbv1.CreateOrgRequest{Slug: c.Org, Name: c.Org}); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("workgraph.EnsureProject: create org %q: %w", c.Org, err)
		}
	}
	// 2. Caller is a member (project creation requires it). Always ensured,
	// not only on first org creation — a pre-existing org may still lack this
	// caller as a member. Both create+add are idempotent. Subject is the
	// accountable actor for the work graph.
	if _, err := c.admin.CreateUser(c.ctx(ctx), &cwbv1.CreateUserRequest{Id: c.Subject, Kind: "ai"}); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("workgraph.EnsureProject: create user %q: %w", c.Subject, err)
	}
	if _, err := c.admin.AddMember(c.ctx(ctx), &cwbv1.AddMemberRequest{Slug: c.Org, UserId: c.Subject, Role: "owner"}); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("workgraph.EnsureProject: add member %q to %q: %w", c.Subject, c.Org, err)
	}
	return nil
}

// isAlreadyExists reports whether err signals a resource that already exists
// (so an idempotent create can ignore it). Like isNotFound, the ledger does
// not always use codes.AlreadyExists — match the code and the message.
func isAlreadyExists(err error) bool {
	if status.Code(err) == codes.AlreadyExists {
		return true
	}
	m := strings.ToLower(status.Convert(err).Message())
	return strings.Contains(m, "already exists") || strings.Contains(m, "already a member") || strings.Contains(m, "duplicate") || strings.Contains(m, "unique constraint")
}

// isNotFound reports whether err signals an absent resource. The sovereign
// ledger does not always use codes.NotFound — GetOrg on a missing org
// returns codes.Internal with a "not found" message — so match both the
// clean code and the message substring.
func isNotFound(err error) bool {
	if status.Code(err) == codes.NotFound {
		return true
	}
	return strings.Contains(strings.ToLower(status.Convert(err).Message()), "not found")
}

func (c *Client) ensureProjectKey(ctx context.Context) error {
	resp, err := c.project.ListProjects(c.ctx(ctx), &cwbv1.ListProjectsRequest{IncludeArchived: true})
	if err != nil {
		return fmt.Errorf("workgraph.EnsureProject: list projects: %w", err)
	}
	for _, p := range resp.GetProjects() {
		if p.GetKey() == c.Project {
			return nil
		}
	}
	if _, err := c.project.CreateProject(c.ctx(ctx), &cwbv1.CreateProjectRequest{
		Key:  c.Project,
		Name: c.Project,
	}); err != nil {
		return fmt.Errorf("workgraph.EnsureProject: create project %q: %w", c.Project, err)
	}
	return nil
}

func (c *Client) ensureWorkflow(ctx context.Context) error {
	names := make([]string, len(workflowStates))
	for i, s := range workflowStates {
		names[i] = s.name
	}
	states := make([]*cwbv1.WorkflowState, len(workflowStates))
	transitions := make([]*cwbv1.WorkflowTransition, len(workflowStates))
	for i, s := range workflowStates {
		states[i] = &cwbv1.WorkflowState{Name: s.name, Category: s.category, DodGate: s.dodGate}
		// Includes a self-loop (name -> name): confirmed against the live
		// ledger that a same-state Transition (e.g. Cancel(requeue=true) on
		// an item that's still "To Do", never dispatched) is rejected as
		// "not allowed by workflow" without one. Transition/Cancel are meant
		// to be idempotent-safe, so every state permits holding itself.
		to := make([]string, 0, len(names))
		to = append(to, s.name)
		for _, n := range names {
			if n != s.name {
				to = append(to, n)
			}
		}
		transitions[i] = &cwbv1.WorkflowTransition{From: s.name, To: to}
	}
	_, err := c.issue.SetProjectWorkflow(c.ctx(ctx), &cwbv1.SetProjectWorkflowRequest{
		Project:  c.Project,
		Workflow: &cwbv1.Workflow{States: states, Transitions: transitions},
	})
	if err != nil {
		return fmt.Errorf("workgraph.EnsureProject: set workflow: %w", err)
	}
	return nil
}
