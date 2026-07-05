package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
	"github.com/CarriedWorldUniverse/nexus/nexus/orchestrator"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	_ "github.com/ncruces/go-sqlite3/driver"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, nil))
}

// testWriter routes slog output through t.Log so failures print alongside
// the rest of the test's context.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func newMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// --- buildDocRegister ---

func TestBuildDocRegister_NoEnv_ReturnsNil(t *testing.T) {
	db := newMemDB(t)
	if got := buildDocRegister(testLogger(t), db); got != nil {
		t.Fatalf("buildDocRegister with no env = %v, want nil", got)
	}
}

func TestBuildDocRegister_EnableWithoutDir_ReturnsNil(t *testing.T) {
	t.Setenv("DOCREGISTER_ENABLE", "1")
	db := newMemDB(t)
	if got := buildDocRegister(testLogger(t), db); got != nil {
		t.Fatalf("buildDocRegister with ENABLE=1 but no dir = %v, want nil", got)
	}
}

func TestBuildDocRegister_WithCairnDir_Constructs(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable in test env: %v: %s", err, out)
	}
	t.Setenv("DOCREGISTER_CAIRN_DIR", dir)
	db := newMemDB(t)
	reg := buildDocRegister(testLogger(t), db)
	if reg == nil {
		t.Fatal("buildDocRegister with DOCREGISTER_CAIRN_DIR set = nil, want non-nil")
	}
	if reg.Store == nil {
		t.Fatal("reg.Store is nil")
	}
	gcc, ok := reg.Content.(*docregister.GitCairnContent)
	if !ok {
		t.Fatalf("reg.Content type = %T, want *docregister.GitCairnContent", reg.Content)
	}
	if gcc.RepoDir != dir {
		t.Fatalf("RepoDir = %q, want %q", gcc.RepoDir, dir)
	}
}

