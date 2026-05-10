package rewriter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubDistiller records calls + returns a deterministic short-form. The
// tests assert behavior on this stub; Part 2's real haiku-backed
// distiller is verified separately against live sessions.
type stubDistiller struct {
	toolCalls      int
	textCalls      int
	failNext       bool
	failNextErr    error
	suffix         string // appended to the input so tests can confirm the rewrite landed
	identityReturn bool   // when true, return input unchanged (covers the "distiller said no" path)
}

func (s *stubDistiller) DistillToolResult(_ context.Context, _, content string) (string, error) {
	s.toolCalls++
	if s.failNext {
		s.failNext = false
		err := s.failNextErr
		if err == nil {
			err = errors.New("stub: forced failure")
		}
		return content, err
	}
	if s.identityReturn {
		return content, nil
	}
	return "[distilled] " + s.suffix, nil
}

func (s *stubDistiller) DistillAssistantText(_ context.Context, content string) (string, error) {
	s.textCalls++
	if s.failNext {
		s.failNext = false
		err := s.failNextErr
		if err == nil {
			err = errors.New("stub: forced failure")
		}
		return content, err
	}
	if s.identityReturn {
		return content, nil
	}
	return "[distilled-text] " + s.suffix, nil
}

// quietLogger discards everything — tests don't assert on log output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeJSONL builds a session jsonl from a list of map records, in
// order. Records are encoded with json.Marshal; tests pass map[string]any
// for readability.
func writeJSONL(t *testing.T, records []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

// readJSONL reads a session jsonl back into a slice of decoded records
// for assertion. Skips parse failures with t.Fatal — tests should be
// catching parse bugs.
func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func longString(n int) string {
	return strings.Repeat("x", n)
}

func TestNew_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing path", Config{Distiller: &stubDistiller{}, Logger: quietLogger()}},
		{"missing distiller", Config{SessionPath: "x", Logger: quietLogger()}},
		{"missing logger", Config{SessionPath: "x", Distiller: &stubDistiller{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestDistillTurn_NoBoundary(t *testing.T) {
	path := writeJSONL(t, []map[string]any{
		{"type": "user", "uuid": "u1", "message": map[string]any{"content": []any{}}},
	})
	rw, err := New(Config{SessionPath: path, Distiller: &stubDistiller{suffix: "x"}, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.DistillTurn(context.Background(), ""); !errors.Is(err, ErrNoBoundary) {
		t.Errorf("empty boundary uuid: got %v want ErrNoBoundary", err)
	}
	if _, err := rw.DistillTurn(context.Background(), "does-not-exist"); !errors.Is(err, ErrNoBoundary) {
		t.Errorf("missing boundary uuid: got %v want ErrNoBoundary", err)
	}
}

func TestDistillTurn_FirstTurn_AssistantTextDistilled(t *testing.T) {
	// First turn ever — no prior _nexus_distilled marker, distill from
	// the start of the session.
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": long},
				},
			},
		},
	})
	d := &stubDistiller{suffix: "first"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
		ModelName:              "stub-model",
	})

	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		t.Fatalf("DistillTurn: %v", err)
	}
	if stats.RecordsRewritten != 1 {
		t.Errorf("RecordsRewritten = %d, want 1", stats.RecordsRewritten)
	}
	if d.textCalls != 1 {
		t.Errorf("textCalls = %d, want 1", d.textCalls)
	}

	out := readJSONL(t, path)
	rec := out[0]
	msg := rec["message"].(map[string]any)
	content := msg["content"].([]any)
	textBlock := content[0].(map[string]any)
	if got := textBlock["text"].(string); !strings.HasPrefix(got, "[distilled-text]") {
		t.Errorf("text not distilled, got %q", got)
	}
	if _, ok := rec[MarkerKey]; !ok {
		t.Errorf("expected %s marker on rewritten record", MarkerKey)
	}
}

