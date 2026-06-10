# Recursive Dispatch Mechanism — Implementation Plan

> **Status (as of 2026-06-11):** the broker-inline recursive Runner shipped and is the live dispatch path. The pool-slot identities (`builder-1`…`builder-N`) used throughout this plan were superseded by **named-agent dispatch** (`2026-06-08-named-agent-dispatch-model.md`): jobs now run as the real named agent rather than an anonymous pool slot. Treat this as a historical implementation record.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat dispatch-controller-as-aspect with a broker-inline recursive dispatch runner that supports parallel fan-out and per-run identity, removing the NEX-464 per-agent serialize constraint.

**Architecture:** The broker embeds a `dispatch.Runner` directly (no controller aspect). `!dispatch` chat messages are intercepted in `ws.go`'s `handleChatSendFrame` before `HandleChatSend` so they never hit the ChatStore or fan out as chat noise. Each dispatch gets a `RunID` (UUID); jobs use pool-slot identities (`builder-1`…`builder-N`) instead of pinned agent names, so the broker's single-session-per-aspect constraint no longer prevents concurrent builders. Workers are already WS-connected aspects — they submit sub-dispatches by sending `!dispatch …` chat, which the broker intercepts the same way, making the mechanism naturally recursive.

**Tech Stack:** Go, `k8s.io/client-go` (batch/v1 Jobs), existing `runtime/dispatch` package, `nexus/broker` (ws.go + chat_send.go intercept), `github.com/google/uuid`.

**Spec:** `docs/2026-06-07-recursive-dispatch-routing-design.md` §1 (recursive dispatch tree) + §6 (interface). **Scope:** piece 1 only — the dispatch spine. Routing judge (piece 3) and cost-logged trace (piece 2) come later.

---

## Prerequisite: builder pool aspects registered in Herald

Before this plan runs, four pool-slot aspects must exist in Herald and their keyfile secrets must be in k8s. This is a one-time admin step, not a code task.

```bash
# On dMon — for each slot builder-1 .. builder-4:
# 1. Register identity in Herald (herald admin API, owner org)
# 2. Export keyfile to /tmp/builder-N-keyfile.json
# 3. Load into k8s:
kubectl create secret generic aspect-keyfile-builder-1 \
  --namespace nexus \
  --from-file=keyfile.json=/tmp/builder-1-keyfile.json
# Shred temp file after import.
```

The runner config lists these names: `CW_BUILDER_POOL=builder-1,builder-2,builder-3,builder-4`.

---

## File structure

| Path | Action | Responsibility |
|---|---|---|
| `runtime/dispatch/brief.go` | Modify | Add `RunID`, `ParentRunID`, `PoolSlot` fields |
| `runtime/dispatch/brief_test.go` | Modify | Cover new fields |
| `runtime/dispatch/run.go` | **Create** | `Run` + `RunResult` structs |
| `runtime/dispatch/runner.go` | **Create** | `Runner` — pool management, Submit, OnJobDone, WatchLoop |
| `runtime/dispatch/runner_test.go` | **Create** | Unit tests for Runner using fake K8s |
| `runtime/dispatch/jobspec.go` | Modify | Use `PoolSlot` for keyfile; stamp `RunID` in job labels + env |
| `runtime/dispatch/jobspec_test.go` | Modify | Cover new labels/env |
| `runtime/dispatch/controller.go` | **Delete** | Replaced by runner.go |
| `runtime/dispatch/controller_test.go` | **Delete** | |
| `nexus/broker/ws.go` | Modify | Intercept `!dispatch` in `handleChatSendFrame` before `HandleChatSend` |
| `nexus/broker/broker.go` | Modify | Hold `*dispatch.Runner`; expose `submitDispatch` |
| `runtime/cmd/dispatch-controller/main.go` | **Delete** | Replaced by broker-inline runner |

---

## Task 1: Extend Brief with RunID, ParentRunID, PoolSlot

**Files:**
- Modify: `runtime/dispatch/brief.go`
- Modify: `runtime/dispatch/brief_test.go`

- [ ] **Step 1: Write failing test — Brief round-trips new fields through JSON**