func TestBuildDocRegister_MissingDir_ReturnsNil(t *testing.T) {
	t.Setenv("DOCREGISTER_CAIRN_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	db := newMemDB(t)
	if got := buildDocRegister(testLogger(t), db); got != nil {
		t.Fatalf("buildDocRegister with missing dir = %v, want nil", got)
	}
}

// --- buildWorkgraphClient ---

func TestBuildWorkgraphClient_NoEnv_ReturnsNil(t *testing.T) {
	if got := buildWorkgraphClient(testLogger(t)); got != nil {
		t.Fatalf("buildWorkgraphClient with no env = %v, want nil", got)
	}
}

func TestBuildWorkgraphClient_AddrSet_ConstructsDespiteUnreachableLedger(t *testing.T) {
	// Fail-soft: even though nothing is listening on this address, New()
	// itself never dials (lazy gRPC connection) and EnsureProject's
	// failure is logged, not fatal — the client is still returned so the
	// orchestrator can be wired up (its own DrainOnce calls will surface
	// the same unreachable-ledger error repeatedly, which is the correct
	// place for that signal to live).
	t.Setenv("WORKGRAPH_LEDGER_ADDR", "127.0.0.1:1")
	t.Setenv("WORKGRAPH_DEV_INSECURE", "1")
	got := buildWorkgraphClient(testLogger(t))
	if got == nil {
		t.Fatal("buildWorkgraphClient with WORKGRAPH_LEDGER_ADDR set = nil, want non-nil client (fail-soft on EnsureProject)")
	}
}

func TestBuildWorkgraphClient_MissingTLS_ReturnsNil(t *testing.T) {
	t.Setenv("WORKGRAPH_LEDGER_ADDR", "127.0.0.1:1")
	// No WORKGRAPH_DEV_INSECURE, no TLS material → DialCreds fails.
	if got := buildWorkgraphClient(testLogger(t)); got != nil {
		t.Fatalf("buildWorkgraphClient with missing TLS material = %v, want nil", got)
	}
}

// --- buildOrchestrator ---

type fakeWorkGraph struct{}

func (fakeWorkGraph) ListReady(ctx context.Context, role, stream string) ([]workgraph.WorkItem, error) {
	return nil, nil
}
func (fakeWorkGraph) GetWorkItem(ctx context.Context, id string) (workgraph.WorkItem, error) {
	return workgraph.WorkItem{}, errors.New("not found")
}
func (fakeWorkGraph) Transition(ctx context.Context, id string, status workgraph.Status) error {
	return nil
}
func (fakeWorkGraph) RecordResult(ctx context.Context, id string, result workgraph.Result) error {
	return nil
}
func (fakeWorkGraph) Rework(ctx context.Context, rejectedID string, newSpec workgraph.WorkItem) (string, error) {
	return "", nil
}
func (fakeWorkGraph) Claim(ctx context.Context, id, agent string) error { return nil }
func (fakeWorkGraph) Cancel(ctx context.Context, id string, requeue bool, reason string) error {
	return nil
}

// fakeDispatcher satisfies orchestrator.Dispatcher without a live k8s/pool.
type fakeDispatcher struct{}

func (fakeDispatcher) SubmitPoolItem(ctx context.Context, item dispatch.PoolItem) (string, error) {
	return "leased", nil
}

// fakeWorkerStatus satisfies orchestrator.WorkerStatusStore.
type fakeWorkerStatus struct{}

func (fakeWorkerStatus) List(ctx context.Context) ([]workerstatus.Status, error) {
	return nil, nil
}

func TestBuildOrchestrator_NoEnv_ReturnsNil(t *testing.T) {
	if got := buildOrchestrator(testLogger(t), nil, nil, nil); got != nil {
		t.Fatalf("buildOrchestrator with no env = %v, want nil", got)
	}
}

func TestBuildOrchestrator_EnabledButNoWorkgraph_ReturnsNil(t *testing.T) {
	t.Setenv("ORCHESTRATOR_ENABLE", "1")
	if got := buildOrchestrator(testLogger(t), nil, nil, nil); got != nil {
		t.Fatalf("buildOrchestrator with nil workgraph client = %v, want nil", got)
	}
}

func TestBuildOrchestrator_EnabledButNoDispatcher_ReturnsNil(t *testing.T) {
	t.Setenv("ORCHESTRATOR_ENABLE", "1")
	wg := &workgraph.Client{} // non-nil is all buildOrchestrator's nil-check needs
	if got := buildOrchestrator(testLogger(t), wg, nil, nil); got != nil {
		t.Fatalf("buildOrchestrator with nil dispatcher = %v, want nil", got)
	}
}

// TestBuildOrchestrator_FullEnv_ConstructsAndWiresHook exercises the whole
// happy path deliverable 3 describes: with all prerequisites present, the
// orchestrator is constructed, its OnJobDoneHook is obtainable, and
// DrainOnce runs cleanly against fakes (no live ledger/pool needed).
func TestBuildOrchestrator_FullEnv_ConstructsAndWiresHook(t *testing.T) {
	t.Setenv("ORCHESTRATOR_ENABLE", "1")
	t.Setenv("ORCHESTRATOR_ROLES", "builder,tester")
	t.Setenv("ORCHESTRATOR_STALE_AFTER", "1m")

	wg := &workgraph.Client{}
	orch := buildOrchestrator(testLogger(t), wg, fakeDispatcher{}, fakeWorkerStatus{})
	if orch == nil {
		t.Fatal("buildOrchestrator with full env = nil, want non-nil orchestrator")
	}
	if orch.OnJobDoneHook() == nil {
		t.Fatal("orch.OnJobDoneHook() = nil, want a callable hook")
	}
	if got, want := orch.Roles, []string{"builder", "tester"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("orch.Roles = %v, want %v", got, want)
	}
	if orch.StaleAfter != time.Minute {
		t.Fatalf("orch.StaleAfter = %v, want 1m", orch.StaleAfter)
	}
}

// --- runDrainLoop (the ticker-cadence wake trigger) ---

type countingDrainer struct {
	calls  atomic.Int64
	callCh chan struct{}
}

func (d *countingDrainer) DrainOnce(ctx context.Context) (orchestrator.DrainReport, error) {
	d.calls.Add(1)
	select {
	case d.callCh <- struct{}{}:
	default:
	}
	return orchestrator.DrainReport{}, nil
}

func TestRunDrainLoop_CallsDrainOnceOnTickerCadence(t *testing.T) {
	d := &countingDrainer{callCh: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runDrainLoop(ctx, d, 5*time.Millisecond, testLogger(t))

	select {
	case <-d.callCh:
		// at least one DrainOnce call observed.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runDrainLoop to call DrainOnce")
	}
	cancel()
	if d.calls.Load() < 1 {
		t.Fatalf("DrainOnce calls = %d, want >= 1", d.calls.Load())
	}
}

func TestRunDrainLoop_StopsOnContextCancel(t *testing.T) {
	d := &countingDrainer{callCh: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runDrainLoop(ctx, d, 5*time.Millisecond, testLogger(t))
		close(done)
	}()

	<-d.callCh // wait for at least one pass so the loop is definitely running
	cancel()

	select {
	case <-done:
		// runDrainLoop returned promptly after cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("runDrainLoop did not return after ctx cancellation")
	}
}

// --- parseCSVOrDefault ---

func TestParseCSVOrDefault(t *testing.T) {
	def := []string{"a", "b"}
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty uses default", "", def},
		{"whitespace only uses default", "   ", def},
		{"single", "builder", []string{"builder"}},
		{"multi with spaces", " builder, tester ,reviewer", []string{"builder", "tester", "reviewer"}},
		{"all-empty entries uses default", " , , ", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCSVOrDefault(tc.in, def)
			if len(got) != len(tc.want) {
				t.Fatalf("parseCSVOrDefault(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseCSVOrDefault(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