func TestDistillTurn_BelowThreshold_PassThrough(t *testing.T) {
	short := "small content"
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": short}},
			},
		},
	})
	d := &stubDistiller{suffix: "x"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 500,
	})

	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 0 {
		t.Errorf("RecordsRewritten = %d, want 0", stats.RecordsRewritten)
	}
	if d.textCalls != 0 {
		t.Errorf("distiller called for sub-threshold content: %d", d.textCalls)
	}
	out := readJSONL(t, path)
	if _, ok := out[0][MarkerKey]; ok {
		t.Errorf("marker stamped on unchanged record — should only stamp when something changed")
	}
}

func TestDistillTurn_Idempotence_SkipsMarked(t *testing.T) {
	long := longString(800)
	// Pre-stamp the record so it should be skipped.
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": long}},
			},
			MarkerKey: map[string]any{
				"at":            "2026-05-10T12:00:00Z",
				"model":         "previous-pass",
				"originalBytes": 4321,
			},
		},
	})
	d := &stubDistiller{suffix: "second-pass"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})
	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsSkipped != 1 {
		t.Errorf("RecordsSkipped = %d, want 1", stats.RecordsSkipped)
	}
	if stats.RecordsRewritten != 0 {
		t.Errorf("RecordsRewritten = %d, want 0", stats.RecordsRewritten)
	}
	if d.textCalls != 0 {
		t.Errorf("distiller called on already-marked record: %d", d.textCalls)
	}

	// File should be byte-identical (no-op write avoidance).
	out := readJSONL(t, path)
	textBlock := out[0]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if got := textBlock["text"].(string); got != long {
		t.Errorf("marked record was modified: %q", got[:50])
	}
}

func TestDistillTurn_ScopeRespectsPriorMarker(t *testing.T) {
	// Older record carries marker → not in scope.
	// Middle record (after marker) → in scope, distilled.
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type":    "assistant",
			"uuid":    "old",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}},
			MarkerKey: map[string]any{"at": "2026-05-10T11:00:00Z", "model": "prior"},
		},
		{
			"type":    "assistant",
			"uuid":    "new",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}},
		},
	})
	d := &stubDistiller{suffix: "fresh"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})
	stats, err := rw.DistillTurn(context.Background(), "new")
	if err != nil {
		t.Fatal(err)
	}
	// old is OUT of scope (above the prior marker). new is the scope.
	if stats.RecordsScanned != 1 {
		t.Errorf("RecordsScanned = %d, want 1 (only post-marker scope)", stats.RecordsScanned)
	}
	if stats.RecordsRewritten != 1 {
		t.Errorf("RecordsRewritten = %d, want 1", stats.RecordsRewritten)
	}
	out := readJSONL(t, path)
	// old must be byte-identical: still has its marker, content unchanged.
	if oldText := out[0]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); oldText != long {
		t.Errorf("out-of-scope record was modified")
	}
}

func TestDistillTurn_ToolResultArrayShape(t *testing.T) {
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":         "tool_result",
						"tool_use_id":  "tu_1",
						"is_error":     false,
						"content": []any{
							map[string]any{"type": "text", "text": long},
						},
					},
				},
			},
		},
	})
	d := &stubDistiller{suffix: "tr"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})

	stats, err := rw.DistillTurn(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 1 || d.toolCalls != 1 {
		t.Errorf("expected 1 tool distillation: stats=%+v calls=%d", stats, d.toolCalls)
	}

	out := readJSONL(t, path)
	tr := out[0]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	// Structural fields preserved.
	if tr["tool_use_id"] != "tu_1" {
		t.Errorf("tool_use_id stripped/changed: %v", tr["tool_use_id"])
	}
	if tr["is_error"] != false {
		t.Errorf("is_error stripped/changed: %v", tr["is_error"])
	}
	// Content distilled.
	textBlock := tr["content"].([]any)[0].(map[string]any)
	if got := textBlock["text"].(string); !strings.HasPrefix(got, "[distilled]") {
		t.Errorf("tool_result text not distilled: %q", got)
	}
}