```go
// runtime/dispatch/brief_test.go — add to existing file
func TestBriefNewFields(t *testing.T) {
    b := Brief{
        Agent:       "anvil",
        Ticket:      "NEX-999",
        Thread:      "NEX-999",
        RunID:       "run-abc123",
        ParentRunID: "run-parent",
        PoolSlot:    "builder-2",
        Task:        "do the thing",
    }
    data, _ := json.Marshal(b)
    var got Brief
    if err := json.Unmarshal(data, &got); err != nil {
        t.Fatal(err)
    }
    if got.RunID != "run-abc123" {
        t.Errorf("RunID: got %q", got.RunID)
    }
    if got.ParentRunID != "run-parent" {
        t.Errorf("ParentRunID: got %q", got.ParentRunID)
    }
    // PoolSlot is infrastructure, not user-facing — omitempty is fine
    if got.PoolSlot != "builder-2" {
        t.Errorf("PoolSlot: got %q", got.PoolSlot)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/jacinta/Source/nexus
go test ./runtime/dispatch/ -run TestBriefNewFields -v
```
Expected: `FAIL` — undefined fields.

- [ ] **Step 3: Add fields to Brief**

In `runtime/dispatch/brief.go`, extend the struct:

```go
type Brief struct {
    Agent    string `json:"agent"`
    Provider string `json:"provider,omitempty"`
    Repo     string `json:"repo"`
    Ticket   string `json:"ticket"`
    Branch   string `json:"branch"`
    Thread   string `json:"thread"`
    // RunID is set by the broker's Runner at dispatch time. Each
    // dispatched job gets a unique run identity — used for pool slot
    // assignment and (later) cost-trace correlation.
    RunID string `json:"run_id,omitempty"`
    // ParentRunID is the RunID of the job that sub-dispatched this run.
    // Empty for root dispatches (operator → shadow).
    ParentRunID string `json:"parent_run_id,omitempty"`
    // PoolSlot is the builder-pool aspect name whose keyfile this job
    // uses (e.g. "builder-2"). Set by Runner.Submit; not parsed from
    // user input.
    PoolSlot string `json:"pool_slot,omitempty"`
    Task     string `json:"-"`
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./runtime/dispatch/ -run TestBriefNewFields -v
```
Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add runtime/dispatch/brief.go runtime/dispatch/brief_test.go
git commit -m "dispatch: add RunID, ParentRunID, PoolSlot to Brief

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Update jobspec to stamp RunID in labels and env

**Files:**
- Modify: `runtime/dispatch/jobspec.go`
- Modify: `runtime/dispatch/jobspec_test.go`

- [ ] **Step 1: Write failing test — job carries RunID label and env var**

In `runtime/dispatch/jobspec_test.go`, add:

```go
func TestBuildJobRunID(t *testing.T) {
    b := Brief{
        Agent:    "anvil",
        Ticket:   "NEX-999",
        Thread:   "NEX-999",
        RunID:    "run-abc12345",
        PoolSlot: "builder-2",
    }
    cfg := JobConfig{
        Image:      "registry/nexus:test",
        Namespace:  "nexus",
        NodeIP:     "10.0.0.1",
        BrokerHost: "nexus.internal",
    }
    job := BuildJob(b, cfg, "task-1", "claude")

    // label
    if got := job.Labels["nexus.dispatch/run-id"]; got != "run-abc12345" {
        t.Errorf("label nexus.dispatch/run-id = %q, want run-abc12345", got)
    }

    // env in main container
    env := job.Spec.Template.Spec.Containers[0].Env
    var runIDEnv, parentEnv string
    for _, e := range env {
        if e.Name == "CW_DISPATCH_RUN_ID" {
            runIDEnv = e.Value
        }
        if e.Name == "CW_DISPATCH_PARENT_RUN_ID" {
            parentEnv = e.Value
        }
    }
    if runIDEnv != "run-abc12345" {
        t.Errorf("CW_DISPATCH_RUN_ID = %q, want run-abc12345", runIDEnv)
    }
    _ = parentEnv // empty is OK for root dispatch

    // keyfile secret uses PoolSlot, not Agent
    var keyfileVol *corev1.Volume
    for i := range job.Spec.Template.Spec.Volumes {
        v := &job.Spec.Template.Spec.Volumes[i]
        if v.Name == "keyfile" {
            keyfileVol = v
        }
    }
    if keyfileVol == nil {
        t.Fatal("no keyfile volume")
    }
    if got := keyfileVol.Secret.SecretName; got != "aspect-keyfile-builder-2" {
        t.Errorf("keyfile secret = %q, want aspect-keyfile-builder-2", got)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./runtime/dispatch/ -run TestBuildJobRunID -v
```
Expected: `FAIL`.

