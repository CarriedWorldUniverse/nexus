package cfgreconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeReader serves prefix snapshots + single values from fixed maps.
type fakeReader struct {
	snaps map[string]map[string]string // prefix -> {leaf: raw}
	vals  map[string]string            // path -> raw
	err   error
}

func (f *fakeReader) Snapshot(_ context.Context, prefix string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snaps[prefix], nil
}
func (f *fakeReader) Value(_ context.Context, path string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.vals[path]
	return v, ok, nil
}

// --- provider-bindings ---

type fakeAspectStore struct {
	rows map[string]*aspects.Aspect
	sets []struct{ name, provider, model string }
}

func (s *fakeAspectStore) Get(_ context.Context, name string) (*aspects.Aspect, error) {
	a, ok := s.rows[name]
	if !ok {
		return nil, aspects.ErrNotFound
	}
	return a, nil
}
func (s *fakeAspectStore) SetProviderAndModel(_ context.Context, name, provider, model string) error {
	s.sets = append(s.sets, struct{ name, provider, model string }{name, provider, model})
	if a, ok := s.rows[name]; ok {
		a.Provider, a.Model = provider, model
	}
	return nil
}

func TestProviderBindings_WriteThroughAndSkip(t *testing.T) {
	store := &fakeAspectStore{rows: map[string]*aspects.Aspect{
		"anvil": {Name: "anvil", Provider: "codex", Model: "default"},
		"keel":  {Name: "keel", Provider: "openai", Model: "ornith"},
	}}
	r := &fakeReader{snaps: map[string]map[string]string{ProviderBindingPrefix: {
		"anvil":   `{"provider":"openai","model":"ornith"}`,   // change
		"keel":    `{"provider":"openai","model":"ornith"}`,   // no change
		"ghost":   `{"provider":"openai","model":"ornith"}`,   // not in store -> skip
		"bad":     `{"provider":"nope","model":"x"}`,          // unsupported -> skip
		"garbage": `xxx`,                                       // malformed -> skip
	}}}
	n, err := NewProviderBindings(r, store, quietLog()).ReconcileOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("want 1 update, got n=%d err=%v", n, err)
	}
	if len(store.sets) != 1 || store.sets[0].name != "anvil" || store.sets[0].model != "ornith" {
		t.Fatalf("wrong write-through: %+v", store.sets)
	}
}

// --- network-defaults ---

type fakeNDStore struct {
	cur  credentials.NetworkDefaults
	sets []struct{ col, val string }
}

func (s *fakeNDStore) GetNetworkDefaults(context.Context) (credentials.NetworkDefaults, error) {
	return s.cur, nil
}
func (s *fakeNDStore) SetNetworkDefaultField(_ context.Context, col, val string) error {
	s.sets = append(s.sets, struct{ col, val string }{col, val})
	return nil
}

func TestNetworkDefaults_ChangedFieldsOnly(t *testing.T) {
	store := &fakeNDStore{cur: credentials.NetworkDefaults{JudgeProvider: "openai", JudgeModel: "deepseek-v4-flash"}}
	// doc changes judge_model, leaves judge_provider equal, omits the rest.
	r := &fakeReader{vals: map[string]string{NetworkDefaultsPath: `{"judge_provider":"openai","judge_model":"glm-4.6"}`}}
	n, err := NewNetworkDefaults(r, store, quietLog()).ReconcileOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("want 1 update, got n=%d err=%v", n, err)
	}
	if len(store.sets) != 1 || store.sets[0].col != "judge_model" || store.sets[0].val != "glm-4.6" {
		t.Fatalf("wrong write-through: %+v", store.sets)
	}
}

func TestNetworkDefaults_NoDocNoOp(t *testing.T) {
	store := &fakeNDStore{cur: credentials.NetworkDefaults{JudgeModel: "x"}}
	r := &fakeReader{vals: map[string]string{}} // no doc
	n, err := NewNetworkDefaults(r, store, quietLog()).ReconcileOnce(context.Background())
	if err != nil || n != 0 || len(store.sets) != 0 {
		t.Fatalf("want no-op; n=%d sets=%+v err=%v", n, store.sets, err)
	}
}

// --- wake-policy ---

type fakeWakeSetter struct {
	cur  map[string]string
	sets []struct{ aspect, policy string }
}

func (s *fakeWakeSetter) SetWakePolicy(aspect, policy string) bool {
	s.sets = append(s.sets, struct{ aspect, policy string }{aspect, policy})
	if s.cur[aspect] == policy {
		return false
	}
	s.cur[aspect] = policy
	return true
}

func TestWakePolicy_ValidChangesOnly(t *testing.T) {
	setter := &fakeWakeSetter{cur: map[string]string{"anvil": "wake-on-mention", "keel": "always-on"}}
	r := &fakeReader{snaps: map[string]map[string]string{WakePolicyPrefix: {
		"anvil": "always-on",     // change
		"keel":  "always-on",     // no change
		"maren": "bogus",         // invalid -> skip
	}}}
	n, err := NewWakePolicy(r, setter, quietLog()).ReconcileOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("want 1 change, got n=%d err=%v", n, err)
	}
	if setter.cur["anvil"] != "always-on" {
		t.Fatalf("anvil policy not updated: %v", setter.cur)
	}
}

func TestReader_ErrorAborts(t *testing.T) {
	r := &fakeReader{err: errors.New("almanac down")}
	if _, err := NewProviderBindings(r, &fakeAspectStore{}, quietLog()).ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected error from snapshot failure")
	}
	if _, err := NewNetworkDefaults(r, &fakeNDStore{}, quietLog()).ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected error from value failure")
	}
}