func TestDistillTurn_ToolResultStringShape(t *testing.T) {
	long := longString(2000)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "user",
			"uuid": "u1",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_1",
						"content":     long, // bare string form (Bash error path)
					},
				},
			},
		},
	})
	d := &stubDistiller{suffix: "str"}
	rw, _ := New(Config{
		SessionPath:         path,
		Distiller:           d,
		Logger:              quietLogger(),
		ToolResultThreshold: 1000,
	})

	stats, err := rw.DistillTurn(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 1 {
		t.Errorf("RecordsRewritten = %d, want 1", stats.RecordsRewritten)
	}

	out := readJSONL(t, path)
	tr := out[0]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if got, ok := tr["content"].(string); !ok || !strings.HasPrefix(got, "[distilled]") {
		t.Errorf("string-form content not distilled: %v", tr["content"])
	}
}

func TestDistillTurn_StructuralFieldsPreserved(t *testing.T) {
	// Critical safety property: distillation MUST NOT touch uuid,
	// parentUuid, sessionId, version, cwd, gitBranch, type, etc.
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type":       "assistant",
			"uuid":       "a1",
			"parentUuid": "p1",
			"sessionId":  "sess-xyz",
			"version":    "2.1.113",
			"cwd":        "/tmp/work",
			"gitBranch":  "main",
			"timestamp":  "2026-05-10T12:00:00Z",
			"message": map[string]any{
				"id":         "msg_01",
				"model":      "claude-opus-4-7",
				"role":       "assistant",
				"stop_reason": "end_turn",
				"content":    []any{map[string]any{"type": "text", "text": long}},
			},
		},
	})
	d := &stubDistiller{suffix: "preserve"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})
	if _, err := rw.DistillTurn(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}

	out := readJSONL(t, path)
	rec := out[0]
	for _, field := range []string{"type", "uuid", "parentUuid", "sessionId", "version", "cwd", "gitBranch", "timestamp"} {
		if rec[field] == nil {
			t.Errorf("structural field %q stripped", field)
		}
	}
	if rec["uuid"] != "a1" || rec["parentUuid"] != "p1" || rec["sessionId"] != "sess-xyz" {
		t.Errorf("structural fields mutated: uuid=%v parentUuid=%v sessionId=%v",
			rec["uuid"], rec["parentUuid"], rec["sessionId"])
	}
	msg := rec["message"].(map[string]any)
	if msg["id"] != "msg_01" || msg["model"] != "claude-opus-4-7" || msg["stop_reason"] != "end_turn" {
		t.Errorf("message metadata stripped: %+v", msg)
	}
}

func TestDistillTurn_DistillerError_DoesNotStampMarker(t *testing.T) {
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type":    "assistant",
			"uuid":    "a1",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}},
		},
	})
	d := &stubDistiller{failNext: true, failNextErr: errors.New("haiku timeout")}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})

	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		// Distiller errors don't bubble out of DistillTurn — we log
		// and continue. err nil is correct here.
		t.Fatalf("DistillTurn returned error: %v", err)
	}
	if stats.DistillerErrors != 1 {
		t.Errorf("DistillerErrors = %d, want 1", stats.DistillerErrors)
	}
	if stats.RecordsRewritten != 0 {
		t.Errorf("RecordsRewritten = %d, want 0 (errored records aren't rewritten)", stats.RecordsRewritten)
	}
	out := readJSONL(t, path)
	if _, ok := out[0][MarkerKey]; ok {
		t.Errorf("marker stamped on errored record — must not happen, retry semantics depend on absence")
	}
}

func TestDistillTurn_MalformedLine_PassesThrough(t *testing.T) {
	// Insert a malformed line directly. The rewriter must not corrupt
	// or drop it — write-back leaves it byte-identical.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	rec1, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"uuid": "a1",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": longString(800)}},
		},
	})
	body := string(rec1) + "\n" + "this is not json\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &stubDistiller{suffix: "ok"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})
	if _, err := rw.DistillTurn(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "this is not json") {
		t.Errorf("malformed line dropped from output: %q", got)
	}
}

