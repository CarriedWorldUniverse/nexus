# Work UI Phase 1 (Watch + Run Spine + Env-Health) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a dispatch run's full story (command + activity + outcome, live and historical) visible in a read-only Watch surface by making the run a first-class, persisted, queryable object.

**Architecture:** A new `runs` read-model (sqld table + `nexus/runs` store) written by the dispatch runner on reserve and on `JobDone`. New operator WS RPCs (`runs.list`, `run.get`, `activity.history`, `env.health`) follow the existing `dispatchOperatorFrame` pattern. Activity frames gain a `run_id` tag for exact run↔activity association. The build-free Preact+htm dashboard gains a three-area shell (Converse · Watch · Configure, Watch active) with a stable run-feed + unified-timeline surface and openable Team/Env-health panels.

**Tech Stack:** Go (broker `nexus/broker`, dispatch `runtime/dispatch`, observability `nexus/observability`), sqld/libSQL (`database/sql`), Preact + htm via `window.__preact` (no build step), WS frames (`nexus/frames`).

**Spec:** `docs/2026-06-09-work-ui-phase1-watch-design.md`. **Branch:** `design/work-ui-phase1`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

**Backend (Go):**
- Create `nexus/runs/runs.go` — the `Run` type, `Store` interface, `SQLStore` impl, migration. One responsibility: persist/query runs.
- Create `nexus/runs/runs_test.go` — store tests against an in-memory sqlite.
- Modify `runtime/dispatch/runner.go` — call a new `RunsRecorder` on reserve and on `JobDone`.
- Create `runtime/dispatch/runs_recorder.go` — the `RunsRecorder` interface (primitive params, no shared type → no import coupling).
- Modify `nexus/observability/types.go` — add `RunID` to `Frame`.
- Modify the agentfunnel turn/presence emit site — populate `Frame.RunID` from `CW_DISPATCH_RUN_ID`.
- Create `nexus/observability/jsonlsink/reader.go` — read an aspect's JSONL frames for a run/window.
- Create `nexus/broker/runs_rpc.go` — `runs.list` + `run.get` handlers + the timeline merge.
- Create `nexus/broker/activity_rpc.go` — `activity.history` handler.
- Create `nexus/broker/env_health.go` — `env.health` handler (k8s reads).
- Modify `nexus/broker/operator_frames.go` — register the four new kinds in `dispatchOperatorFrame`.
- Modify `nexus/frames/payloads.go` + `nexus/frames/frames.go` — new frame kinds + payloads.
- Modify `nexus/broker/server.go` — add `RunsStore` + `K8sReader` to `Config`; wire the runs adapter into the runner; broadcast `runs.update`.

**Frontend (`nexus/broker/static/dashboard/`):**
- Modify `js/app.js` — three-area shell nav + panel-toggle state + Watch route.
- Create `js/views/WatchView.js` — run feed + unified timeline.
- Create `js/views/panels/TeamPanel.js`, `js/views/panels/EnvHealthPanel.js`.
- Modify `js/api.js` — `runsList`/`runGet`/`activityHistory`/`envHealth` + `runs.update` sub.
- Create `css/watch.css` — Watch + panel styles (reuse `css/tokens.css`; `MessageBubble`/`chat.css` unchanged).

**Infra:**
- Modify `carriedworld-cloud/hosting/services/nexus-broker-dispatch-rbac.yaml` — add `deployments` (and confirm `pods`) get/list; apply.

---

## Task 1: Runs read-model — `Run` type, `Store` interface, `SQLStore`

**Files:**
- Create: `nexus/runs/runs.go`
- Test: `nexus/runs/runs_test.go`

Mirror the chat store pattern (`nexus/chat/chat.go:203` — `SQLStore{ DB *sql.DB }`, `BeginTx`, `ExecContext`, `QueryRowContext`).

- [ ] **Step 1: Write the failing test**

```go
// nexus/runs/runs_test.go
package runs

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3" // same driver the chat store tests use
)

func newTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInsertThenMarkDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	start := time.UnixMilli(1_000)

	err := s.Insert(ctx, Run{
		RunID: "run-abc", Ticket: "NEX-1", Agent: "anvil", Thread: "NEX-1",
		DispatchMsgID: 42, Command: "do the thing", Repo: "org/repo",
		Status: StatusRunning, StartedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || got.Agent != "anvil" || got.DispatchMsgID != 42 {
		t.Fatalf("after insert: %+v", got)
	}

	done := time.UnixMilli(5_000)
	if err := s.MarkDone(ctx, "run-abc", StatusComplete, done, "https://pr/1", 4); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "run-abc")
	if got.Status != StatusComplete || got.PRURL != "https://pr/1" || got.DurationSecs != 4 {
		t.Fatalf("after done: %+v", got)
	}
	if got.CompletedAt.UnixMilli() != 5_000 {
		t.Fatalf("completed_at = %v", got.CompletedAt)
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i, id := range []string{"run-1", "run-2", "run-3"} {
		_ = s.Insert(ctx, Run{RunID: id, Ticket: id, Agent: "anvil",
			Status: StatusRunning, StartedAt: time.UnixMilli(int64(i + 1))})
	}
	got, err := s.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RunID != "run-3" {
		t.Fatalf("list newest-first: %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/runs/ -run TestInsert -v`
Expected: FAIL — package/types not defined.

- [ ] **Step 3: Implement `nexus/runs/runs.go`**

```go
// nexus/runs/runs.go
package runs

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusComplete  Status = "complete"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Run is the persisted dispatch-run read-model.
type Run struct {
	RunID         string    `json:"run_id"`
	Ticket        string    `json:"ticket"`
	Agent         string    `json:"agent"`
	Thread        string    `json:"thread"`
	DispatchMsgID int64     `json:"dispatch_msg_id"`
	ParentRunID   string    `json:"parent_run_id,omitempty"`
	Command       string    `json:"command"`
	Repo          string    `json:"repo,omitempty"`
	Status        Status    `json:"status"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
	DurationSecs  int       `json:"duration_secs,omitempty"`
}

// Store is the runs read-model. The dispatch runner is the only writer.
type Store interface {
	Migrate(ctx context.Context) error
	Insert(ctx context.Context, r Run) error
	MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int) error
	List(ctx context.Context, limit int) ([]Run, error)
	Get(ctx context.Context, runID string) (Run, error)
}

