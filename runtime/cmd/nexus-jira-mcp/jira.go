// Cairn-shaped Jira REST client wrapped behind a minimal Go surface
// the MCP tool handlers can call. Basic auth (email + API token) per
// Atlassian Cloud's documented pattern; ADF for descriptions; bog-
// standard net/http throughout.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// jiraClient is the thin layer between MCP handlers and Atlassian REST.
// Stateless; safe for concurrent use because *http.Client is.
type jiraClient struct {
	site       string // hostname only, e.g. "carriedworlduniverse.atlassian.net"
	email      string
	apiToken   string
	projectKey string
	http       *http.Client

	// myAccount caches the result of GET /myself so the auth-self
	// lookup happens once per client. Empty until first call to
	// MyAccountID. Not concurrent-safe by itself; the data race is
	// benign (two concurrent fetches would both succeed and one
	// overwrite the other with the same value).
	myAccount string
}

// newJiraClient builds a client around the given credentials. http is
// optional; nil → http.DefaultClient with a 30s timeout.
func newJiraClient(site, email, apiToken, projectKey string, hc *http.Client) *jiraClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &jiraClient{
		site:       site,
		email:      email,
		apiToken:   apiToken,
		projectKey: projectKey,
		http:       hc,
	}
}

// authHeader returns the Basic-auth header value for this client.
func (c *jiraClient) authHeader() string {
	raw := c.email + ":" + c.apiToken
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

// do issues an HTTP request with auth + JSON headers + body marshalling.
// reqBody is marshalled to JSON if non-nil; respBody (if non-nil) is
// unmarshalled from the response on 2xx.
func (c *jiraClient) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	u := "https://" + c.site + path
	var body io.Reader
	if reqBody != nil {
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("jira: marshal request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("jira: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(buf))
	}
	if respBody == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("jira: decode response: %w", err)
	}
	return nil
}

// adfFromMarkdown wraps a markdown blob in an ADF document with a
// codeBlock node so structure renders monospaced and operators can
// round-trip to the original spec. Cheap and lossless for the MCP
// use case where we'd otherwise need a full markdown-to-ADF parser.
func adfFromMarkdown(md string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{
			{
				"type": "codeBlock",
				"attrs": map[string]any{
					"language": "markdown",
				},
				"content": []map[string]any{
					{"type": "text", "text": md},
				},
			},
		},
	}
}

// adfFromPlain wraps plain text in ADF paragraphs split on newlines.
// Use for comments and short bodies where the user expects normal
// rendering rather than a monospaced block.
func adfFromPlain(text string) map[string]any {
	var nodes []map[string]any
	for _, line := range bytes.Split([]byte(text), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			nodes = append(nodes, map[string]any{"type": "paragraph"})
			continue
		}
		nodes = append(nodes, map[string]any{
			"type": "paragraph",
			"content": []map[string]any{
				{"type": "text", "text": string(line)},
			},
		})
	}
	return map[string]any{"type": "doc", "version": 1, "content": nodes}
}

// --- Domain shapes used by the MCP tool handlers ------------------

// IssueRef is the minimal projection of an issue we surface to MCP
// callers: key, summary, status, issuetype, assignee.
type IssueRef struct {
	Key       string `json:"key"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	IssueType string `json:"issue_type"`
	Assignee  string `json:"assignee,omitempty"`
}

// Issue is the richer projection with body + components + labels.
type Issue struct {
	IssueRef
	Description string   `json:"description"` // rendered as plain text (best-effort from ADF)
	Components  []string `json:"components,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	Created     string   `json:"created,omitempty"`
	Updated     string   `json:"updated,omitempty"`
}

// rawIssue mirrors the fields we read off Atlassian's REST response.
type rawIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description any    `json:"description"` // ADF doc or null
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		Labels []string `json:"labels"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent"`
		Created string `json:"created"`
		Updated string `json:"updated"`
	} `json:"fields"`
}

func (r *rawIssue) toIssue() Issue {
	out := Issue{
		IssueRef: IssueRef{
			Key:       r.Key,
			Summary:   r.Fields.Summary,
			Status:    r.Fields.Status.Name,
			IssueType: r.Fields.IssueType.Name,
		},
		Labels:  append([]string(nil), r.Fields.Labels...),
		Created: r.Fields.Created,
		Updated: r.Fields.Updated,
	}
	if r.Fields.Assignee != nil {
		out.Assignee = r.Fields.Assignee.DisplayName
	}
	for _, c := range r.Fields.Components {
		out.Components = append(out.Components, c.Name)
	}
	if r.Fields.Parent != nil {
		out.Parent = r.Fields.Parent.Key
	}
	out.Description = adfToPlain(r.Fields.Description)
	return out
}

// adfToPlain extracts plain text from an ADF document. Best-effort:
// walks the tree collecting text nodes, ignores formatting. Returns
// empty string for null/empty docs.
func adfToPlain(v any) string {
	if v == nil {
		return ""
	}
	var sb bytes.Buffer
	walkADF(v, &sb)
	return sb.String()
}

