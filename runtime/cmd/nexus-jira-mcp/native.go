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
)

// nativeClient mirrors Jira writes to the native issue tracker over HTTPS.
// Non-fatal on error: Jira is authoritative.
type nativeClient struct {
	base   string
	jwt    string
	aspect string
	http   *http.Client
	log    *slog.Logger
}

func (n *nativeClient) enabled() bool { return n != nil && n.base != "" }

func (n *nativeClient) MirrorCreate(ctx context.Context, body map[string]any) {
	if !n.enabled() {
		return
	}
	if err := n.do(ctx, http.MethodPost, "/api/issues", body, nil); err != nil {
		n.log.Warn("dual-write create failed", "err", err)
	}
}

func (n *nativeClient) MirrorTransition(ctx context.Context, key, status, actor string) {
	if !n.enabled() {
		return
	}
	body := map[string]any{"status": status, "actor": actor}
	if err := n.do(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(key)+"/transition", body, nil); err != nil {
		n.log.Warn("dual-write transition failed", "err", err, "key", key)
	}
}

func (n *nativeClient) MirrorAssign(ctx context.Context, key, aspect, actor string) {
	if !n.enabled() {
		return
	}
	body := map[string]any{"aspect": aspect, "actor": actor}
	if err := n.do(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(key)+"/assign", body, nil); err != nil {
		n.log.Warn("dual-write assign failed", "err", err, "key", key)
	}
}

func (n *nativeClient) do(ctx context.Context, method, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, method, n.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.jwt)
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d: %s", resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// translateJiraCreate maps the Jira create payload to the native shape.
// If dod is empty, a placeholder is used (native requires definition_of_done).
func translateJiraCreate(project, typ, summary, description, reporter, dod string) map[string]any {
	if dod == "" {
		dod = "- [ ] _carried_from_jira_"
	}
	return map[string]any{
		"project":            project,
		"type":               typ,
		"summary":            summary,
		"description":        description,
		"definition_of_done": dod,
		"reporter":           reporter,
	}
}
