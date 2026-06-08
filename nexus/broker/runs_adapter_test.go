package broker

import (
	"context"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
)

type memRuns struct{ rows map[string]runs.Run }

func (m *memRuns) Migrate(context.Context) error { return nil }

func (m *memRuns) Insert(_ context.Context, r runs.Run) error {
	if m.rows == nil {
		m.rows = map[string]runs.Run{}
	}
	m.rows[r.RunID] = r
	return nil
}

func (m *memRuns) MarkDone(_ context.Context, id string, st runs.Status, t time.Time, pr string, d int) error {
	r := m.rows[id]
	r.Status, r.CompletedAt, r.PRURL, r.DurationSecs = st, t, pr, d
	m.rows[id] = r
	return nil
}

func (m *memRuns) List(context.Context, int) ([]runs.Run, error) { return nil, nil }

func (m *memRuns) Get(_ context.Context, id string) (runs.Run, error) { return m.rows[id], nil }

func TestAdapterRecordsStartAndDone(t *testing.T) {
	store := &memRuns{}
	a := newRunsAdapter(store, func(runs.Run) {})
	a.RecordRunStart(context.Background(), "run-a", "NEX-1", "anvil", "NEX-1", "o/r", "cmd", "", 7)
	if got := store.rows["run-a"]; got.Status != runs.StatusRunning || got.DispatchMsgID != 7 {
		t.Fatalf("start: %+v", got)
	}
	a.RecordRunDone(context.Background(), "run-a", "complete", time.UnixMilli(9000), "pr", 4)
	if got := store.rows["run-a"]; got.Status != runs.StatusComplete || got.DurationSecs != 4 {
		t.Fatalf("done: %+v", got)
	}
}
