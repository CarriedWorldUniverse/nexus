package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxExcerptLines = 8
	defaultMaxExcerptBytes = 2000
)

type AlertmanagerWebhook struct {
	Status       string            `json:"status"`
	Receiver     string            `json:"receiver"`
	GroupLabels  map[string]string `json:"groupLabels"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []WebhookAlert    `json:"alerts"`
}

type WebhookAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

type AlertContext struct {
	Alert     WebhookAlert
	Namespace string
	Workload  string
	Query     string
	Start     time.Time
	End       time.Time
	Limit     int
}

type LokiQuerier interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, limit int) ([]string, error)
}

type ChatPoster interface {
	PostChat(ctx context.Context, content string) error
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Service struct {
	Loki      LokiQuerier
	Chat      ChatPoster
	Dedup     *Deduper
	Clock     Clock
	Log       *slog.Logger
	Window    time.Duration
	LineLimit int
	ByteLimit int
	Mention   string
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var payload AlertmanagerWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "decode alertmanager webhook", http.StatusBadRequest)
		return
	}
	if s.Clock == nil {
		s.Clock = realClock{}
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	now := s.Clock.Now()
	processed := 0
	for _, alert := range firingAlerts(payload) {
		ctx := BuildAlertContext(alert, now, s.Window, s.LineLimit)
		if ctx.Query == "" {
			s.Log.Warn("loki-alert-bridge: alert has no usable Loki labels", "alertname", label(alert.Labels, "alertname"))
			continue
		}
		if s.Dedup != nil && s.Dedup.Seen(ctx.Alert, now) {
			continue
		}
		lines, err := s.Loki.QueryRange(r.Context(), ctx.Query, ctx.Start, ctx.End, ctx.Limit)
		if err != nil {
			s.Log.Warn("loki-alert-bridge: Loki query failed", "query", ctx.Query, "err", err)
			lines = nil
		}
		msg := FormatMessage(ctx, lines, FormatOptions{
			Mention:      s.Mention,
			MaxLines:     s.LineLimit,
			MaxBytes:     s.ByteLimit,
			LokiQueryURL: ctx.Alert.GeneratorURL,
		})
		if err := s.Chat.PostChat(r.Context(), msg); err != nil {
			http.Error(w, "post chat", http.StatusBadGateway)
			return
		}
		processed++
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"processed":%d}`+"\n", processed)
}

func firingAlerts(payload AlertmanagerWebhook) []WebhookAlert {
	out := make([]WebhookAlert, 0, len(payload.Alerts))
	for _, a := range payload.Alerts {
		if strings.EqualFold(a.Status, "resolved") {
			continue
		}
		if a.Status == "" && strings.EqualFold(payload.Status, "resolved") {
			continue
		}
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		out = append(out, a)
	}
	return out
}

func BuildAlertContext(alert WebhookAlert, now time.Time, window time.Duration, limit int) AlertContext {
	if window <= 0 {
		window = 10 * time.Minute
	}
	if limit <= 0 {
		limit = defaultMaxExcerptLines
	}
	end := now
	if !alert.StartsAt.IsZero() && alert.StartsAt.After(now.Add(-window)) && !alert.StartsAt.After(now) {
		end = now
	}
	namespace := label(alert.Labels, "namespace")
	workload := workloadName(alert.Labels)
	query := BuildLokiQuery(alert.Labels)
	return AlertContext{
		Alert:     alert,
		Namespace: namespace,
		Workload:  workload,
		Query:     query,
		Start:     end.Add(-window),
		End:       end,
		Limit:     limit,
	}
}

