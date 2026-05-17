// GitHub REST API client. Calls authenticate using the keyfile-supplied
// classic PAT via HTTPS Basic auth (username:PAT). API responses are
// returned as decoded structs for the typed tools, and as raw
// json.RawMessage for the github.api escape hatch.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

type client struct {
	username   string
	pat        string
	defaultOrg string
	http       *http.Client
	log        *slog.Logger
}

func newClient(username, pat, defaultOrg string, log *slog.Logger) *client {
	return &client{
		username:   username,
		pat:        pat,
		defaultOrg: defaultOrg,
		http:       &http.Client{Timeout: 30 * time.Second},
		log:        log,
	}
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	u := apiBase + path
	if !strings.HasPrefix(path, "/") {
		u = apiBase + "/" + path
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return raw, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(raw))
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return json.RawMessage(raw), nil
}

// User is the projection of GET /user.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

func (c *client) WhoAmI(ctx context.Context) (*User, error) {
	var u User
	if _, err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// PR is the projection of /repos/{owner}/{repo}/pulls/{number}.
type PR struct {
	Number    int    `json:"number"`
	URL       string `json:"html_url"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Draft     bool   `json:"draft"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Head      Ref    `json:"head"`
	Base      Ref    `json:"base"`
	User      User   `json:"user"`
	Mergeable *bool  `json:"mergeable"`
}

type Ref struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
}

type PRCreateInput struct {
	Owner string `json:"-"`
	Repo  string `json:"-"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft,omitempty"`
}

func (c *client) CreatePR(ctx context.Context, in PRCreateInput) (*PR, error) {
	if in.Owner == "" || in.Repo == "" {
		return nil, fmt.Errorf("CreatePR: owner and repo required")
	}
	if in.Title == "" || in.Head == "" || in.Base == "" {
		return nil, fmt.Errorf("CreatePR: title, head, base required")
	}
	var p PR
	path := fmt.Sprintf("/repos/%s/%s/pulls", in.Owner, in.Repo)
	if _, err := c.do(ctx, http.MethodPost, path, in, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *client) GetPR(ctx context.Context, owner, repo string, number int) (*PR, error) {
	var p PR
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type ListPRsInput struct {
	Owner string
	Repo  string
	State string
	Head  string
	Base  string
	Sort  string
	Limit int
}

func (c *client) ListPRs(ctx context.Context, in ListPRsInput) ([]PR, error) {
	q := url.Values{}
	if in.State != "" {
		q.Set("state", in.State)
	}
	if in.Head != "" {
		q.Set("head", in.Head)
	}
	if in.Base != "" {
		q.Set("base", in.Base)
	}
	if in.Sort != "" {
		q.Set("sort", in.Sort)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q.Set("per_page", fmt.Sprintf("%d", limit))

	var prs []PR
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s", in.Owner, in.Repo, q.Encode())
	if _, err := c.do(ctx, http.MethodGet, path, nil, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

type MergePRInput struct {
	Owner         string `json:"-"`
	Repo          string `json:"-"`
	Number        int    `json:"-"`
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
}

func (c *client) MergePR(ctx context.Context, in MergePRInput) (json.RawMessage, error) {
	if in.MergeMethod == "" {
		in.MergeMethod = "squash"
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", in.Owner, in.Repo, in.Number)
	return c.do(ctx, http.MethodPut, path, in, nil)
}

func (c *client) PRChecks(ctx context.Context, owner, repo string, number int) (json.RawMessage, error) {
	pr, err := c.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, pr.Head.SHA)
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

func (c *client) PRDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", apiBase, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.username, c.pat)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("PRDiff: %d %s", resp.StatusCode, string(b))
	}
	return string(b), nil
}

type Issue struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	User   User   `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type IssueCreateInput struct {
	Owner     string   `json:"-"`
	Repo      string   `json:"-"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

func (c *client) CreateIssueOnRepo(ctx context.Context, in IssueCreateInput) (*Issue, error) {
	if in.Owner == "" || in.Repo == "" || in.Title == "" {
		return nil, fmt.Errorf("CreateIssue: owner, repo, title required")
	}
	var i Issue
	path := fmt.Sprintf("/repos/%s/%s/issues", in.Owner, in.Repo)
	if _, err := c.do(ctx, http.MethodPost, path, in, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

func (c *client) GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	var i Issue
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

type ListIssuesInput struct {
	Owner  string
	Repo   string
	State  string
	Labels string
	Limit  int
}

func (c *client) ListIssues(ctx context.Context, in ListIssuesInput) ([]Issue, error) {
	q := url.Values{}
	if in.State != "" {
		q.Set("state", in.State)
	}
	if in.Labels != "" {
		q.Set("labels", in.Labels)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q.Set("per_page", fmt.Sprintf("%d", limit))

	var raw []map[string]any
	path := fmt.Sprintf("/repos/%s/%s/issues?%s", in.Owner, in.Repo, q.Encode())
	if _, err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if _, isPR := r["pull_request"]; isPR {
			continue
		}
		b, _ := json.Marshal(r)
		var i Issue
		if err := json.Unmarshal(b, &i); err != nil {
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

func (c *client) GetWorkflowRun(ctx context.Context, owner, repo string, runID int64) (json.RawMessage, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, runID)
	return c.do(ctx, http.MethodGet, path, nil, nil)
}
