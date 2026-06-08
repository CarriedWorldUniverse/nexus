package jsonlsink

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// Reader reads persisted frames back from the JSONL sink root.
type Reader struct{ root string }

func NewReader(root string) *Reader { return &Reader{root: root} }

// ReadByRun returns frames for an aspect whose RunID matches, newest day first
// scanned but returned in sequence order, capped at limit. Missing files are
// not an error (returns what exists).
func (r *Reader) ReadByRun(ctx context.Context, aspect, runID string, limit int) ([]observability.Frame, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	dir := filepath.Join(r.root, aspect)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []observability.Frame
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		fs, err := scanFile(filepath.Join(dir, e.Name()), func(f observability.Frame) bool {
			return f.RunID == runID
		})
		if err != nil {
			continue // a corrupt/locked day file must not sink the whole read
		}
		out = append(out, fs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func scanFile(path string, keep func(observability.Frame) bool) ([]observability.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []observability.Frame
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var fr observability.Frame
		if json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		if keep(fr) {
			out = append(out, fr)
		}
	}
	return out, sc.Err()
}