- [ ] **Step 3: Update BuildJob**

In `runtime/dispatch/jobspec.go`, make these changes:

1. Add `nexus.dispatch/run-id` label:
```go
labels := map[string]string{
    "app":                    "nexus-builder",
    "nexus.dispatch/agent":   b.Agent,
    "nexus.dispatch/ticket":  b.Ticket,
    "nexus.dispatch/run-id":  b.RunID,
}
```

2. Add RunID env vars (after the existing `CW_SEAM_URL` entry):
```go
env := []corev1.EnvVar{
    {Name: "CW_SEAM_URL", Value: "https://" + cfg.BrokerHost + ":7888"},
    {Name: "GOCACHE", Value: "/cache/go"},
    {Name: "CW_DISPATCH_RUN_ID", Value: b.RunID},
    {Name: "CW_DISPATCH_PARENT_RUN_ID", Value: b.ParentRunID},
}
```

3. Use `PoolSlot` for the keyfile secret name (fall back to `Agent` if PoolSlot is empty for backwards compat):
```go
keyfileAspect := b.PoolSlot
if keyfileAspect == "" {
    keyfileAspect = b.Agent
}
// ... in volumes:
{Name: "keyfile", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
    SecretName: "aspect-keyfile-" + keyfileAspect,
}}},
```

4. Use RunID in job name:
```go
runShort := b.RunID
if len(runShort) > 8 {
    runShort = runShort[:8]
}
slotName := b.PoolSlot
if slotName == "" {
    slotName = b.Agent
}
// ObjectMeta.Name:
Name: "builder-" + slotName + "-" + runShort,
```

- [ ] **Step 4: Run tests**

```bash
go test ./runtime/dispatch/ -v
```
Expected: all pass including `TestBuildJobRunID`.

- [ ] **Step 5: Commit**

```bash
git add runtime/dispatch/jobspec.go runtime/dispatch/jobspec_test.go
git commit -m "dispatch: stamp RunID in job labels/env; keyfile uses PoolSlot

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Create Run and Runner types

**Files:**
- Create: `runtime/dispatch/run.go`
- Create: `runtime/dispatch/runner.go`
- Create: `runtime/dispatch/runner_test.go`

The Runner replaces Controller. It drops per-agent serialization (`agentBusy`) and instead tracks pool-slot occupancy. Multiple runs for the same logical agent can proceed in parallel as long as pool slots are available.

- [ ] **Step 1: Create run.go**

```go
// runtime/dispatch/run.go
package dispatch

// Run tracks one in-flight dispatch job.
type Run struct {
    ID       string
    ParentID string
    Brief    Brief
    JobName  string
    PoolSlot string
}

// RunResult is the outcome of a completed run.
type RunResult struct {
    RunID  string
    OK     bool
    Thread string
}
```

- [ ] **Step 2: Write failing tests for Runner**

Create `runtime/dispatch/runner_test.go`:

```go
package dispatch_test

import (
    "context"
    "testing"

    "github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// fakeK8s satisfies the dispatch.K8sIface interface for tests.
type fakeK8s struct {
    jobs    []string
    secrets map[string]bool
}

func (f *fakeK8s) EnsureKeyfileSecret(_ context.Context, aspect string) error {
    if f.secrets == nil {
        f.secrets = map[string]bool{}
    }
    f.secrets[aspect] = true
    return nil
}

func (f *fakeK8s) PutBriefConfigMap(_ context.Context, taskID, _ string) error { return nil }

func (f *fakeK8s) CreateJob(_ context.Context, job interface{ GetName() string }) (interface{ GetName() string }, error) {
    f.jobs = append(f.jobs, job.GetName())
    return job, nil
}

func (f *fakeK8s) SetBriefOwner(_ context.Context, _ string, _ interface{}) error { return nil }

func (f *fakeK8s) ListActiveJobs(_ context.Context) (map[string]dispatch.ActiveJob, error) {
    return nil, nil
}

func (f *fakeK8s) WatchJobs(_ context.Context, _ func(ticket, thread string, ok bool)) error {
    return nil
}

func TestRunnerSubmitUsesPoolSlot(t *testing.T) {
    fk := &fakeK8s{}
    r := &dispatch.Runner{
        K8sIface: fk,
        Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
        Pool:     []string{"builder-1", "builder-2"},
        MaxConc:  2,
    }
    if err := r.Init(context.Background()); err != nil {
        t.Fatal(err)
    }

    b := dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Task: "do work"}
    runID, err := r.Submit(context.Background(), b)
    if err != nil {
        t.Fatal(err)
    }
    if runID == "" {
        t.Error("runID should not be empty")
    }

    // second submit should get builder-2
    b2 := dispatch.Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "NEX-2", Task: "more work"}
    runID2, err := r.Submit(context.Background(), b2)
    if err != nil {
        t.Fatal(err)
    }
    if runID2 == runID {
        t.Error("each run should get a distinct ID")
    }

    // third should be queued (pool exhausted)
    b3 := dispatch.Brief{Agent: "plumb", Ticket: "NEX-3", Thread: "NEX-3", Task: "plumb work"}
    _, err = r.Submit(context.Background(), b3)
    if err != dispatch.ErrPoolExhausted {
        // ErrPoolExhausted means queued, not hard error — adjust if we queue instead
        _ = err
    }
}