// Boundary case: content of EXACTLY threshold bytes must NOT be
// distilled (spec uses strict `>`). One byte larger MUST be.
func TestDistillTurn_ThresholdBoundary(t *testing.T) {
	threshold := 100
	atThreshold := longString(threshold)
	overThreshold := longString(threshold + 1)

	path := writeJSONL(t, []map[string]any{
		{
			"type":    "assistant",
			"uuid":    "at",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": atThreshold}}},
		},
		{
			"type":    "assistant",
			"uuid":    "over",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": overThreshold}}},
		},
	})
	d := &stubDistiller{suffix: "boundary"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: threshold,
	})

	// Distill turn boundary at "over" — both records in scope.
	stats, err := rw.DistillTurn(context.Background(), "over")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 1 {
		t.Errorf("RecordsRewritten = %d, want 1 (only the >threshold record)", stats.RecordsRewritten)
	}
	if d.textCalls != 1 {
		t.Errorf("textCalls = %d, want 1 — at-threshold record must not call distiller", d.textCalls)
	}
}

// Multi-block partial-threshold: same record has block 0 below
// threshold and block 1 above. Only block 1 is distilled; block 0 is
// preserved exactly; the marker IS stamped because something changed.
func TestDistillTurn_MultiBlockPartialThreshold(t *testing.T) {
	short := "short"
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type": "assistant",
			"uuid": "a1",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": short},
					map[string]any{"type": "text", "text": long},
				},
			},
		},
	})
	d := &stubDistiller{suffix: "partial"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})

	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 1 || d.textCalls != 1 {
		t.Errorf("expected 1 distill call on block 1: stats=%+v calls=%d", stats, d.textCalls)
	}
	out := readJSONL(t, path)
	rec := out[0]
	if _, ok := rec[MarkerKey]; !ok {
		t.Errorf("marker missing despite partial change")
	}
	blocks := rec["message"].(map[string]any)["content"].([]any)
	if got := blocks[0].(map[string]any)["text"].(string); got != short {
		t.Errorf("block 0 mutated despite below-threshold: %q", got)
	}
	if got := blocks[1].(map[string]any)["text"].(string); !strings.HasPrefix(got, "[distilled-text]") {
		t.Errorf("block 1 not distilled: %q", got[:50])
	}
}

// Empty session (no records). DistillTurn with any boundary uuid
// should return ErrNoBoundary cleanly, not crash.
func TestDistillTurn_EmptySession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	rw, _ := New(Config{
		SessionPath: path,
		Distiller:   &stubDistiller{suffix: "x"},
		Logger:      quietLogger(),
	})
	if _, err := rw.DistillTurn(context.Background(), "any"); !errors.Is(err, ErrNoBoundary) {
		t.Errorf("got %v, want ErrNoBoundary on empty session", err)
	}
}

// Single-record session with boundary at index 0: should distill that
// record (the spec's "first turn ever" path).
func TestDistillTurn_BoundaryAtIndexZero(t *testing.T) {
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type":    "assistant",
			"uuid":    "first",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}},
		},
	})
	d := &stubDistiller{suffix: "first"}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})
	stats, err := rw.DistillTurn(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsScanned != 1 || stats.RecordsRewritten != 1 {
		t.Errorf("boundary at 0: scanned=%d rewritten=%d, want 1/1", stats.RecordsScanned, stats.RecordsRewritten)
	}
}

func TestDistillTurn_DistillerNoOp_NoMarker(t *testing.T) {
	// If the distiller chooses to return content unchanged, we should
	// NOT stamp a marker — preserves the ability for thresholds to
	// re-evaluate the record on a future pass.
	long := longString(800)
	path := writeJSONL(t, []map[string]any{
		{
			"type":    "assistant",
			"uuid":    "a1",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": long}}},
		},
	})
	d := &stubDistiller{identityReturn: true}
	rw, _ := New(Config{
		SessionPath:            path,
		Distiller:              d,
		Logger:                 quietLogger(),
		AssistantTextThreshold: 100,
	})

	stats, err := rw.DistillTurn(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsRewritten != 0 {
		t.Errorf("RecordsRewritten = %d, want 0 when distiller returns identity", stats.RecordsRewritten)
	}
	out := readJSONL(t, path)
	if _, ok := out[0][MarkerKey]; ok {
		t.Errorf("marker stamped despite no content change")
	}
}