func walkADF(v any, sb *bytes.Buffer) {
	switch n := v.(type) {
	case map[string]any:
		if t, _ := n["type"].(string); t == "text" {
			if s, _ := n["text"].(string); s != "" {
				sb.WriteString(s)
			}
		}
		if c, ok := n["content"].([]any); ok {
			for _, child := range c {
				walkADF(child, sb)
			}
			// Insert a newline after paragraph/heading-like blocks for
			// readability when we collapse ADF to plain text.
			if t, _ := n["type"].(string); t == "paragraph" || t == "heading" || t == "codeBlock" {
				sb.WriteString("\n")
			}
		}
	case []any:
		for _, child := range n {
			walkADF(child, sb)
		}
	}
}

// --- High-level operations ----------------------------------------

// Search runs a JQL search and returns the matched issues (lightweight
// projection). maxResults is clamped to [1, 100]; 0 means default 50.
func (c *jiraClient) Search(ctx context.Context, jql string, maxResults int) ([]IssueRef, error) {
	if maxResults <= 0 {
		maxResults = 50
	}
	if maxResults > 100 {
		maxResults = 100
	}
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", "summary,status,issuetype,assignee")
	q.Set("maxResults", fmt.Sprintf("%d", maxResults))
	var resp struct {
		Issues []rawIssue `json:"issues"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/search/jql?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]IssueRef, 0, len(resp.Issues))
	for i := range resp.Issues {
		out = append(out, resp.Issues[i].toIssue().IssueRef)
	}
	return out, nil
}

// Get returns the full issue projection for a single key.
func (c *jiraClient) Get(ctx context.Context, key string) (Issue, error) {
	var raw rawIssue
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key), nil, &raw); err != nil {
		return Issue{}, err
	}
	return raw.toIssue(), nil
}

// Comment posts a plain-text comment on the issue.
func (c *jiraClient) Comment(ctx context.Context, key, body string) error {
	payload := map[string]any{"body": adfFromPlain(body)}
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", payload, nil)
}

// TransitionTo moves the issue to a status by name (case-sensitive
// Atlassian status name). Optional comment is posted before the
// transition fires so the audit history threads correctly.
func (c *jiraClient) TransitionTo(ctx context.Context, key, target, comment string) error {
	if comment != "" {
		if err := c.Comment(ctx, key, comment); err != nil {
			return fmt.Errorf("pre-transition comment: %w", err)
		}
	}
	var resp struct {
		Transitions []struct {
			ID string `json:"id"`
			To struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", nil, &resp); err != nil {
		return err
	}
	for _, t := range resp.Transitions {
		if t.To.Name == target {
			return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", map[string]any{"transition": map[string]string{"id": t.ID}}, nil)
		}
	}
	avail := make([]string, 0, len(resp.Transitions))
	for _, t := range resp.Transitions {
		avail = append(avail, t.To.Name)
	}
	return fmt.Errorf("no transition to %q from current state; available: %v", target, avail)
}

// Assign sets the assignee on an issue. accountID empty → unassigns.
func (c *jiraClient) Assign(ctx context.Context, key, accountID string) error {
	payload := map[string]any{"accountId": accountID}
	if accountID == "" {
		payload["accountId"] = nil
	}
	return c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(key)+"/assignee", payload, nil)
}

// MyAccountID returns the accountId of the authenticated user.
// Cached locally so the lookup happens once per client lifetime.
func (c *jiraClient) MyAccountID(ctx context.Context) (string, error) {
	if c.myAccount != "" {
		return c.myAccount, nil
	}
	var resp struct {
		AccountID string `json:"accountId"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/myself", nil, &resp); err != nil {
		return "", err
	}
	c.myAccount = resp.AccountID
	return resp.AccountID, nil
}

// CreateIssue files a new issue. issueType is the Atlassian-name
// ("Epic" | "Story" | "Task" | "Subtask" | "Bug" ...). parentKey
// empty → no parent (orphan Task or top-level Epic). component
// empty → no component tag. labels nil/empty → no labels.
func (c *jiraClient) CreateIssue(ctx context.Context, summary, descriptionMarkdown, issueType, parentKey, component string, labels []string) (string, error) {
	project := c.projectKey
	if project == "" {
		return "", errors.New("jira: project_key not configured in keyfile and not supplied")
	}
	fields := map[string]any{
		"project":   map[string]string{"key": project},
		"summary":   summary,
		"issuetype": map[string]string{"name": issueType},
	}
	if descriptionMarkdown != "" {
		fields["description"] = adfFromMarkdown(descriptionMarkdown)
	}
	if parentKey != "" {
		fields["parent"] = map[string]string{"key": parentKey}
	}
	if component != "" {
		fields["components"] = []map[string]string{{"name": component}}
	}
	if len(labels) > 0 {
		fields["labels"] = labels
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/rest/api/3/issue", map[string]any{"fields": fields}, &resp); err != nil {
		return "", err
	}
	return resp.Key, nil
}