type SQLStore struct{ DB *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS runs (
			run_id          TEXT PRIMARY KEY,
			ticket          TEXT NOT NULL,
			agent           TEXT NOT NULL,
			thread          TEXT NOT NULL,
			dispatch_msg_id INTEGER NOT NULL DEFAULT 0,
			parent_run_id   TEXT NOT NULL DEFAULT '',
			command         TEXT NOT NULL DEFAULT '',
			repo            TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			started_at      INTEGER NOT NULL,
			completed_at    INTEGER NOT NULL DEFAULT 0,
			pr_url          TEXT NOT NULL DEFAULT '',
			duration_secs   INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);`)
	if err != nil {
		return fmt.Errorf("runs.Migrate: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, r Run) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO runs (run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		                  command, repo, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		r.RunID, r.Ticket, r.Agent, r.Thread, r.DispatchMsgID, r.ParentRunID,
		r.Command, r.Repo, string(r.Status), r.StartedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("runs.Insert: %w", err)
	}
	return nil
}

func (s *SQLStore) MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE runs SET status = ?, completed_at = ?, pr_url = ?, duration_secs = ?
		WHERE run_id = ?`,
		string(status), completedAt.UnixMilli(), prURL, durationSecs, runID)
	if err != nil {
		return fmt.Errorf("runs.MarkDone: %w", err)
	}
	return nil
}

func (s *SQLStore) List(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, started_at, completed_at, pr_url, duration_secs
		FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runs.List: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) Get(ctx context.Context, runID string) (Run, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, started_at, completed_at, pr_url, duration_secs
		FROM runs WHERE run_id = ?`, runID)
	return scanRun(row)
}

type scanner interface{ Scan(...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var status string
	var startedMs, completedMs int64
	if err := sc.Scan(&r.RunID, &r.Ticket, &r.Agent, &r.Thread, &r.DispatchMsgID,
		&r.ParentRunID, &r.Command, &r.Repo, &status, &startedMs, &completedMs,
		&r.PRURL, &r.DurationSecs); err != nil {
		return Run{}, err
	}
	r.Status = Status(status)
	r.StartedAt = time.UnixMilli(startedMs)
	if completedMs > 0 {
		r.CompletedAt = time.UnixMilli(completedMs)
	}
	return r, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/runs/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add nexus/runs/runs.go nexus/runs/runs_test.go
git commit -m "feat(runs): persisted runs read-model (store + migration)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `RunsRecorder` interface + runner writes on reserve and JobDone

**Files:**
- Create: `runtime/dispatch/runs_recorder.go`
- Modify: `runtime/dispatch/runner.go` (`reserve` ~line 224, `OnJobDone` ~line 244)
- Test: `runtime/dispatch/runs_recorder_test.go`

The recorder is defined with primitive params (no `nexus/runs` import in dispatch — avoids coupling). The broker wires an adapter (Task 9).

- [ ] **Step 1: Write the failing test**

```go
// runtime/dispatch/runs_recorder_test.go
package dispatch

import (
	"context"
	"testing"
	"time"
)

type fakeRecorder struct {
	started []startCall
	done    []doneCall
}
type startCall struct{ runID, ticket, agent, thread, repo, command, parent string }
type doneCall struct {
	runID, status, pr string
	dur               int
}

func (f *fakeRecorder) RecordRunStart(_ context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64) {
	f.started = append(f.started, startCall{runID, ticket, agent, thread, repo, command, parentRunID})
}
func (f *fakeRecorder) RecordRunDone(_ context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int) {
	f.done = append(f.done, doneCall{runID, status, prURL, durationSecs})
}

func TestReserveRecordsRunStart(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Runner{Recorder: rec, NewID: func() string { return "run-x" }}
	r.initMaps()
	run := r.reserve(Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Repo: "o/r", Task: "brief text"})
	if run.ID != "run-x" {
		t.Fatalf("run id = %q", run.ID)
	}
	if len(rec.started) != 1 || rec.started[0].agent != "anvil" || rec.started[0].command != "brief text" {
		t.Fatalf("RecordRunStart not called correctly: %+v", rec.started)
	}
}

func TestJobDoneRecordsRunDone(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Runner{Recorder: rec, NewID: func() string { return "run-y" }, post: func(string, string) {}}
	r.initMaps()
	r.reserve(Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "NEX-2"})
	r.OnJobDone(JobDone{Ticket: "NEX-2", OK: true, CompletedAt: time.UnixMilli(9000)})
	if len(rec.done) != 1 || rec.done[0].runID != "run-y" || rec.done[0].status != "complete" {
		t.Fatalf("RecordRunDone not called correctly: %+v", rec.done)
	}
}
```

> NOTE: `initMaps` and the `post` field may already exist under other names. Inspect `runner.go` and adapt the test harness to the real constructor; the assertions (RecordRunStart on reserve, RecordRunDone with status="complete" on a successful JobDone) are the contract.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/dispatch/ -run TestReserveRecords -v`
Expected: FAIL — `Recorder` field undefined.

- [ ] **Step 3: Implement the recorder interface + wire the runner**

```go
// runtime/dispatch/runs_recorder.go
package dispatch

import (
	"context"
	"time"
)

// RunsRecorder persists run lifecycle. Primitive params keep dispatch free of a
// store-package import. The broker adapts nexus/runs.Store to this.
type RunsRecorder interface {
	RecordRunStart(ctx context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64)
	RecordRunDone(ctx context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int)
}

func statusFor(ok bool) string {
	if ok {
		return "complete"
	}
	return "failed"
}
```

Add `Recorder RunsRecorder` to the `Runner` struct. In `reserve` (runner.go:224), after `r.active[runID] = run`, record the start (guard nil):

```go
func (r *Runner) reserve(b Brief) *Run {
	runID := r.nextID()
	b.RunID = runID
	run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, Started: time.Now()}
	r.active[runID] = run
	r.agentBusy[b.Agent] = runID
	if r.Recorder != nil {
		r.Recorder.RecordRunStart(r.ctx, runID, b.Ticket, b.Agent, b.Thread, b.Repo, b.Task, b.ParentRunID, b.DispatchMsgID)
	}
	return run
}
```

> `Brief.DispatchMsgID int64` does not exist yet — add it to the `Brief` struct in `runtime/dispatch/brief.go` (alongside `Thread`). It is populated by the dispatch intercept in Task 8.

In `OnJobDone` (runner.go:244), after resolving `run` and computing `done.CompletedAt`, record done (use the resolved `run.ID`, not the ticket):

```go
	if r.Recorder != nil {
		dur := int(done.CompletedAt.Sub(done.StartedAt).Seconds())
		r.Recorder.RecordRunDone(r.ctx, run.ID, statusFor(done.OK), done.CompletedAt, prURLFromDone(done), dur)
	}
```

> `prURLFromDone` — `JobDone` (run.go:14) has no PR field today. If the completion summary already resolves a PR URL elsewhere, pass it through; otherwise pass `""` for Phase 1 and let NEX-514 (PR-from-signal) fill it later. Define `func prURLFromDone(JobDone) string { return "" }` as a seam.

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/dispatch/ -run 'TestReserveRecords|TestJobDoneRecords' -v`
Expected: PASS. Then run the full package: `go test ./runtime/dispatch/` — Expected: PASS (no regressions).

- [ ] **Step 5: Commit**

```bash
git add runtime/dispatch/runs_recorder.go runtime/dispatch/runner.go runtime/dispatch/brief.go runtime/dispatch/runs_recorder_test.go
git commit -m "feat(dispatch): record run start/done via RunsRecorder

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Tag activity frames with `run_id`

