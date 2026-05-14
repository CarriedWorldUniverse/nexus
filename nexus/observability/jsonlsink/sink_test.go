package jsonlsink

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func TestSink_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	day := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	frames := []observability.Frame{
		{Kind: observability.FrameTurn, Aspect: "keel", Sequence: 1, TS: day, Payload: json.RawMessage(`{"foo":1}`)},
		{Kind: observability.FrameChat, Aspect: "keel", Sequence: 2, TS: day.Add(time.Second), Payload: json.RawMessage(`{"bar":2}`)},
		{Kind: observability.FrameTurn, Aspect: "plumb", Sequence: 1, TS: day, Payload: json.RawMessage(`{"qux":3}`)},
	}
	for _, f := range frames {
		s.OnFrame(f.Aspect, f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify keel file has 2 lines, plumb has 1, both at the expected
	// daily path.
	keelPath := filepath.Join(dir, "keel", "2026-05-14.jsonl")
	plumbPath := filepath.Join(dir, "plumb", "2026-05-14.jsonl")
	if got := countLines(t, keelPath); got != 2 {
		t.Errorf("keel lines = %d; want 2", got)
	}
	if got := countLines(t, plumbPath); got != 1 {
		t.Errorf("plumb lines = %d; want 1", got)
	}

	// Round-trip the first keel line through JSON unmarshal — confirms
	// the on-disk format is valid JSONL.
	raw, err := os.ReadFile(keelPath)
	if err != nil {
		t.Fatalf("read %q: %v", keelPath, err)
	}
	firstLine := strings.SplitN(string(raw), "\n", 2)[0]
	var got observability.Frame
	if err := json.Unmarshal([]byte(firstLine), &got); err != nil {
		t.Fatalf("unmarshal: %v (line=%q)", err, firstLine)
	}
	if got.Aspect != "keel" || got.Sequence != 1 || got.Kind != observability.FrameTurn {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestSink_DailyRotation(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	day1 := time.Date(2026, 5, 14, 23, 59, 59, 0, time.UTC)
	day2 := time.Date(2026, 5, 15, 0, 0, 1, 0, time.UTC)
	s.OnFrame("keel", observability.Frame{Aspect: "keel", Kind: observability.FrameTurn, Sequence: 1, TS: day1, Payload: json.RawMessage(`{}`)})
	s.OnFrame("keel", observability.Frame{Aspect: "keel", Kind: observability.FrameTurn, Sequence: 2, TS: day2, Payload: json.RawMessage(`{}`)})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, day := range []string{"2026-05-14", "2026-05-15"} {
		path := filepath.Join(dir, "keel", day+".jsonl")
		if got := countLines(t, path); got != 1 {
			t.Errorf("day %s: lines = %d; want 1", day, got)
		}
	}
}

func TestSink_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.OnFrame("keel", observability.Frame{Aspect: "keel", Kind: observability.FrameTurn, TS: time.Now(), Payload: json.RawMessage(`{}`)})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close (first): %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Errorf("Close (second): want nil, got %v", err)
	}
}

func TestSink_EmptyAspectDropped(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Empty-aspect frame should not produce any file.
	s.OnFrame("", observability.Frame{Kind: observability.FrameTurn, TS: time.Now(), Payload: json.RawMessage(`{}`)})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no per-aspect dirs, got %d entries", len(entries))
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}
