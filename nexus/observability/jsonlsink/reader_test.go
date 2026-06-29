package jsonlsink

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func TestReadFramesByRunID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "anvil")
	_ = os.MkdirAll(dir, 0o755)
	day := time.Now().UTC().Format("2006-01-02")
	lines := `{"kind":"turn","aspect":"anvil","seq":1,"ts":"2026-06-09T00:00:00Z","run_id":"run-a","payload":{}}
{"kind":"turn","aspect":"anvil","seq":2,"ts":"2026-06-09T00:00:01Z","run_id":"run-b","payload":{}}
{"kind":"turn","aspect":"anvil","seq":3,"ts":"2026-06-09T00:00:02Z","run_id":"run-a","payload":{}}
`
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewReader(root)
	frames, err := r.ReadByRun(context.Background(), "anvil", "run-a", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Sequence != 1 || frames[1].Sequence != 3 {
		t.Fatalf("ReadByRun: %+v", frames)
	}
	_ = observability.Frame{}
}

func TestReadFramesByRunWindowFreezesCompletedRun(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "anvil")
	_ = os.MkdirAll(dir, 0o755)
	day := time.Now().UTC().Format("2006-01-02")
	lines := `{"kind":"presence","aspect":"anvil","seq":1,"ts":"2026-06-09T00:00:00Z","payload":{}}
{"kind":"turn","aspect":"anvil","seq":2,"ts":"2026-06-09T00:00:01Z","run_id":"run-a","payload":{}}
{"kind":"turn","aspect":"anvil","seq":5,"ts":"2026-06-09T00:00:01.500Z","run_id":"run-b","payload":{}}
{"kind":"chat","aspect":"anvil","seq":3,"ts":"2026-06-09T00:00:02Z","payload":{}}
{"kind":"presence","aspect":"anvil","seq":4,"ts":"2026-06-09T00:00:03Z","run_id":"run-a","payload":{}}
`
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewReader(root)
	frames, err := r.ReadByRunWindow(
		context.Background(),
		"anvil",
		"run-a",
		time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 9, 0, 0, 2, 0, time.UTC),
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("len = %d, frames = %+v", len(frames), frames)
	}
	for _, f := range frames {
		if f.Sequence == 4 {
			t.Fatalf("completed run included stale later activity: %+v", frames)
		}
	}
}
