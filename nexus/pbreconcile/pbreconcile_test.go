package pbreconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeReader returns a fixed snapshot (or an error).
type fakeReader struct {
	snap map[string]string
	err  error
}

func (f *fakeReader) Snapshot(context.Context) (map[string]string, error) {
	return f.snap, f.err
}

// fakeStore is an in-memory aspects store recording write-throughs.
type fakeStore struct {
	rows  map[string]*aspects.Aspect
	sets  []struct{ name, provider, model string }
	getErr error
}

func (s *fakeStore) Get(_ context.Context, name string) (*aspects.Aspect, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	a, ok := s.rows[name]
	if !ok {
		return nil, aspects.ErrNotFound
	}
	return a, nil
}

func (s *fakeStore) SetProviderAndModel(_ context.Context, name, provider, model string) error {
	s.sets = append(s.sets, struct{ name, provider, model string }{name, provider, model})
	if a, ok := s.rows[name]; ok {
		a.Provider, a.Model = provider, model
	}
	return nil
}

func TestReconcileOnce_WriteThroughChange(t *testing.T) {
	store := &fakeStore{rows: map[string]*aspects.Aspect{
		"anvil": {Name: "anvil", Provider: "codex", Model: "default"},
	}}
	rc := New(&fakeReader{snap: map[string]string{
		"anvil": `{"provider":"openai","model":"ornith"}`,
	}}, store, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated=%d, want 1", n)
	}
	if len(store.sets) != 1 || store.sets[0].provider != "openai" || store.sets[0].model != "ornith" {
		t.Fatalf("write-through wrong: %+v", store.sets)
	}
}

func TestReconcileOnce_NoChangeNoWrite(t *testing.T) {
	store := &fakeStore{rows: map[string]*aspects.Aspect{
		"anvil": {Name: "anvil", Provider: "openai", Model: "ornith"},
	}}
	rc := New(&fakeReader{snap: map[string]string{
		"anvil": `{"provider":"openai","model":"ornith"}`,
	}}, store, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil || n != 0 || len(store.sets) != 0 {
		t.Fatalf("want no-op; got n=%d sets=%+v err=%v", n, store.sets, err)
	}
}

func TestReconcileOnce_SkipsBadKeys(t *testing.T) {
	store := &fakeStore{rows: map[string]*aspects.Aspect{
		"anvil": {Name: "anvil", Provider: "codex", Model: "default"},
		"keel":  {Name: "keel", Provider: "codex", Model: "default"},
	}}
	rc := New(&fakeReader{snap: map[string]string{
		"anvil":   `{"provider":"openai"}`,        // incomplete (no model)
		"keel":    `{"provider":"nope","model":"x"}`, // unsupported provider
		"ghost":   `{"provider":"openai","model":"ornith"}`, // not in store
		"garbage": `not json`,                       // malformed
	}}, store, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 0 || len(store.sets) != 0 {
		t.Fatalf("expected all skipped; got n=%d sets=%+v", n, store.sets)
	}
}

func TestReconcileOnce_SnapshotErrorAbortsNoWrites(t *testing.T) {
	store := &fakeStore{rows: map[string]*aspects.Aspect{
		"anvil": {Name: "anvil", Provider: "codex", Model: "default"},
	}}
	rc := New(&fakeReader{err: errors.New("almanac down")}, store, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("expected error on snapshot failure")
	}
	if n != 0 || len(store.sets) != 0 {
		t.Fatalf("no writes expected when almanac is down; got n=%d sets=%+v", n, store.sets)
	}
}
