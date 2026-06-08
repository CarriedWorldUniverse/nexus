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