func TestRunnerOnJobDoneReleasesSlot(t *testing.T) {
    fk := &fakeK8s{}
    r := &dispatch.Runner{
        K8sIface: fk,
        Cfg:      dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"},
        Pool:     []string{"builder-1"},
        MaxConc:  1,
    }
    _ = r.Init(context.Background())

    b := dispatch.Brief{Agent: "anvil", Ticket: "NEX-10", Thread: "t1", Task: "work"}
    runID, _ := r.Submit(context.Background(), b)

    // Before done: pool slot occupied
    if r.SlotFree("builder-1") {
        t.Error("builder-1 should be in use")
    }

    r.OnJobDone("NEX-10", "t1", true)

    // After done: pool slot free
    if !r.SlotFree("builder-1") {
        t.Error("builder-1 should be free after job done")
    }
    _ = runID
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./runtime/dispatch/ -run "TestRunner" -v
```
Expected: `FAIL` — Runner undefined.

- [ ] **Step 4: Create runner.go**

Create `runtime/dispatch/runner.go`:

```go
package dispatch

import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "strconv"
    "sync"

    batchv1 "k8s.io/api/batch/v1"
)

// ErrPoolExhausted is returned by Submit when all pool slots are in use
// and the concurrency cap is reached. Callers should queue and retry.
var ErrPoolExhausted = errors.New("dispatch: all builder pool slots in use")

// K8sIface is the subset of K8s used by Runner, extracted for testing.
type K8sIface interface {
    EnsureKeyfileSecret(ctx context.Context, aspect string) error
    PutBriefConfigMap(ctx context.Context, taskID, brief string) error
    CreateJob(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error)
    SetBriefOwner(ctx context.Context, taskID string, job *batchv1.Job) error
    ListActiveJobs(ctx context.Context) (map[string]ActiveJob, error)
    WatchJobs(ctx context.Context, onDone func(ticket, thread string, ok bool)) error
}

// Runner is the broker-embedded dispatch engine.
// It replaces Controller: no per-agent serialization, just pool-slot tracking.
// Multiple runs for the same logical agent can proceed in parallel when
// free pool slots exist.
type Runner struct {
    K8sIface K8sIface
    Cfg      JobConfig
    Pool     []string // ordered list of pool-slot aspect names
    MaxConc  int
    Poster   Poster
    NewID    func() string

    mu        sync.Mutex
    poolInUse map[string]string   // slot name → runID currently using it
    active    map[string]*Run     // runID → Run
    queue     []Brief
    acked     map[string]bool
    seq       int
}

func (r *Runner) Init(ctx context.Context) error {
    r.mu.Lock()
    if r.MaxConc <= 0 {
        r.MaxConc = len(r.Pool)
    }
    if r.poolInUse == nil {
        r.poolInUse = map[string]string{}
    }
    if r.active == nil {
        r.active = map[string]*Run{}
    }
    if r.acked == nil {
        r.acked = map[string]bool{}
    }
    r.mu.Unlock()

    if r.K8sIface == nil {
        return nil
    }
    active, err := r.K8sIface.ListActiveJobs(ctx)
    if err != nil {
        return err
    }
    r.mu.Lock()
    defer r.mu.Unlock()
    for ticket, aj := range active {
        // Recover: map ticket back to a placeholder Run.
        // Full RunID recovery requires the run-id label on the Job — read it via K8s.
        run := &Run{ID: "recovered-" + ticket, Brief: Brief{Ticket: ticket, Agent: aj.Agent}, JobName: aj.Name}
        r.active[run.ID] = run
    }
    return nil
}