**Files:**
- Modify: `nexus/observability/types.go:22-28` (`Frame`)
- Modify: the agentfunnel frame-construction site (turn/presence emit)
- Test: `nexus/observability/types_test.go` (add a case)

- [ ] **Step 1: Write the failing test**

```go
// nexus/observability/types_test.go (add)
func TestFrameCarriesRunID(t *testing.T) {
	f := Frame{Kind: FrameTurn, Aspect: "anvil", RunID: "run-z"}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"run_id":"run-z"`) {
		t.Fatalf("run_id not serialized: %s", b)
	}
	var back Frame
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.RunID != "run-z" {
		t.Fatalf("run_id round-trip = %q", back.RunID)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/observability/ -run TestFrameCarriesRunID -v`
Expected: FAIL — `RunID` field undefined.

- [ ] **Step 3: Add the field + populate at emit**

In `nexus/observability/types.go`:

```go
type Frame struct {
	Kind     FrameKind       `json:"kind"`
	Aspect   string          `json:"aspect"`
	Sequence int64           `json:"seq"`
	TS       time.Time       `json:"ts"`
	RunID    string          `json:"run_id,omitempty"` // dispatch run that emitted this frame
	Payload  json.RawMessage `json:"payload"`
}
```

Locate where the agentfunnel constructs `observability.Frame{...}` for turn/presence/chat (search `observability.Frame{` under `runtime/`). At each construction, set `RunID: os.Getenv("CW_DISPATCH_RUN_ID")`. If frames are built through a single helper, set it once there. The env var is already present in builder pods (jobspec). For non-dispatch aspects the var is empty → `omitempty` drops it (no behavior change).

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/observability/... && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/observability/types.go nexus/observability/types_test.go runtime/
git commit -m "feat(observability): tag activity frames with run_id

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: JSONL reader — `activity.history` source

**Files:**
- Create: `nexus/observability/jsonlsink/reader.go`
- Test: `nexus/observability/jsonlsink/reader_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nexus/observability/jsonlsink/reader_test.go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/observability/jsonlsink/ -run TestReadFramesByRunID -v`
Expected: FAIL — `NewReader` undefined.

- [ ] **Step 3: Implement the reader**

```go
// nexus/observability/jsonlsink/reader.go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/observability/jsonlsink/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add nexus/observability/jsonlsink/reader.go nexus/observability/jsonlsink/reader_test.go
git commit -m "feat(observability): JSONL reader for activity.history by run_id

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Frame kinds + payloads for the new RPCs

**Files:**
- Modify: `nexus/frames/frames.go` (Kind constants)
- Modify: `nexus/frames/payloads.go` (payload structs)
- Test: `nexus/frames/payloads_test.go` (round-trip)

- [ ] **Step 1: Write the failing test**

```go
// nexus/frames/payloads_test.go (add)
func TestRunGetResultRoundTrip(t *testing.T) {
	p := RunGetResultPayload{
		Run: RunPayload{RunID: "run-a", Ticket: "NEX-1", Status: "running"},
		Timeline: []TimelineItemPayload{
			{Kind: "chat", At: 1, Chat: &ChatItemPayload{MsgID: 5, From: "shadow", Content: "!dispatch ..."}},
			{Kind: "activity", At: 2, Activity: &ActivityItemPayload{Type: "turn", Text: "thinking"}},
		},
	}
	b, _ := json.Marshal(p)
	var back RunGetResultPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Run.RunID != "run-a" || len(back.Timeline) != 2 || back.Timeline[1].Activity.Type != "turn" {
		t.Fatalf("round-trip: %+v", back)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/frames/ -run TestRunGetResultRoundTrip -v`
Expected: FAIL — types undefined.

- [ ] **Step 3: Add kinds + payloads**

In `nexus/frames/frames.go`, alongside the existing `Kind` constants (e.g. `KindChatList`):

```go
	KindRunsList         Kind = "runs.list"
	KindRunsListResult   Kind = "runs.list.result"
	KindRunGet           Kind = "run.get"
	KindRunGetResult     Kind = "run.get.result"
	KindActivityHistory  Kind = "activity.history"
	KindActivityHistoryResult Kind = "activity.history.result"
	KindEnvHealth        Kind = "env.health"
	KindEnvHealthResult  Kind = "env.health.result"
	KindRunsUpdate       Kind = "runs.update" // push
```

In `nexus/frames/payloads.go`:

```go
type RunPayload struct {
	RunID        string `json:"run_id"`
	Ticket       string `json:"ticket"`
	Agent        string `json:"agent"`
	Thread       string `json:"thread"`
	Command      string `json:"command,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Status       string `json:"status"`
	StartedAt    int64  `json:"started_at"`             // unix ms
	CompletedAt  int64  `json:"completed_at,omitempty"` // unix ms
	PRURL        string `json:"pr_url,omitempty"`
	DurationSecs int    `json:"duration_secs,omitempty"`
	ParentRunID  string `json:"parent_run_id,omitempty"`
}

type RunsListPayload struct {
	Limit int `json:"limit,omitempty"`
}
type RunsListResultPayload struct {
	Runs []RunPayload `json:"runs"`
}

type RunGetPayload struct {
	RunID string `json:"run_id"`
}
type ChatItemPayload struct {
	MsgID   int64  `json:"msg_id"`
	From    string `json:"from"`
	Content string `json:"content"`
	ReplyTo int64  `json:"reply_to,omitempty"`
}
type ActivityItemPayload struct {
	Type  string `json:"type"` // turn | tool | thought | presence
	Text  string `json:"text,omitempty"`
	Tool  string `json:"tool,omitempty"`
	State string `json:"state,omitempty"`
}
type TimelineItemPayload struct {
	Kind     string               `json:"kind"` // chat | activity
	At       int64                `json:"at"`   // unix ms
	Chat     *ChatItemPayload     `json:"chat,omitempty"`
	Activity *ActivityItemPayload `json:"activity,omitempty"`
}
type RunGetResultPayload struct {
	Run      RunPayload            `json:"run"`
	Timeline []TimelineItemPayload `json:"timeline"`
	Partial  bool                  `json:"partial,omitempty"` // activity history incomplete
}

type ActivityHistoryPayload struct {
	RunID string `json:"run_id"`
	Limit int    `json:"limit,omitempty"`
}
type ActivityHistoryResultPayload struct {
	Items   []ActivityItemPayload `json:"items"`
	Partial bool                  `json:"partial,omitempty"`
}

type EnvComponentPayload struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}
type EnvPVCPayload struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}
type EnvHealthResultPayload struct {
	Components  []EnvComponentPayload `json:"components"`
	PodsRunning int                   `json:"pods_running"`
	PodsTotal   int                   `json:"pods_total"`
	PVCs        []EnvPVCPayload       `json:"pvcs"`
	LastDeploy  string                `json:"last_deploy,omitempty"`
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/frames/ -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/frames/frames.go nexus/frames/payloads.go nexus/frames/payloads_test.go
git commit -m "feat(frames): runs/activity/env-health RPC kinds + payloads

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: `runs.list` + `run.get` handlers + the timeline merge

**Files:**
- Create: `nexus/broker/runs_rpc.go`
- Modify: `nexus/broker/operator_frames.go:39-74` (register kinds)
- Modify: `nexus/broker/server.go` (`Config.RunsStore runs.Store`)
- Test: `nexus/broker/runs_rpc_test.go` (the merge, in isolation)

The merge is the one piece of real logic — unit-test it directly, independent of WS.

- [ ] **Step 1: Write the failing test (merge logic)**

```go
// nexus/broker/runs_rpc_test.go
package broker

import (
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func TestMergeTimelineOrdersByTimeChatBeforeActivityOnTie(t *testing.T) {
	msgs := []chat.Message{
		{ID: 1, From: "shadow", Content: "!dispatch", CreatedAt: time.UnixMilli(100)},
		{ID: 2, From: "anvil", Content: "done", CreatedAt: time.UnixMilli(300)},
	}
	acts := []observability.Frame{
		{Kind: observability.FrameTurn, Sequence: 1, TS: time.UnixMilli(100)}, // tie with msg 1
		{Kind: observability.FrameTurn, Sequence: 2, TS: time.UnixMilli(200)},
	}
	tl := mergeTimeline(msgs, acts)
	if len(tl) != 4 {
		t.Fatalf("len = %d", len(tl))
	}
	// tie at 100: chat before activity
	if tl[0].Kind != "chat" || tl[1].Kind != "activity" {
		t.Fatalf("tie-break wrong: %+v %+v", tl[0], tl[1])
	}
	if tl[2].At != 200 || tl[3].At != 300 {
		t.Fatalf("order wrong: %+v", tl)
	}
	_ = frames.TimelineItemPayload{}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestMergeTimeline -v`
Expected: FAIL — `mergeTimeline` undefined.

- [ ] **Step 3: Implement handlers + merge**

```go
// nexus/broker/runs_rpc.go
package broker

import (
	"sort"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
)

func runToPayload(r runs.Run) frames.RunPayload {
	p := frames.RunPayload{
		RunID: r.RunID, Ticket: r.Ticket, Agent: r.Agent, Thread: r.Thread,
		Command: r.Command, Repo: r.Repo, Status: string(r.Status),
		StartedAt: r.StartedAt.UnixMilli(), PRURL: r.PRURL,
		DurationSecs: r.DurationSecs, ParentRunID: r.ParentRunID,
	}
	if !r.CompletedAt.IsZero() {
		p.CompletedAt = r.CompletedAt.UnixMilli()
	}
	return p
}

func activityType(f observability.Frame) string {
	switch f.Kind {
	case observability.FrameTurn:
		return "turn"
	case observability.FramePresence:
		return "presence"
	case observability.FrameFilterDecision:
		return "thought"
	default:
		return string(f.Kind)
	}
}

// mergeTimeline interleaves chat messages and activity frames by time. Ties
// order chat before activity (the command precedes the work it triggers).
func mergeTimeline(msgs []chat.Message, acts []observability.Frame) []frames.TimelineItemPayload {
	out := make([]frames.TimelineItemPayload, 0, len(msgs)+len(acts))
	for _, m := range msgs {
		out = append(out, frames.TimelineItemPayload{
			Kind: "chat", At: m.CreatedAt.UnixMilli(),
			Chat: &frames.ChatItemPayload{MsgID: m.ID, From: m.From, Content: m.Content, ReplyTo: m.ReplyTo},
		})
	}
	for _, f := range acts {
		out = append(out, frames.TimelineItemPayload{
			Kind: "activity", At: f.TS.UnixMilli(),
			Activity: &frames.ActivityItemPayload{Type: activityType(f)},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].Kind == "chat" && out[j].Kind == "activity"
	})
	return out
}

func (c *wsConn) handleOperatorRunsList(env frames.Envelope) {
	store := c.broker.cfg.RunsStore
	if store == nil {
		c.operatorError(env, "runs store not configured")
		return
	}
	var p frames.RunsListPayload
	_ = frames.PayloadAs(env, &p)
	ctx, cancel := c.opCtx()
	defer cancel()
	rs, err := store.List(ctx, p.Limit)
	if err != nil {
		c.operatorError(env, "runs.list: "+err.Error())
		return
	}
	out := make([]frames.RunPayload, 0, len(rs))
	for _, r := range rs {
		out = append(out, runToPayload(r))
	}
	resp, _ := frames.NewResponse(frames.KindRunsListResult, env.ID, frames.RunsListResultPayload{Runs: out})
	c.send(resp)
}

func (c *wsConn) handleOperatorRunGet(env frames.Envelope) {
	store := c.broker.cfg.RunsStore
	if store == nil {
		c.operatorError(env, "runs store not configured")
		return
	}
	var p frames.RunGetPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "run.get: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	run, err := store.Get(ctx, p.RunID)
	if err != nil {
		// unknown / aged-out → empty timeline, not an error panel
		resp, _ := frames.NewResponse(frames.KindRunGetResult, env.ID, frames.RunGetResultPayload{
			Run: frames.RunPayload{RunID: p.RunID, Status: "unknown"},
		})
		c.send(resp)
		return
	}

	var msgs []chat.Message
	if cs := c.broker.cfg.ChatStore; cs != nil && run.DispatchMsgID > 0 {
		msgs, _ = cs.ListThread(ctx, run.DispatchMsgID, 0, 1000)
	}
	var acts []observability.Frame
	partial := false
	if c.broker.activityReader != nil {
		acts, err = c.broker.activityReader.ReadByRun(ctx, run.Agent, run.RunID, 2000)
		if err != nil {
			partial = true
		}
	}
	resp, _ := frames.NewResponse(frames.KindRunGetResult, env.ID, frames.RunGetResultPayload{
		Run:      runToPayload(run),
		Timeline: mergeTimeline(msgs, acts),
		Partial:  partial,
	})
	c.send(resp)
}

var _ = time.Now
```

Add to `Config` in `nexus/broker/server.go` (near `ChatStore chat.Store`):

```go
	// RunsStore powers the Watch run feed + run.get timeline.
	RunsStore runs.Store
```

Add an `activityReader *jsonlsink.Reader` field to the `Broker` struct, constructed in `New()` from the same root the sink uses (search where `jsonlsink.New(root,...)` is constructed; reuse that root: `b.activityReader = jsonlsink.NewReader(root)`).

Register in `dispatchOperatorFrame` (operator_frames.go:54, add cases):

```go
	case frames.KindRunsList:
		c.handleOperatorRunsList(env)
	case frames.KindRunGet:
		c.handleOperatorRunGet(env)
```

> `chat.Message` field names (`ID`, `From`, `Content`, `ReplyTo`, `CreatedAt`) — confirm against `nexus/chat/chat.go`; `ListThread(ctx, threadID, sinceID, limit)` is the existing method (interface line 74). If thread lookup needs the thread-root id rather than the dispatch msg id, pass `run.DispatchMsgID` (the dispatch post is the thread root).

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestMergeTimeline -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/runs_rpc.go nexus/broker/operator_frames.go nexus/broker/server.go nexus/broker/runs_rpc_test.go
git commit -m "feat(broker): runs.list + run.get with unified chat+activity timeline

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: `activity.history` handler

**Files:**
- Create: `nexus/broker/activity_rpc.go`
- Modify: `nexus/broker/operator_frames.go` (register `KindActivityHistory`)
- Test: covered via the reader (Task 4); add a thin handler test if the WS test harness exists, else rely on build + manual.

- [ ] **Step 1: Implement the handler**

```go
// nexus/broker/activity_rpc.go
package broker

import "github.com/CarriedWorldUniverse/nexus/nexus/frames"

func (c *wsConn) handleOperatorActivityHistory(env frames.Envelope) {
	if c.broker.activityReader == nil || c.broker.cfg.RunsStore == nil {
		c.operatorError(env, "activity history not configured")
		return
	}
	var p frames.ActivityHistoryPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "activity.history: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	run, err := c.broker.cfg.RunsStore.Get(ctx, p.RunID)
	if err != nil {
		c.operatorError(env, "activity.history: unknown run")
		return
	}
	acts, err := c.broker.activityReader.ReadByRun(ctx, run.Agent, p.RunID, p.Limit)
	partial := err != nil
	items := make([]frames.ActivityItemPayload, 0, len(acts))
	for _, f := range acts {
		items = append(items, frames.ActivityItemPayload{Type: activityType(f)})
	}
	resp, _ := frames.NewResponse(frames.KindActivityHistoryResult, env.ID,
		frames.ActivityHistoryResultPayload{Items: items, Partial: partial})
	c.send(resp)
}
```

Register in `dispatchOperatorFrame`:

```go
	case frames.KindActivityHistory:
		c.handleOperatorActivityHistory(env)
```

- [ ] **Step 2: Build**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/broker/`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/activity_rpc.go nexus/broker/operator_frames.go
git commit -m "feat(broker): activity.history RPC (JSONL backfill by run_id)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: Populate `Brief.DispatchMsgID` at the dispatch intercept

**Files:**
- Modify: `nexus/broker/dispatch_intercept.go` (`submitDispatch`) + `nexus/broker/chat_send.go` (the `!dispatch` store site ~line 53/67)

The runs row links to its command message via `dispatch_msg_id`. The `!dispatch` post is stored first (`ChatStore.Insert` returns the message id); pass that id into the Brief.

- [ ] **Step 1: Thread the id**

At the `!dispatch` store site in `chat_send.go`, capture the inserted message's `ID` and pass it through to `submitDispatch`. In `submitDispatch` (dispatch_intercept.go:15), set `b.DispatchMsgID = msgID` on the parsed `Brief` before `b.runner.Submit(ctx, b)`.

> Exact code depends on the current `submitDispatch` signature — add a `dispatchMsgID int64` parameter and set `brief.DispatchMsgID = dispatchMsgID`. The `Brief.DispatchMsgID` field was added in Task 2.

- [ ] **Step 2: Build + existing dispatch tests**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go test ./nexus/broker/ -run Dispatch`
Expected: clean + PASS.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/dispatch_intercept.go nexus/broker/chat_send.go
git commit -m "feat(broker): link runs row to its !dispatch command message

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Wire the runs adapter + `runs.update` push

**Files:**
- Create: `nexus/broker/runs_adapter.go` (adapts `runs.Store` → `dispatch.RunsRecorder`, broadcasts `runs.update`)
- Modify: `nexus/broker/server.go` (set `runner.Recorder`)
- Test: `nexus/broker/runs_adapter_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nexus/broker/runs_adapter_test.go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestAdapterRecords -v`
Expected: FAIL — `newRunsAdapter` undefined.

- [ ] **Step 3: Implement the adapter + push**

```go
// nexus/broker/runs_adapter.go
package broker

import (
	"context"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
)

// runsAdapter bridges the dispatch runner's RunsRecorder to the runs.Store and
// emits an onChange callback (used to push runs.update to operators).
type runsAdapter struct {
	store    runs.Store
	onChange func(runs.Run)
}

func newRunsAdapter(store runs.Store, onChange func(runs.Run)) *runsAdapter {
	return &runsAdapter{store: store, onChange: onChange}
}

func (a *runsAdapter) RecordRunStart(ctx context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64) {
	r := runs.Run{
		RunID: runID, Ticket: ticket, Agent: agent, Thread: thread, Repo: repo,
		Command: command, ParentRunID: parentRunID, DispatchMsgID: dispatchMsgID,
		Status: runs.StatusRunning, StartedAt: time.Now(),
	}
	_ = a.store.Insert(ctx, r)
	if a.onChange != nil {
		a.onChange(r)
	}
}

func (a *runsAdapter) RecordRunDone(ctx context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int) {
	_ = a.store.MarkDone(ctx, runID, runs.Status(status), completedAt, prURL, durationSecs)
	if a.onChange != nil {
		if r, err := a.store.Get(ctx, runID); err == nil {
			a.onChange(r)
		}
	}
}
```

In `server.go` `New()`, after the runner is constructed and when `cfg.RunsStore != nil`:

```go
	if cfg.RunsStore != nil {
		_ = cfg.RunsStore.Migrate(context.Background())
		b.runner.Recorder = newRunsAdapter(cfg.RunsStore, b.broadcastRunsUpdate)
	}
```

Add `broadcastRunsUpdate(r runs.Run)` mirroring `BroadcastObserveFrame` (observe.go:109) — fan a `KindRunsUpdate` frame (payload = `runToPayload(r)`) to all operator conns. For Phase 1, fan to every operator conn (no per-run subscription needed):

```go
func (b *Broker) broadcastRunsUpdate(r runs.Run) {
	b.opMu.RLock()
	targets := make([]*wsConn, 0, len(b.operators))
	for c := range b.operators {
		targets = append(targets, c)
	}
	b.opMu.RUnlock()
	frame, _ := frames.NewResponse(frames.KindRunsUpdate, "", runToPayload(r))
	for _, c := range targets {
		c.send(frame)
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestAdapterRecords -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/runs_adapter.go nexus/broker/server.go nexus/broker/runs_adapter_test.go
git commit -m "feat(broker): wire runs adapter into runner + runs.update push

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: `env.health` handler

**Files:**
- Create: `nexus/broker/env_health.go`
- Modify: `nexus/broker/operator_frames.go` (register `KindEnvHealth`)
- Test: `nexus/broker/env_health_test.go` (fake clientset, per `k8s_test.go` pattern)

The broker already builds an in-cluster k8s client for dispatch (`runtime/dispatch.NewInClusterK8s`). Reuse a `kubernetes.Interface` here. Define a narrow `K8sReader` interface so the test can inject `fake.NewSimpleClientset`.

- [ ] **Step 1: Write the failing test**

```go
// nexus/broker/env_health_test.go
package broker

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnvHealthSnapshot(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nexus-broker-x", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gemma-y", Namespace: "nexus"},
			Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "aspect-home-anvil", Namespace: "nexus"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	)
	h, err := envHealthSnapshot(context.Background(), cs, "nexus")
	if err != nil {
		t.Fatal(err)
	}
	if h.PodsTotal != 2 || h.PodsRunning != 1 {
		t.Fatalf("pods: %+v", h)
	}
	if len(h.PVCs) != 1 || h.PVCs[0].Status != "Bound" {
		t.Fatalf("pvcs: %+v", h)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestEnvHealthSnapshot -v`
Expected: FAIL — `envHealthSnapshot` undefined.

- [ ] **Step 3: Implement**

```go
// nexus/broker/env_health.go
package broker

import (
	"context"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

func envHealthSnapshot(ctx context.Context, cs kubernetes.Interface, ns string) (frames.EnvHealthResultPayload, error) {
	var out frames.EnvHealthResultPayload
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}
	wanted := map[string]string{"nexus-broker": "broker", "sqld": "sqld", "gemma": "gemma"}
	seen := map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		out.PodsTotal++
		running := p.Status.Phase == corev1.PodRunning
		if running {
			out.PodsRunning++
		}
		for prefix, name := range wanted {
			if strings.HasPrefix(p.Name, prefix) && !seen[name] {
				seen[name] = true
				detail := string(p.Status.Phase)
				out.Components = append(out.Components, frames.EnvComponentPayload{
					Name: name, Kind: "pod", Healthy: running, Detail: detail,
				})
			}
		}
	}
	for name := range wanted {
		if !seen[wanted[name]] {
			out.Components = append(out.Components, frames.EnvComponentPayload{
				Name: wanted[name], Kind: "pod", Healthy: false, Detail: "not found",
			})
		}
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range pvcs.Items {
			pv := &pvcs.Items[i]
			out.PVCs = append(out.PVCs, frames.EnvPVCPayload{Name: pv.Name, Status: string(pv.Status.Phase)})
		}
	}
	return out, nil
}

