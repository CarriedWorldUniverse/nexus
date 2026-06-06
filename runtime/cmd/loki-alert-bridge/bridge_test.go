package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFiringAlertsBatchAndMissingLabels(t *testing.T) {
	payload := AlertmanagerWebhook{
		Status: "firing",
		Alerts: []WebhookAlert{
			{
				Status: "firing",
				Labels: map[string]string{"alertname": "ErrorsHigh", "namespace": "prod", "pod": "api-123"},
			},
			{
				Status: "resolved",
				Labels: map[string]string{"alertname": "Resolved"},
			},
			{
				Labels: map[string]string{"alertname": "NoStatus"},
			},
			{
				Status: "firing",
			},
		},
	}
	got := firingAlerts(payload)
	if len(got) != 3 {
		t.Fatalf("firingAlerts len=%d, want 3", len(got))
	}
	if got[2].Labels == nil || got[2].Annotations == nil {
		t.Fatalf("missing labels/annotations should be normalized: %+v", got[2])
	}
}

func TestBuildLokiQueryFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "namespace and pod",
			labels: map[string]string{"namespace": "prod", "pod": "api-abc", "workload": "api"},
			want:   `{namespace="prod",pod="api-abc"} |~ "(?i)(error|exception|panic|fail|fatal)"`,
		},
		{
			name:   "namespace and workload",
			labels: map[string]string{"namespace": "prod", "workload": "worker"},
			want:   `{namespace="prod",workload="worker"} |~ "(?i)(error|exception|panic|fail|fatal)"`,
		},
		{
			name:   "missing workload still scopes namespace",
			labels: map[string]string{"namespace": "prod", "alertname": "ErrorsHigh"},
			want:   `{namespace="prod"} |~ "(?i)(error|exception|panic|fail|fatal)"`,
		},
		{
			name:   "no usable labels",
			labels: map[string]string{"alertname": "ErrorsHigh"},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildLokiQuery(tt.labels); got != tt.want {
				t.Fatalf("BuildLokiQuery()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatMessageBoundsAndContent(t *testing.T) {
	ctx := AlertContext{
		Namespace: "prod",
		Workload:  "api-123",
		Query:     `{namespace="prod",pod="api-123"}`,
		Alert: WebhookAlert{
			Labels:      map[string]string{"alertname": "ErrorsHigh", "severity": "critical", "count": "12"},
			Annotations: map[string]string{"summary": "API returned errors\nacross requests"},
		},
	}
	lines := []string{"one", "two", "three", strings.Repeat("x", 80)}
	got := FormatMessage(ctx, lines, FormatOptions{Mention: "keel", MaxLines: 2, MaxBytes: 30})
	for _, want := range []string{
		"@keel Loki alert firing: ErrorsHigh (count=12)",
		"namespace=prod workload=api-123 severity=critical",
		"summary: API returned errors across requests",
		"query: `{namespace=\"prod\",pod=\"api-123\"}`",
		"...(truncated)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "one") || strings.Contains(got, "two") {
		t.Fatalf("message should keep only bounded tail lines:\n%s", got)
	}
}

func TestDeduperWindow(t *testing.T) {
	d := NewDeduper(5 * time.Minute)
	alert := WebhookAlert{Fingerprint: "abc", Labels: map[string]string{"alertname": "ErrorsHigh"}}
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	if d.Seen(alert, now) {
		t.Fatal("first alert should not dedup")
	}
	if !d.Seen(alert, now.Add(time.Minute)) {
		t.Fatal("repeat inside window should dedup")
	}
	if d.Seen(alert, now.Add(6*time.Minute)) {
		t.Fatal("repeat outside window should not dedup")
	}
}

func TestServicePostsOneMessagePerFiringAlertAndDedups(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	service := &Service{
		Loki:      &fakeLoki{lines: []string{"error line"}},
		Chat:      &fakeChat{},
		Dedup:     NewDeduper(10 * time.Minute),
		Clock:     fixedClock{t: now},
		Window:    5 * time.Minute,
		LineLimit: 3,
		ByteLimit: 1000,
		Mention:   "keel",
	}
	payload := AlertmanagerWebhook{Status: "firing", Alerts: []WebhookAlert{
		{Status: "firing", Fingerprint: "a", Labels: map[string]string{"alertname": "ErrorsHigh", "namespace": "prod", "pod": "api-1"}},
		{Status: "resolved", Fingerprint: "b", Labels: map[string]string{"alertname": "Resolved", "namespace": "prod", "pod": "api-2"}},
		{Status: "firing", Fingerprint: "a", Labels: map[string]string{"alertname": "ErrorsHigh", "namespace": "prod", "pod": "api-1"}},
	}}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/alertmanager", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	chat := service.Chat.(*fakeChat)
	if len(chat.posts) != 1 {
		t.Fatalf("posts=%d, want 1: %#v", len(chat.posts), chat.posts)
	}
	loki := service.Loki.(*fakeLoki)
	if len(loki.queries) != 1 {
		t.Fatalf("queries=%d, want 1", len(loki.queries))
	}
	if loki.queries[0] != `{namespace="prod",pod="api-1"} |~ "(?i)(error|exception|panic|fail|fatal)"` {
		t.Fatalf("query=%q", loki.queries[0])
	}
}

type fixedClock struct {
	t time.Time
}

func (c fixedClock) Now() time.Time { return c.t }

type fakeLoki struct {
	lines   []string
	queries []string
}

func (f *fakeLoki) QueryRange(_ context.Context, query string, _, _ time.Time, _ int) ([]string, error) {
	f.queries = append(f.queries, query)
	return f.lines, nil
}

type fakeChat struct {
	posts []string
}

func (f *fakeChat) PostChat(_ context.Context, content string) error {
	f.posts = append(f.posts, content)
	return nil
}