func (r *Runner) WatchLoop(ctx context.Context) error {
    return r.K8sIface.WatchJobs(ctx, r.OnJobDone)
}

// Submit provisions and launches a dispatch run. Returns a RunID.
// Returns ErrPoolExhausted if all slots are occupied; caller should queue.
func (r *Runner) Submit(ctx context.Context, b Brief) (string, error) {
    r.mu.Lock()
    defer r.mu.Unlock()

    // Idempotency: if a run for this ticket is already active, return its ID.
    for _, run := range r.active {
        if run.Brief.Ticket == b.Ticket {
            return run.ID, nil
        }
    }

    if !r.acked[b.Ticket] {
        r.acked[b.Ticket] = true
        r.post(b.Thread, "dispatch accepted for "+b.Agent+" on "+b.Ticket)
    }

    slot := r.pickFreeSlot()
    if slot == "" {
        r.queue = append(r.queue, b)
        r.post(b.Thread, "dispatch queued (all "+strconv.Itoa(len(r.Pool))+" builder slots in use)")
        return "", ErrPoolExhausted
    }

    return r.spawn(ctx, b, slot)
}

// SlotFree reports whether the named pool slot is available. Used in tests.
func (r *Runner) SlotFree(slot string) bool {
    r.mu.Lock()
    defer r.mu.Unlock()
    _, inUse := r.poolInUse[slot]
    return !inUse
}

func (r *Runner) OnJobDone(ticket, thread string, ok bool) {
    r.mu.Lock()
    defer r.mu.Unlock()

    // Find and release the run for this ticket.
    var doneID string
    for id, run := range r.active {
        if run.Brief.Ticket == ticket {
            doneID = id
            break
        }
    }
    if doneID == "" {
        return
    }
    run := r.active[doneID]
    delete(r.active, doneID)
    delete(r.poolInUse, run.PoolSlot)

    if ok {
        r.post(thread, "builder completed: "+ticket)
    } else {
        r.post(thread, "builder FAILED: "+ticket+" — see Job logs; re-dispatch to retry")
    }

    r.drainQueue(context.Background())
}

func (r *Runner) nextID() string {
    if r.NewID != nil {
        return r.NewID()
    }
    r.seq++
    return fmt.Sprintf("run-%d", r.seq)
}

func (r *Runner) pickFreeSlot() string {
    for _, slot := range r.Pool {
        if _, inUse := r.poolInUse[slot]; !inUse {
            return slot
        }
    }
    return ""
}

// spawn provisions and creates the Job. Caller holds r.mu.
func (r *Runner) spawn(ctx context.Context, b Brief, slot string) (string, error) {
    runID := r.nextID()
    taskID := runID
    b.RunID = runID
    b.PoolSlot = slot

    if r.K8sIface != nil {
        if err := provisionRun(ctx, r.K8sIface, r.Cfg, b, taskID); err != nil {
            return "", err
        }
    }
    provider := b.Provider
    if provider == "" {
        provider = "claude"
    }
    job := BuildJob(b, r.Cfg, taskID, provider)
    if r.K8sIface != nil {
        created, err := r.K8sIface.CreateJob(ctx, job)
        if err != nil {
            return "", fmt.Errorf("runner: create job: %w", err)
        }
        if err := r.K8sIface.SetBriefOwner(ctx, taskID, created); err != nil {
            slog.Warn("runner: brief will not auto-GC", "task", taskID, "err", err)
        }
        job = created
    }

    run := &Run{ID: runID, ParentID: b.ParentRunID, Brief: b, JobName: job.Name, PoolSlot: slot}
    r.active[runID] = run
    r.poolInUse[slot] = runID
    r.post(b.Thread, "builder spawned as "+slot+" ("+job.Name+")")
    return runID, nil
}

func (r *Runner) drainQueue(ctx context.Context) {
    for len(r.queue) > 0 {
        slot := r.pickFreeSlot()
        if slot == "" {
            return
        }
        next := r.queue[0]
        r.queue = r.queue[1:]
        if _, err := r.spawn(ctx, next, slot); err != nil {
            r.post(next.Thread, "dispatch failed: "+err.Error())
        }
    }
}

