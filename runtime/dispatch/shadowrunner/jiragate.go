package shadowrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// JiraGate is the cost gate for the heartbeat drain: a cheap JQL probe of
// shadow's actionable queue so an (expensive, Opus) claude drain only spins when
// there is actually work. It reuses the Atlassian REST shape the nexus triage
// client uses (Basic auth, /rest/api/3/search/jql).
type JiraGate struct {
	baseURL  string // e.g. https://carriedworlduniverse.atlassian.net
	email    string
	apiToken string
	jql      string
	http     *http.Client
}

// NewJiraGate builds a gate from the broker `jira` credential bundle
// (atlassian_subdomain/email/token) + the JQL defining shadow's queue.
func NewJiraGate(subdomain, email, apiToken, jql string) *JiraGate {
	return &JiraGate{
		baseURL:  "https://" + subdomain + ".atlassian.net",
		email:    email,
		apiToken: apiToken,
		jql:      jql,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// DefaultQueueJQL is shadow's actionable orchestration queue: issues in `project`
// carrying `label`, that are neither Done nor Blocked. Blocked = escalated and
// waiting on the operator, so waking for them would burn frontier cost for no
// progress; Done has nothing left to do.
func DefaultQueueJQL(project, label string) string {
	return fmt.Sprintf(`project = %s AND labels = %q AND statusCategory != Done AND status != "Blocked"`, project, label)
}

// HasWork reports whether shadow's queue has at least one actionable issue. A
// false return is the gate firing closed (skip the drain).
func (g *JiraGate) HasWork(ctx context.Context) (bool, error) {
	q := url.Values{}
	q.Set("jql", g.jql)
	q.Set("fields", "key")
	q.Set("maxResults", "1")
	u := strings.TrimRight(g.baseURL, "/") + "/rest/api/3/search/jql?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("jiragate: build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(g.email+":"+g.apiToken)))
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("jiragate: search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("jiragate: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	var out struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("jiragate: decode: %w", err)
	}
	return len(out.Issues) > 0, nil
}
