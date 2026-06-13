package broker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	if r.Status != runs.StatusAccepted && r.Status != runs.StatusSubmitted && r.Status != runs.StatusRunning && r.Status != runs.StatusQueued {
		return nil
	}
	r.Status, r.CompletedAt, r.PRURL, r.DurationSecs = st, t, pr, d
	m.rows[id] = r
	return nil
}

func (m *memRuns) MarkAccepted(_ context.Context, id string, at time.Time) error {
	r := m.rows[id]
	if r.Status != runs.StatusSubmitted {
		return nil
	}
	r.Status = runs.StatusAccepted
	r.StartedAt = at
	m.rows[id] = r
	return nil
}

func (m *memRuns) RecordLogs(_ context.Context, id, logs string) error {
	r := m.rows[id]
	r.RunID = id
	r.Logs = logs
	m.rows[id] = r
	return nil
}

func (m *memRuns) GetLogs(_ context.Context, id string) (string, error) {
	return m.rows[id].Logs, nil
}

func (m *memRuns) List(context.Context, int) ([]runs.Run, error) { return nil, nil }

func (m *memRuns) ListRunning(context.Context) ([]runs.Run, error) {
	var out []runs.Run
	for _, r := range m.rows {
		if r.Status == runs.StatusRunning {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRuns) Get(_ context.Context, id string) (runs.Run, error) { return m.rows[id], nil }

func TestAdapterRecordsStartAndDone(t *testing.T) {
	store := &memRuns{}
	a := newRunsAdapter(store, func(runs.Run) {}, slog.Default())
	a.RecordRunStart(context.Background(), "run-a", "NEX-1", "anvil", "NEX-1", "o/r", "cmd", "", 7)
	if got := store.rows["run-a"]; got.Status != runs.StatusSubmitted || got.DispatchMsgID != 7 {
		t.Fatalf("start: %+v", got)
	}
	a.RecordRunAccepted(context.Background(), "run-a", time.UnixMilli(8000))
	if got := store.rows["run-a"]; got.Status != runs.StatusAccepted || got.StartedAt.UnixMilli() != 8000 {
		t.Fatalf("accepted: %+v", got)
	}
	a.RecordRunDone(context.Background(), "run-a", "complete", time.UnixMilli(9000), "pr", 4)
	if got := store.rows["run-a"]; got.Status != runs.StatusComplete || got.DurationSecs != 4 {
		t.Fatalf("done: %+v", got)
	}
	a.RecordRunLogs(context.Background(), "run-a", "builder output\n")
	if got, _ := store.GetLogs(context.Background(), "run-a"); got != "builder output\n" {
		t.Fatalf("logs = %q", got)
	}
}

func TestDispatchStatusAcceptedFrameRecordsAccepted(t *testing.T) {
	store := &memRuns{rows: map[string]runs.Run{
		"run-a": {RunID: "run-a", Ticket: "NEX-653", Agent: "anvil", Status: runs.StatusSubmitted, StartedAt: time.UnixMilli(1)},
	}}
	b := New(Config{RunsStore: store}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: slog.Default()}
	env, err := frames.New(frames.KindDispatchStatus, frames.DispatchStatusPayload{
		RunID:  "run-a",
		Status: "accepted",
		At:     time.UnixMilli(7000),
	})
	if err != nil {
		t.Fatal(err)
	}

	c.handleDispatchStatusFrame(env)

	if got := store.rows["run-a"]; got.Status != runs.StatusAccepted || got.StartedAt.UnixMilli() != 7000 {
		t.Fatalf("run after accepted frame = %+v", got)
	}
}

func TestDispatchStatusPreAcceptanceFailureRecordsFailedAndEscalates(t *testing.T) {
	store := &memRuns{rows: map[string]runs.Run{
		"run-f": {RunID: "run-f", Ticket: "NEX-653", Agent: "anvil", Status: runs.StatusSubmitted, StartedAt: time.UnixMilli(1)},
	}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	b := New(Config{RunsStore: store, Logger: logger}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: logger}
	env, err := frames.New(frames.KindDispatchStatus, frames.DispatchStatusPayload{
		RunID:  "run-f",
		Status: "failed",
		Reason: "brief unreadable",
		At:     time.UnixMilli(9000),
	})
	if err != nil {
		t.Fatal(err)
	}

	c.handleDispatchStatusFrame(env)

	if got := store.rows["run-f"]; got.Status != runs.StatusFailed || got.CompletedAt.UnixMilli() != 9000 {
		t.Fatalf("run after failed frame = %+v", got)
	}
	if !strings.Contains(logs.String(), "dispatch: ESCALATION run failed pre-acceptance") {
		t.Fatalf("escalation log missing, logs:\n%s", logs.String())
	}
}

func TestBrokerStartupMarksRunningRunsWithoutActiveJobFailed(t *testing.T) {
	store := &memRuns{rows: map[string]runs.Run{
		"run-live":   {RunID: "run-live", Ticket: "NEX-1", Agent: "anvil", Status: runs.StatusRunning, StartedAt: time.UnixMilli(1)},
		"run-orphan": {RunID: "run-orphan", Ticket: "NEX-2", Agent: "plumb", Status: runs.StatusRunning, StartedAt: time.UnixMilli(2)},
		"run-done":   {RunID: "run-done", Ticket: "NEX-3", Agent: "keel", Status: runs.StatusComplete, StartedAt: time.UnixMilli(3)},
	}}
	cs := fake.NewSimpleClientset()
	job := dispatch.BuildJob(dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", RunID: "run-live"}, dispatch.JobConfig{Namespace: "nexus"}, "run-live", "codex-cli")
	if _, err := cs.BatchV1().Jobs("nexus").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	New(Config{RunsStore: store, K8sReader: cs, K8sNamespace: "nexus"}, roster.New())

	if got := store.rows["run-live"].Status; got != runs.StatusRunning {
		t.Fatalf("live run status = %q, want running", got)
	}
	if got := store.rows["run-orphan"].Status; got != runs.StatusFailed {
		t.Fatalf("orphan run status = %q, want failed", got)
	}
	if got := store.rows["run-done"].Status; got != runs.StatusComplete {
		t.Fatalf("terminal run status = %q, want complete", got)
	}
}