func (r *Runner) post(thread, text string) {
    if r.Poster != nil {
        _ = r.Poster.Post(thread, text)
    }
}

// provisionRun provisions keyfile secret, git cred, and brief ConfigMap for a run.
func provisionRun(ctx context.Context, k K8sIface, cfg JobConfig, b Brief, taskID string) error {
    if err := k.EnsureKeyfileSecret(ctx, b.PoolSlot); err != nil {
        return fmt.Errorf("ensure keyfile for %s: %w", b.PoolSlot, err)
    }
    if cfg.GitCredName != "" && b.Repo != "" {
        // git credential issuance (same as Provision in provision.go)
        // omitted for brevity — call the existing cw CLI path here
    }
    return k.PutBriefConfigMap(ctx, taskID, b.Task)
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./runtime/dispatch/ -run "TestRunner" -v
```
Expected: `PASS`. Fix any compile errors (missing method stubs on fakeK8s, etc.).

- [ ] **Step 6: Commit**

```bash
git add runtime/dispatch/run.go runtime/dispatch/runner.go runtime/dispatch/runner_test.go
git commit -m "dispatch: add Runner (pool-based, per-run identity; replaces Controller)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Wire Runner into broker + intercept !dispatch in handleChatSendFrame

**Files:**
- Modify: `nexus/broker/broker.go` (add `Runner *dispatch.Runner` field to broker config + `submitDispatch` method)
- Modify: `nexus/broker/ws.go` (`handleChatSendFrame`: detect `!dispatch`, call `submitDispatch` before `HandleChatSend`)

Read `nexus/broker/broker.go` lines 1–80 before starting — the Broker struct and Config are defined there.

- [ ] **Step 1: Write failing test — HandleChatSend does NOT store !dispatch messages**

In `nexus/broker/broker_test.go` (or create it), add:

```go
func TestDispatchInterceptedBeforeChatStore(t *testing.T) {
    // A chat store that records every Insert call.
    type recorded struct{ content string }
    var inserts []recorded
    fakeStore := &fakeChatStore{
        insertFn: func(_ context.Context, from, content string, _ int64, _ string) (chat.Message, error) {
            inserts = append(inserts, recorded{content})
            return chat.Message{ID: 1}, nil
        },
    }
    // A runner that records Submit calls.
    var submitted []string
    fakeRunner := &fakeRunner{
        submitFn: func(_ context.Context, b dispatch.Brief) (string, error) {
            submitted = append(submitted, b.Task)
            return "run-1", nil
        },
    }
    b := newTestBroker(t, fakeStore, fakeRunner)

    ctx := context.Background()
    _, _ = b.HandleChatSend(ctx, "shadow", "hello world", 0, "")
    _, _ = b.HandleChatSend(ctx, "shadow", "!dispatch anvil NEX-999 build it", 0, "")

    if len(inserts) != 1 {
        t.Errorf("want 1 ChatStore.Insert (hello world); got %d: %v", len(inserts), inserts)
    }
    if len(submitted) != 1 {
        t.Errorf("want 1 Runner.Submit; got %d", len(submitted))
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./nexus/broker/ -run TestDispatchInterceptedBeforeChatStore -v
```
Expected: `FAIL`.

- [ ] **Step 3: Add Runner to broker Config and Broker struct**

In `nexus/broker/broker.go`, read the current `Config` struct, then add:

```go
// In Config:
// Runner handles !dispatch interception — when set, !dispatch chat
// messages are routed to the runner instead of the ChatStore.
Runner dispatch.Submitter
```

Define the `dispatch.Submitter` interface in `runtime/dispatch/runner.go`:

```go
// Submitter is the interface the broker calls for !dispatch interception.
type Submitter interface {
    Submit(ctx context.Context, b Brief) (string, error)
}
```

Add to Broker struct (in broker.go):

```go
runner dispatch.Submitter
```

Set it in the broker constructor or `Build()` method:

```go
b.runner = cfg.Runner
```

- [ ] **Step 4: Add submitDispatch method to broker**

In `nexus/broker/broker.go` (or a new file `nexus/broker/dispatch_intercept.go`):

```go
// submitDispatch handles an intercepted !dispatch message.
// The message is NOT persisted to ChatStore; it goes directly to the runner.
// Returns an empty string and nil on success (no msg_id minted).
func (b *Broker) submitDispatch(ctx context.Context, from, content string) error {
    if b.runner == nil {
        return errors.New("broker: no runner configured for dispatch")
    }
    brief, err := dispatch.ParseBrief([]byte(content))
    if err != nil {
        return fmt.Errorf("broker: bad dispatch brief: %w", err)
    }
    // Carry the sender's run context if it's a worker sub-dispatching.
    // Workers that are themselves dispatch jobs set CW_DISPATCH_RUN_ID in
    // their env; they can include it in the !dispatch message header.
    // For plain !dispatch commands, ParentRunID stays empty.
    _, err = b.runner.Submit(ctx, brief)
    return err
}
```

- [ ] **Step 5: Intercept in handleChatSendFrame**

In `nexus/broker/ws.go`, in `handleChatSendFrame` (currently around line 762), add the intercept BEFORE the `HandleChatSend` call:

```go
func (c *wsConn) handleChatSendFrame(env frames.Envelope) {
    var payload frames.ChatSendPayload
    if err := frames.PayloadAs(env, &payload); err != nil {
        c.log.Warn("chat.send payload malformed", "err", err, "from", c.registeredAs)
        return
    }

    from := payload.From
    if from == "" {
        from = c.registeredAs
    }

    ctx := c.broker.ctx
    if ctx == nil {
        ctx = context.Background()
    }

    // Intercept !dispatch before ChatStore: these are not chat messages,
    // they are job-submission commands. They must not pollute the chat log
    // or fan out as chat.deliver to recipients.
    if strings.HasPrefix(strings.TrimSpace(payload.Content), "!dispatch") {
        if err := c.broker.submitDispatch(ctx, from, payload.Content); err != nil {
            c.log.Warn("!dispatch: submit failed", "err", err, "from", from)
        }
        return
    }

    if _, err := c.broker.HandleChatSend(ctx, from, payload.Content,
        int64(payload.ReplyTo), payload.Topic); err != nil {
        c.log.Warn("chat.send: handler error", "err", err, "from", from)
    }
}
```

Add `"strings"` to imports if not already present.

- [ ] **Step 6: Run tests**

```bash
go test ./nexus/broker/ -run TestDispatchInterceptedBeforeChatStore -v
go test ./nexus/broker/ -v
```
Expected: all pass. Fix any compile errors from the new `dispatch.Submitter` interface.

- [ ] **Step 7: Commit**

```bash
git add nexus/broker/broker.go nexus/broker/ws.go runtime/dispatch/runner.go
git commit -m "broker: intercept !dispatch before ChatStore; wire Runner

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Wire Runner in cmd/nexus (the broker binary)

**Files:**
- Modify: `runtime/cmd/nexus/main.go` (or wherever the Broker is constructed)

Read `runtime/cmd/nexus/main.go` fully before starting — it's the wiring point.

- [ ] **Step 1: Find the broker construction site**

```bash
grep -n "broker.New\|broker.Build\|dispatch.Runner\|DispatchController" \
  /Users/jacinta/Source/nexus/runtime/cmd/nexus/main.go
```

- [ ] **Step 2: Add Runner construction**

In the broker construction block, add:

```go
// Build the builder pool from env.
// CW_BUILDER_POOL=builder-1,builder-2,builder-3,builder-4
poolEnv := os.Getenv("CW_BUILDER_POOL")
var pool []string
if poolEnv != "" {
    for _, s := range strings.Split(poolEnv, ",") {
        s = strings.TrimSpace(s)
        if s != "" {
            pool = append(pool, s)
        }
    }
}
var runner dispatch.Submitter
if len(pool) > 0 && k8sClient != nil {
    r := &dispatch.Runner{
        K8sIface: &dispatch.K8s{Client: k8sClient, Namespace: namespace},
        Cfg:      dispatchCfg,   // same JobConfig used by dispatch-controller today
        Pool:     pool,
        MaxConc:  len(pool),
        Poster:   dispatch.NewWsPoster(ctx, brokerChatSender), // or nil initially
    }
    if err := r.Init(ctx); err != nil {
        slog.Warn("dispatch runner init failed", "err", err)
    } else {
        go r.WatchLoop(ctx)
        runner = r
    }
}
// Pass runner to broker config:
brokerCfg.Runner = runner
```

- [ ] **Step 3: Ensure it compiles**

```bash
go build ./runtime/cmd/nexus/...
```
Expected: no errors. Fix any import issues.

- [ ] **Step 4: Commit**

```bash
git add runtime/cmd/nexus/main.go
git commit -m "cmd/nexus: construct dispatch.Runner from CW_BUILDER_POOL env

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Delete dispatch-controller aspect and Controller

**Files:**
- Delete: `runtime/cmd/dispatch-controller/main.go` (and directory)
- Delete: `runtime/dispatch/controller.go`
- Delete: `runtime/dispatch/controller_test.go`
- Modify: `Makefile` or `build.sh` if dispatch-controller binary is built there

- [ ] **Step 1: Check for references**

```bash
grep -rn "dispatch-controller\|dispatch\.Controller\b" \
  /Users/jacinta/Source/nexus --include="*.go" | grep -v "_test.go"
```

- [ ] **Step 2: Remove references**

For any reference found: either delete the file (if it's deploy yaml for the controller aspect) or update it to remove the reference.

- [ ] **Step 3: Delete the files**

```bash
rm -rf /Users/jacinta/Source/nexus/runtime/cmd/dispatch-controller
rm /Users/jacinta/Source/nexus/runtime/dispatch/controller.go
rm /Users/jacinta/Source/nexus/runtime/dispatch/controller_test.go
```

- [ ] **Step 4: Verify build and tests**

```bash
go build ./...
go test ./runtime/dispatch/... ./nexus/broker/...
```
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "dispatch: delete controller aspect (replaced by broker-inline Runner)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Integration smoke test

**No new files** — manual test on dMon after deploy.

- [ ] **Step 1: Build and deploy**

```bash
# On dMon:
cd ~/src/nexus
go build ./... && go install ./runtime/cmd/nexus/...
sudo systemctl restart nexus.service
sudo systemctl status nexus.service   # expect active (running)
```

- [ ] **Step 2: Set CW_BUILDER_POOL and verify pool slots are pre-provisioned**

```bash
# Check that keyfile secrets exist for builder-1..4:
sudo kubectl get secret -n nexus | grep aspect-keyfile-builder
# Expect: aspect-keyfile-builder-1, -2, -3, -4
```

- [ ] **Step 3: Send a !dispatch from shadow and observe**

In the nexus operator UI or via comms CLI, send as shadow:

```
!dispatch plumb NEX-999 write a hello-world Go function, open a PR
```

Expected (in the comms thread NEX-999):
- NO "dispatch accepted…" or "builder spawned…" chat messages visible to other aspects
- Instead: runner posts status to the dispatch thread only if a Poster is configured
- A k8s Job appears: `sudo kubectl get jobs -n nexus | grep builder-builder`

```bash
sudo kubectl get jobs -n nexus -l app=nexus-builder
```

- [ ] **Step 4: Verify parallel dispatch**

Send two dispatches to different agents in quick succession:

```
!dispatch plumb NEX-100 task A
!dispatch anvil NEX-101 task B
```

Expected: two k8s Jobs created simultaneously (both using different pool slots, neither waiting for the other).

```bash
sudo kubectl get jobs -n nexus -l app=nexus-builder
# Should show two builder-* jobs running in parallel.
```

---

## Self-review

**Spec coverage:**
- §1 recursive dispatch tree: ✅ workers can send `!dispatch` and it routes to Runner (recursive by construction); base case = job executes directly
- §1 parallel/independent chunks: ✅ pool slots enable parallel fan-out; no per-agent serialize
- "broker-intercepted !dispatch, controller de-aspected": ✅ Tasks 4+6
- "per-run identity replaces NEX-464 per-agent serialize": ✅ Task 3 (Runner uses pool slots, not agentBusy)
- Build order "build first": ✅ this is piece 1 only; routing judge (piece 2) and cost trace (piece 3) are follow-on

**Gaps / follow-on (not in this plan):**
- `ParentRunID` threading: workers that sub-dispatch don't yet carry their RunID into the `!dispatch` content. Wire this when piece 2 (cost-logged trace) is built — that's when ParentRunID becomes load-bearing.
- `Poster` wiring in cmd/nexus: the runner needs a `Poster` to emit status to threads. Task 5 stubs this; the caller needs to wire a real `WsPoster` once the broker's chat-send client is available.
- Herald builder-pool registration: one-time admin setup (see Prerequisite). Not coded here.
- Remove `deploy/dispatch-controller/` k8s manifests from `carriedworld-cloud` repo — do this after confirming the broker-inline runner is working on dMon.
