package workgraph

import (
	"encoding/json"
	"fmt"
	"strings"

	"context"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// statusToLedger maps a WorkItem Status to its ledger workflow-state name
// (see ensureWorkflow / README.md). StatusRejected has no ledger state — a
// reject is handled via Rework (new issue + back-edge link), never a direct
// transition on the rejected issue.
var statusToLedger = map[Status]string{
	StatusQueued:     "To Do",
	StatusReady:      "Ready to Start",
	StatusDispatched: "In Progress",
	StatusRunning:    "In Progress",
	StatusDone:       "Done",
	StatusBlocked:    "Blocked",
	StatusCancelled:  "Cancelled",
}

// ledgerToStatus is the reverse fold used by GetWorkItem. "In Progress"
// collapses dispatched/running into StatusDispatched — the finer runtime
// split lives in nexus/runs, not the ledger.
var ledgerToStatus = map[string]Status{
	"To Do":          StatusQueued,
	"Ready to Start": StatusReady,
	"In Progress":    StatusDispatched,
	"Done":           StatusDone,
	"Blocked":        StatusBlocked,
	"Cancelled":      StatusCancelled,
}

// CreateWorkItem files a new ledger issue for wi (wi.ID is ignored — the
// ledger assigns the issue key, returned as id) and links wi.DependsOn as
// blocks-edges (blocker -> this), per README.md's field mapping. It also
// records the full work_item as a cwb:handoff comment (origin/personality/
// base_knowledge have no first-class ledger column).
func (c *Client) CreateWorkItem(ctx context.Context, wi WorkItem) (string, error) {
	req := &cwbv1.CreateIssueRequest{
		Project:          c.Project,
		Type:             "Task",
		Summary:          summarize(wi),
		Description:      wi.TaskSpec,
		DefinitionOfDone: strings.Join(wi.AcceptanceCriteria, "\n"),
		ParentKey:        wi.StreamID,
		Reporter:         c.Subject,
	}
	if wi.Role != "" {
		req.Skills = []string{wi.Role}
	}
	resp, err := c.issue.CreateIssue(c.ctx(ctx), req)
	if err != nil {
		return "", fmt.Errorf("workgraph.CreateWorkItem: %w", err)
	}
	id := resp.GetIssue().GetKey()

	for _, dep := range wi.DependsOn {
		if _, err := c.issue.AddLink(c.ctx(ctx), &cwbv1.AddLinkRequest{
			Key: dep, ToKey: id, Type: "blocks", Actor: c.Subject,
		}); err != nil {
			return id, fmt.Errorf("workgraph.CreateWorkItem: link depends_on %q -> %q: %w", dep, id, err)
		}
	}

	blob := handoffBlob{
		CairnLine:     wi.CairnLine,
		Artifacts:     wi.Artifacts,
		BaseKnowledge: wi.BaseKnowledge,
		Personality:   wi.Personality,
		Origin:        wi.Origin,
	}
	body, err := blob.marshal()
	if err != nil {
		return id, fmt.Errorf("workgraph.CreateWorkItem: marshal handoff: %w", err)
	}
	if _, err := c.issue.CommentIssue(c.ctx(ctx), &cwbv1.CommentIssueRequest{
		Key: id, Actor: c.Subject, Body: commentTagHandoff + "\n" + body,
	}); err != nil {
		return id, fmt.Errorf("workgraph.CreateWorkItem: comment handoff: %w", err)
	}

	// Carry forward any inherited result history (Rework's rejecting result
	// and earlier) as real cwb:result comments, so a later GetWorkItem folds
	// the same prior_results this item was created with.
	for _, r := range wi.PriorResults {
		if err := c.postResultComment(ctx, id, r); err != nil {
			return id, fmt.Errorf("workgraph.CreateWorkItem: carry forward prior_results: %w", err)
		}
	}
	return id, nil
}

// summarize derives an issue summary from wi when the caller hasn't
// provided one via TaskSpec's first line — kept short for the ledger's
// summary field.
func summarize(wi WorkItem) string {
	line := strings.SplitN(wi.TaskSpec, "\n", 2)[0]
	if len(line) > 120 {
		line = line[:120]
	}
	if line == "" {
		return wi.ID
	}
	return line
}

// GetWorkItem fetches the full work_item state: the ledger issue fields,
// its depends_on edges (ListLinks, incoming blocks), and its folded
// cwb:handoff / cwb:result comments (ListComments).
func (c *Client) GetWorkItem(ctx context.Context, id string) (WorkItem, error) {
	resp, err := c.issue.GetIssue(c.ctx(ctx), &cwbv1.GetIssueRequest{Key: id})
	if err != nil {
		return WorkItem{}, fmt.Errorf("workgraph.GetWorkItem: %w", err)
	}
	issue := resp.GetIssue()

	wi := WorkItem{
		ID:                 issue.GetKey(),
		TaskSpec:           issue.GetDescription(),
		AcceptanceCriteria: splitNonEmpty(issue.GetDefinitionOfDone()),
		StreamID:           issue.GetParentKey(),
		Status:             ledgerToStatus[issue.GetStatus()],
	}
	if skills := issue.GetSkills(); len(skills) > 0 {
		wi.Role = skills[0]
	}

	links, err := c.issue.ListLinks(c.ctx(ctx), &cwbv1.ListLinksRequest{Key: id})
	if err != nil {
		return WorkItem{}, fmt.Errorf("workgraph.GetWorkItem: list links: %w", err)
	}
	for _, l := range links.GetLinks() {
		if l.GetType() == "blocks" && l.GetDirection() == "incoming" {
			wi.DependsOn = append(wi.DependsOn, l.GetFromKey())
		}
	}

	comments, err := c.issue.ListComments(c.ctx(ctx), &cwbv1.ListCommentsRequest{Key: id})
	if err != nil {
		return WorkItem{}, fmt.Errorf("workgraph.GetWorkItem: list comments: %w", err)
	}
	for _, ev := range comments.GetComments() {
		body := commentBody(ev.GetPayload())
		switch {
		case strings.HasPrefix(body, commentTagHandoff+"\n"):
			var h handoffBlob
			if err := json.Unmarshal([]byte(strings.TrimPrefix(body, commentTagHandoff+"\n")), &h); err == nil {
				wi.CairnLine = h.CairnLine
				wi.Artifacts = h.Artifacts
				wi.BaseKnowledge = h.BaseKnowledge
				wi.Personality = h.Personality
				wi.Origin = h.Origin
			}
		case strings.HasPrefix(body, commentTagResult+"\n"):
			var r Result
			if err := json.Unmarshal([]byte(strings.TrimPrefix(body, commentTagResult+"\n")), &r); err == nil {
				wi.PriorResults = append(wi.PriorResults, r)
			}
		}
	}
	return wi, nil
}

// commentBody extracts the comment text from an Event's JSON-encoded
// payload. ledger's CommentIssue stores the actor-supplied body under the
// "body" key of the timeline event payload.
func commentBody(payloadJSON string) string {
	var p struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	return p.Body
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ListReady returns the work items the ledger reports as ready (all
// depends_on satisfied), optionally filtered to one stream (epic subtree).
// Each returned item's Status is forced to StatusReady — that is the
// meaning of ListReadyIssues membership, regardless of the issue's stored
// workflow state (see README.md).
func (c *Client) ListReady(ctx context.Context, stream string) ([]WorkItem, error) {
	resp, err := c.issue.ListReadyIssues(c.ctx(ctx), &cwbv1.ListReadyIssuesRequest{})
	if err != nil {
		return nil, fmt.Errorf("workgraph.ListReady: %w", err)
	}
	var out []WorkItem
	for _, ref := range resp.GetIssues() {
		wi, err := c.GetWorkItem(ctx, ref.GetKey())
		if err != nil {
			return nil, fmt.Errorf("workgraph.ListReady: %w", err)
		}
		if stream != "" && wi.StreamID != stream {
			continue
		}
		wi.Status = StatusReady
		out = append(out, wi)
	}
	return out, nil
}

// Transition moves a work item to status via ledger's TransitionIssue.
// StatusRejected has no ledger mapping — use Rework instead.
func (c *Client) Transition(ctx context.Context, id string, s Status) error {
	ledgerStatus, ok := statusToLedger[s]
	if !ok {
		return fmt.Errorf("workgraph.Transition: %q: %w", s, ErrNoLedgerStatus)
	}
	if _, err := c.issue.TransitionIssue(c.ctx(ctx), &cwbv1.TransitionIssueRequest{
		Key: id, Status: ledgerStatus, Actor: defaultActor,
	}); err != nil {
		return fmt.Errorf("workgraph.Transition: %w", err)
	}
	return nil
}

// RecordResult persists result as a cwb:result comment and, per verdict,
// transitions the issue: done -> Done, blocked -> Blocked. A reject leaves
// the issue's own status untouched — call Rework to create the follow-up
// work item (ledger has no "Rejected" workflow state). A pass likewise
// leaves the issue's status untouched — passing a gate doesn't terminate
// the item, the orchestrator routes it to the next role.
func (c *Client) RecordResult(ctx context.Context, id string, result Result) error {
	if err := c.postResultComment(ctx, id, result); err != nil {
		return fmt.Errorf("workgraph.RecordResult: %w", err)
	}
	switch result.Verdict {
	case VerdictDone:
		return c.Transition(ctx, id, StatusDone)
	case VerdictBlocked:
		return c.Transition(ctx, id, StatusBlocked)
	}
	return nil
}

// postResultComment records result as a cwb:result comment without touching
// the issue's workflow state — the verdict-driven transition (if any) is the
// caller's decision (see RecordResult, Cancel).
func (c *Client) postResultComment(ctx context.Context, id string, result Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	actor := result.Agent
	if actor == "" {
		actor = defaultActor
	}
	if _, err := c.issue.CommentIssue(c.ctx(ctx), &cwbv1.CommentIssueRequest{
		Key: id, Actor: actor, Body: commentTagResult + "\n" + string(body),
	}); err != nil {
		return fmt.Errorf("comment: %w", err)
	}
	return nil
}

// Rework creates a follow-up work item for a rejected one: newSpec is filed
// as a new issue (see CreateWorkItem), a "relates-to" back-edge links it to
// rejectedID, and its prior_results carry the full result history of
// rejectedID (including the rejecting result) unless newSpec already
// supplies its own. Origin is forced to OriginRework.
func (c *Client) Rework(ctx context.Context, rejectedID string, newSpec WorkItem) (string, error) {
	old, err := c.GetWorkItem(ctx, rejectedID)
	if err != nil {
		return "", fmt.Errorf("workgraph.Rework: %w", err)
	}
	item := newSpec
	if item.Role == "" {
		item.Role = old.Role
	}
	if item.StreamID == "" {
		item.StreamID = old.StreamID
	}
	if len(item.PriorResults) == 0 {
		item.PriorResults = old.PriorResults
	}
	item.Origin = OriginRework

	newID, err := c.CreateWorkItem(ctx, item)
	if err != nil {
		return "", fmt.Errorf("workgraph.Rework: %w", err)
	}
	if _, err := c.issue.AddLink(c.ctx(ctx), &cwbv1.AddLinkRequest{
		Key: newID, ToKey: rejectedID, Type: "relates-to", Actor: c.Subject,
	}); err != nil {
		return newID, fmt.Errorf("workgraph.Rework: back-edge link: %w", err)
	}
	return newID, nil
}

// Claim atomically claims a work item for agent, surfacing ErrAlreadyClaimed
// when another agent claimed it first.
func (c *Client) Claim(ctx context.Context, id, agent string) error {
	_, err := c.issue.ClaimIssue(c.ctx(ctx), &cwbv1.ClaimIssueRequest{Key: id, Actor: agent})
	if err != nil {
		if code := status.Code(err); code == codes.FailedPrecondition || code == codes.AlreadyExists {
			return ErrAlreadyClaimed
		}
		return fmt.Errorf("workgraph.Claim: %w", err)
	}
	return nil
}

// Cancel cancels a work item. requeue=true transitions it back to
// StatusQueued and appends reason as a cwb:result comment (verdict blocked,
// so GetWorkItem folds it into prior_results); requeue=false transitions it
// to StatusCancelled and best-effort transitions its direct dependents
// (issues it blocks) to StatusBlocked, since they can never become ready
// now.
func (c *Client) Cancel(ctx context.Context, id string, requeue bool, reason string) error {
	if requeue {
		if err := c.postResultComment(ctx, id, Result{
			WorkItemID: id, Verdict: VerdictBlocked, Reasons: []string{reason},
		}); err != nil {
			return fmt.Errorf("workgraph.Cancel: record requeue reason: %w", err)
		}
		if err := c.Transition(ctx, id, StatusQueued); err != nil {
			return fmt.Errorf("workgraph.Cancel: requeue: %w", err)
		}
		return nil
	}

	if err := c.Transition(ctx, id, StatusCancelled); err != nil {
		return fmt.Errorf("workgraph.Cancel: %w", err)
	}
	links, err := c.issue.ListLinks(c.ctx(ctx), &cwbv1.ListLinksRequest{Key: id})
	if err != nil {
		return fmt.Errorf("workgraph.Cancel: list dependents: %w", err)
	}
	var firstErr error
	for _, l := range links.GetLinks() {
		if l.GetType() != "blocks" || l.GetDirection() != "outgoing" {
			continue
		}
		if err := c.Transition(ctx, l.GetToKey(), StatusBlocked); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("workgraph.Cancel: block dependent %q: %w", l.GetToKey(), err)
		}
	}
	return firstErr
}