func (c *wsConn) handleOperatorEnvHealth(env frames.Envelope) {
	cs := c.broker.k8sReader
	if cs == nil {
		c.operatorError(env, "env.health not available (no in-cluster client)")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	snap, err := envHealthSnapshot(ctx, cs, c.broker.k8sNamespace)
	if err != nil {
		c.operatorError(env, "env.health: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindEnvHealthResult, env.ID, snap)
	c.send(resp)
}
```

Add `k8sReader kubernetes.Interface` and `k8sNamespace string` to the `Broker` struct; set them in `New()` from the same in-cluster client the dispatch runner uses (guard nil for non-k8s boots). Register in `dispatchOperatorFrame`:

```go
	case frames.KindEnvHealth:
		c.handleOperatorEnvHealth(env)
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestEnvHealthSnapshot -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/env_health.go nexus/broker/operator_frames.go nexus/broker/server.go nexus/broker/env_health_test.go
git commit -m "feat(broker): env.health RPC (pods/PVCs/components snapshot)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: Broker boot — construct RunsStore + apply RBAC

**Files:**
- Modify: wherever the broker `Config` is assembled at boot (search `broker.Config{` in `cmd/` / `runtime/`) — open the sqld DB the chat store uses and pass `runs.NewSQLStore(db)` as `RunsStore`.
- Modify: `carriedworld-cloud/hosting/services/nexus-broker-dispatch-rbac.yaml` (on dMon, direct-commit repo).

- [ ] **Step 1: Wire RunsStore at boot**

At the broker boot site that constructs `broker.Config`, after the chat store DB is opened, add:

```go
	cfg.RunsStore = runs.NewSQLStore(chatDB) // same *sql.DB as the chat store
```

(`chatDB` is whatever `*sql.DB` is passed to `chat.NewSQLStore`. The runs `Migrate` runs in `server.New()` per Task 9.)

- [ ] **Step 2: Build**

Run: `cd /Users/jacinta/Source/nexus && go build ./...`
Expected: clean.

- [ ] **Step 3: Add the RBAC verbs (on dMon)**

In `carriedworld-cloud/hosting/services/nexus-broker-dispatch-rbac.yaml`, add to the `nexus-broker-dispatch` Role rules:

```yaml
  - { apiGroups: ["apps"], resources: ["deployments"], verbs: ["get","list"] }
```

Confirm `pods` already has `get,list` (it does: `["pods","pods/log"] -> get,list,watch`) and `persistentvolumeclaims` has `get,list` (added 2026-06-08). Apply: `sudo kubectl apply -f <that file>` and commit it (`git -c user.email=nexus@carriedworld.com -c user.name=shadow commit`).

- [ ] **Step 4: Commit (Go side)**

```bash
git add -A
git commit -m "feat(broker): construct RunsStore at boot

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 12: Frontend — API client additions

**Files:**
- Modify: `nexus/broker/static/dashboard/js/api.js`

- [ ] **Step 1: Add the calls (after `fetchMessages`, ~line 111)**

```javascript
export function runsList(limit = 100) {
  return rpc('runs.list', { limit }).then((p) => p.runs || []);
}

export function runGet(runId) {
  return rpc('run.get', { run_id: runId }).then((p) => ({
    run: p.run || {},
    timeline: p.timeline || [],
    partial: !!p.partial,
  }));
}

export function activityHistory(runId, limit = 1000) {
  return rpc('activity.history', { run_id: runId, limit }).then((p) => ({
    items: p.items || [],
    partial: !!p.partial,
  }));
}

export function envHealth() {
  return rpc('env.health', {}).then((p) => p || {});
}
```

> `comms.js`'s `pushKindFor` maps `subscribe.X` → `X.deliver`-style push kinds. For the `runs.update` push (an unsolicited server frame, not a `subscribe.*`), use the lower-level `onPushKind('runs.update', handler)` that `ObserveView.js` already imports from `comms.js` (line 2: `import { send, onPushKind } from '../comms.js'`). Confirm `onPushKind` is exported; if not, add a thin `onPushKind(kind, handler)` to `comms.js` that registers `handler` in `state.subs` under that literal push kind.

- [ ] **Step 2: Verify (lint/load)**

Open the dashboard in dev mode (broker `--dashboard-dir`), hard-refresh, confirm no console error importing `api.js`. (No unit harness for api.js; manual.)

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/api.js
git commit -m "feat(dashboard): api client for runs/activity/env-health

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 13: Frontend — three-area shell

**Files:**
- Modify: `nexus/broker/static/dashboard/js/app.js:19-52`

- [ ] **Step 1: Add the Watch route + three-area nav**

Extend `getRoute()` and `RouteView()`:

```javascript
function getRoute() {
  const hash = window.location.hash;
  if (hash.startsWith('#/watch')) return 'watch';
  if (hash.startsWith('#/converse')) return 'converse';
  if (hash.startsWith('#/configure')) return 'configure';
  // existing routes preserved for now:
  if (hash.startsWith('#/chat')) return 'chat';
  if (hash.startsWith('#/agents')) return 'agents';
  if (hash.startsWith('#/settings')) return 'settings';
  if (hash === '#/' || hash === '' || hash.startsWith('#/feed')) return 'watch'; // new default
  return 'watch';
}

function RouteView({ route }) {
  switch (route) {
    case 'watch':     return html`<${WatchView} />`;
    case 'converse':  return html`<${Placeholder} title="Converse" note="Coming in Phase 3" />`;
    case 'configure': return html`<${SettingsView} />`; // existing settings, re-homed in Phase 4
    // legacy still reachable:
    case 'chat':      return html`<${Chat} />`;
    case 'agents':    return html`<${ObserveView} />`;
    case 'settings':  return html`<${SettingsView} />`;
    default:          return html`<${WatchView} />`;
  }
}
```

Import `WatchView` and `Placeholder` at the top of `app.js`. Render the top nav with the three primary areas (Converse / Watch / Configure) as links to `#/converse`, `#/watch`, `#/configure` — reuse the existing nav-rendering markup, replacing the old multi-view list. **No always-visible strips** — the env-health/team are panel toggles inside WatchView, not shell chrome.

- [ ] **Step 2: Verify**

Hard-refresh; `#/watch` is the default and renders WatchView; the three nav items route correctly.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/app.js
git commit -m "feat(dashboard): three-area shell (Converse/Watch/Configure), Watch default

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 14: Frontend — WatchView (run feed + unified timeline)

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/WatchView.js`
- Create: `nexus/broker/static/dashboard/css/watch.css` (link it from `index.html`)

Reuse `MessageBubble` for chat items; render activity items as compact lines. Live updates via `onPushKind('runs.update', …)` for the feed and `subscribe('subscribe.observe', {aspect}, …)` for the open run's timeline.

- [ ] **Step 1: Implement WatchView**

```javascript
// nexus/broker/static/dashboard/js/views/WatchView.js
const { html, useEffect, useState, useRef } = window.__preact;

import { runsList, runGet } from '../api.js';
import { onPushKind, subscribe } from '../comms.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { TeamPanel } from './panels/TeamPanel.js';
import { EnvHealthPanel } from './panels/EnvHealthPanel.js';

function statusDot(status) {
  const map = { running: 'live', complete: 'ok', failed: 'bad', queued: 'wait', cancelled: 'muted' };
  return html`<span class=${`run-dot run-dot-${map[status] || 'muted'}`} title=${status}></span>`;
}

function RunFeed({ runs, selected, onSelect }) {
  return html`
    <div class="run-feed">
      ${runs.map((r) => html`
        <button
          key=${r.run_id}
          class=${`run-card ${selected === r.run_id ? 'run-card-active' : ''}`}
          onClick=${() => onSelect(r.run_id)}>
          ${statusDot(r.status)}
          <span class="run-agent">${r.agent}</span>
          <span class="run-ticket">${r.ticket}</span>
          <span class="run-cmd">${(r.command || '').slice(0, 60)}</span>
        </button>`)}
      ${runs.length === 0 ? html`<div class="run-feed-empty">No runs yet.</div>` : null}
    </div>`;
}

function ActivityLine({ item }) {
  return html`<div class=${`activity-line activity-${item.activity.type}`}>
    <span class="activity-kind">${item.activity.type}</span>
    <span class="activity-text">${item.activity.text || item.activity.tool || ''}</span>
  </div>`;
}

function Timeline({ items }) {
  if (!items.length) return html`<div class="timeline-empty">Select a run.</div>`;
  return html`<div class="timeline">
    ${items.map((it, i) => it.kind === 'chat'
      ? html`<${MessageBubble} key=${'c' + i} message=${{
          id: it.chat.msg_id, from: it.chat.from, content: it.chat.content,
          reply_to: it.chat.reply_to, created_at: '',
        }} />`
      : html`<${ActivityLine} key=${'a' + i} item=${it} />`)}
  </div>`;
}

export function WatchView() {
  const [runs, setRuns] = useState([]);
  const [selected, setSelected] = useState(null);
  const [items, setItems] = useState([]);
  const [showTeam, setShowTeam] = useState(false);
  const [showEnv, setShowEnv] = useState(false);
  const selRef = useRef(null);
  selRef.current = selected;

  // initial + live feed
  useEffect(() => {
    runsList().then((rs) => {
      setRuns(rs);
      if (rs.length && !selRef.current) setSelected(rs[0].run_id);
    });
    const off = onPushKind('runs.update', (frame) => {
      const r = frame.payload || frame;
      setRuns((prev) => {
        const idx = prev.findIndex((x) => x.run_id === r.run_id);
        if (idx >= 0) { const next = prev.slice(); next[idx] = r; return next; }
        return [r, ...prev];
      });
    });
    return off;
  }, []);

  // load selected run timeline + live observe
  useEffect(() => {
    if (!selected) return;
    let unsub = null;
    runGet(selected).then((res) => {
      setItems(res.timeline);
      const agent = res.run.agent;
      if (agent) {
        unsub = subscribe('subscribe.observe', { aspect: agent }, (frame) => {
          setItems((prev) => [...prev, {
            kind: 'activity', at: Date.now(),
            activity: { type: frame.kind, text: (frame.payload && frame.payload.text) || '' },
          }]);
        });
      }
    });
    return () => { if (unsub) unsub(); };
  }, [selected]);

  return html`
    <div class="watch">
      <div class="watch-toolbar">
        <button class=${showTeam ? 'panel-toggle on' : 'panel-toggle'} onClick=${() => setShowTeam(v => !v)}>Team</button>
        <button class=${showEnv ? 'panel-toggle on' : 'panel-toggle'} onClick=${() => setShowEnv(v => !v)}>Env</button>
      </div>
      <div class="watch-body">
        ${showTeam ? html`<${TeamPanel} onClose=${() => setShowTeam(false)} />` : null}
        <${RunFeed} runs=${runs} selected=${selected} onSelect=${setSelected} />
        <${Timeline} items=${items} />
        ${showEnv ? html`<${EnvHealthPanel} onClose=${() => setShowEnv(false)} />` : null}
      </div>
    </div>`;
}
```

> Confirm `MessageBubble`'s prop name (`message` vs `msg`) against `components/MessageBubble.js` and match it. Confirm `onPushKind` is exported by `comms.js` (Task 12 note).

- [ ] **Step 2: Add CSS**

Create `css/watch.css` with `.watch`, `.run-feed`, `.run-card`, `.run-dot-*`, `.timeline`, `.activity-line`, `.panel-toggle`, `.watch-panel` styles using `var(--…)` tokens from `css/tokens.css`. Link it in `index.html` alongside the other stylesheets.

- [ ] **Step 3: Verify**

Dev mode, `#/watch`: the feed lists runs from `runs.list`; selecting a run shows the merged timeline; dispatching a new run (via shadow/CLI) makes a card appear/update live; Team/Env toggles open/close panels; nothing is always-visible.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/WatchView.js nexus/broker/static/dashboard/css/watch.css nexus/broker/static/dashboard/index.html
git commit -m "feat(dashboard): WatchView — run feed + unified timeline

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 15: Frontend — Team + Env-health panels

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/panels/TeamPanel.js`
- Create: `nexus/broker/static/dashboard/js/views/panels/EnvHealthPanel.js`

- [ ] **Step 1: TeamPanel (reuse roster signal)**

```javascript
// js/views/panels/TeamPanel.js
const { html } = window.__preact;
import { agents } from '../../state.js';

export function TeamPanel({ onClose }) {
  const list = agents.value || [];
  return html`
    <aside class="watch-panel team-panel">
      <header>Team <button class="panel-close" onClick=${onClose}>×</button></header>
      <ul>
        ${list.map((a) => {
          const name = typeof a === 'string' ? a : a.id;
          const state = (typeof a === 'object' && a.state) || 'idle';
          return html`<li key=${name}><span class=${`team-state team-${state}`}></span>${name}<span class="team-statelbl">${state}</span></li>`;
        })}
      </ul>
    </aside>`;
}
```

> `agents` is the existing roster signal (imported in `ObserveView.js` from `../state.js`). Confirm the per-agent shape (string vs `{id,state}`) and adapt the `state` read.

- [ ] **Step 2: EnvHealthPanel (poll while open)**

```javascript
// js/views/panels/EnvHealthPanel.js
const { html, useEffect, useState } = window.__preact;
import { envHealth } from '../../api.js';

export function EnvHealthPanel({ onClose }) {
  const [h, setH] = useState(null);
  useEffect(() => {
    let alive = true;
    const tick = () => envHealth().then((d) => { if (alive) setH(d); }).catch(() => {});
    tick();
    const iv = setInterval(tick, 15000);
    return () => { alive = false; clearInterval(iv); };
  }, []);
  if (!h) return html`<aside class="watch-panel env-panel"><header>Env <button class="panel-close" onClick=${onClose}>×</button></header><div>loading…</div></aside>`;
  return html`
    <aside class="watch-panel env-panel">
      <header>Env <button class="panel-close" onClick=${onClose}>×</button></header>
      <div class="env-components">
        ${(h.components || []).map((c) => html`<div key=${c.name} class=${`env-comp ${c.healthy ? 'ok' : 'bad'}`}>${c.name}<small>${c.detail || ''}</small></div>`)}
      </div>
      <div class="env-pods">pods ${h.pods_running}/${h.pods_total}</div>
      <ul class="env-pvcs">
        ${(h.pvcs || []).map((p) => html`<li key=${p.name} class=${p.status === 'Bound' ? 'ok' : 'bad'}>${p.name} — ${p.status}</li>`)}
      </ul>
      ${h.last_deploy ? html`<div class="env-deploy">deploy: ${h.last_deploy}</div>` : null}
    </aside>`;
}
```

- [ ] **Step 3: Verify**

Toggle Team and Env panels in `#/watch`: Team lists the roster with live state; Env shows broker/sqld/gemma health, pod count, PVC bound status; both close cleanly; neither is always-visible.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/panels/TeamPanel.js nexus/broker/static/dashboard/js/views/panels/EnvHealthPanel.js
git commit -m "feat(dashboard): Team + Env-health openable panels

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 16: Integration verification (full build + deploy + dogfood)

**Files:** none (verification).

- [ ] **Step 1: Full build + test + vet**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/... ./runtime/... && go test ./nexus/runs/ ./nexus/broker/ ./nexus/observability/... ./runtime/dispatch/`
Expected: all PASS, clean vet.

- [ ] **Step 2: Deploy to dMon**

Per `project_per_agent_home_live` memory: rebuild broker (`deploy/broker/build.sh`) + worker image (`deploy/worker/build.sh`, for the run_id frame tag), apply the RBAC, `kubectl rollout restart deploy/nexus-broker`. Confirm broker boots + shadow reconnects.

- [ ] **Step 3: Dogfood**

Dispatch a throwaway run (`!dispatch anvil%codex-cli repo=… ticket=ui-dogfood …`). In the dashboard `#/watch`: the run appears in the feed live; its timeline shows the `!dispatch` command (chat) interleaved with the builder's activity frames; on completion the card flips to complete; reload the page and the full historical timeline still renders (the core gap, fixed). Toggle Team + Env panels.

- [ ] **Step 4: Final commit / push the branch**

```bash
git push -u origin design/work-ui-phase1
```

---

## Self-Review notes (for the executor)

- **Spec coverage:** runs read-model (T1, T2, T9, T11) · run_id tagging (T3) · activity backfill (T4, T7) · runs.list/run.get unified timeline (T5, T6) · env.health (T5, T10) · runs.update push (T5, T9) · shell + Watch + panels (T13–T15) · RBAC (T11). All spec sections map to a task.
- **Type consistency:** `frames.RunPayload`/`TimelineItemPayload`/`ActivityItemPayload`/`EnvHealthResultPayload` defined in T5 and consumed unchanged in T6/T7/T10/T12/T14. `runs.Run`/`runs.Store` defined in T1, consumed in T6/T9/T11. `dispatch.RunsRecorder` defined in T2, implemented in T9.
- **Known seams to confirm against live code (flagged inline):** `chat.Message` field names + `ListThread` signature (T6); the agentfunnel `observability.Frame{}` construction site (T3); `comms.js onPushKind` export (T12); `MessageBubble` prop name (T14); the broker boot site that builds `broker.Config` + the chat `*sql.DB` (T11); `JobDone` PR field (T2, seam left for NEX-514). These are confirmations, not gaps — the contract in each task is explicit.