func BuildLokiQuery(labels map[string]string) string {
	selector := make(map[string]string)
	if ns := label(labels, "namespace"); ns != "" {
		selector["namespace"] = ns
	}
	for _, key := range []string{"pod", "workload", "container", "app", "job"} {
		if v := label(labels, key); v != "" {
			selector[key] = v
			break
		}
	}
	if len(selector) == 0 {
		return ""
	}
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s=%q`, k, selector[k])
	}
	b.WriteString(`} |~ "(?i)(error|exception|panic|fail|fatal)"`)
	return b.String()
}

type FormatOptions struct {
	Mention      string
	MaxLines     int
	MaxBytes     int
	LokiQueryURL string
}

func FormatMessage(ctx AlertContext, lines []string, opts FormatOptions) string {
	mention := opts.Mention
	if mention == "" {
		mention = "keel"
	}
	if !strings.HasPrefix(mention, "@") {
		mention = "@" + mention
	}
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = defaultMaxExcerptLines
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxExcerptBytes
	}
	alertName := label(ctx.Alert.Labels, "alertname")
	if alertName == "" {
		alertName = "LokiLogAlert"
	}
	namespace := ctx.Namespace
	if namespace == "" {
		namespace = "unknown"
	}
	workload := ctx.Workload
	if workload == "" {
		workload = "unknown"
	}
	count := alertCount(ctx.Alert)
	summary := annotationSummary(ctx.Alert)
	excerpt := boundExcerpt(lines, maxLines, maxBytes)
	if excerpt == "" {
		excerpt = "(no recent matching log lines returned)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s Loki alert firing: %s (%s)\n", mention, alertName, count)
	fmt.Fprintf(&b, "namespace=%s workload=%s", namespace, workload)
	if severity := label(ctx.Alert.Labels, "severity"); severity != "" {
		fmt.Fprintf(&b, " severity=%s", severity)
	}
	if summary != "" {
		fmt.Fprintf(&b, "\nsummary: %s", summary)
	}
	fmt.Fprintf(&b, "\nlogs:\n```text\n%s\n```", excerpt)
	fmt.Fprintf(&b, "\nquery: `%s`", ctx.Query)
	return b.String()
}

func boundExcerpt(lines []string, maxLines, maxBytes int) string {
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	joined := strings.Join(lines, "\n")
	if maxBytes > 0 && len(joined) > maxBytes {
		joined = joined[:maxBytes]
		if i := strings.LastIndexByte(joined, '\n'); i > maxBytes/2 {
			joined = joined[:i]
		}
		joined += "\n...(truncated)"
	}
	return joined
}

func alertCount(alert WebhookAlert) string {
	for _, key := range []string{"count", "value"} {
		if v := label(alert.Labels, key); v != "" {
			return "count=" + v
		}
		if v := label(alert.Annotations, key); v != "" {
			return "count=" + v
		}
	}
	return "count=1"
}

func annotationSummary(alert WebhookAlert) string {
	for _, key := range []string{"summary", "description", "message"} {
		if v := strings.TrimSpace(label(alert.Annotations, key)); v != "" {
			return singleLine(v, 240)
		}
	}
	return ""
}

func singleLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		return strings.TrimSpace(s[:max]) + "..."
	}
	return s
}

func workloadName(labels map[string]string) string {
	for _, key := range []string{"workload", "pod", "container", "app", "job"} {
		if v := label(labels, key); v != "" {
			return v
		}
	}
	return ""
}

func label(labels map[string]string, key string) string {
	if labels == nil {
		return ""
	}
	if v := strings.TrimSpace(labels[key]); v != "" {
		return v
	}
	normalized := strings.ReplaceAll(key, "_", ".")
	for k, v := range labels {
		lk := strings.ToLower(strings.ReplaceAll(k, "_", "."))
		if lk == normalized {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type Deduper struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[string]time.Time
}

func NewDeduper(window time.Duration) *Deduper {
	return &Deduper{window: window, seen: make(map[string]time.Time)}
}

func (d *Deduper) Seen(alert WebhookAlert, now time.Time) bool {
	if d == nil || d.window <= 0 {
		return false
	}
	key := dedupKey(alert)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.seen {
		if now.Sub(t) > d.window {
			delete(d.seen, k)
		}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) <= d.window {
		return true
	}
	d.seen[key] = now
	return false
}

func dedupKey(alert WebhookAlert) string {
	if alert.Fingerprint != "" {
		return alert.Fingerprint
	}
	keys := make([]string, 0, len(alert.Labels))
	for k := range alert.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = io.WriteString(h, k)
		_, _ = io.WriteString(h, "=")
		_, _ = io.WriteString(h, alert.Labels[k])
		_, _ = io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

var errMissingDependency = errors.New("loki-alert-bridge: missing service dependency")

func (s *Service) Validate() error {
	if s.Loki == nil || s.Chat == nil {
		return errMissingDependency
	}
	return nil
}
