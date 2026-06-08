# Work UI Phase 2 (Control Actions) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator act on the dispatch-native world from the Watch UI — dispatch a new run, cancel/stop a run (graceful + force), reply into a run's thread, and configure an agent — reusing existing paths wherever possible.

**Architecture:** Two new backend pieces only — `run.cancel` (+ `K8s.DeleteJob`, graceful via a `SIGTERM` grace period / force via grace=0, both freeing the agent through NEX-528) and a per-agent `dispatch-enabled` flag (store + admin endpoint + `!dispatch` gating). Dispatch-from-UI posts a `!dispatch` chat message; reply uses `aspect.say`; fuller agent config reuses the existing admin endpoints. New WS RPCs register in `dispatchOperatorFrame` following the Phase 1 `runs.list`/`run.get` pattern.

**Tech Stack:** Go (broker `nexus/broker`, dispatch `runtime/dispatch`), Preact + htm dashboard (`nexus/broker/static/dashboard`, go:embed, no build), WS frames (`nexus/frames`).

**Spec:** `docs/2026-06-09-work-ui-phase2-control-design.md`. **Branch:** `design/work-ui-phase2`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

**Ticket 2a — Cancel (Go + JS):**
- Modify `runtime/dispatch/k8s.go` — add `DeleteJob(ctx, name, gracePeriodSecs)`.
- Modify `nexus/runs/runs.go` — guard `MarkDone` so a terminal status isn't overwritten (cancel-vs-emitJobDeleted race) + a `cancelled` status helper.
- Modify `nexus/frames/frames.go` + `payloads.go` — `KindRunCancel`/`KindRunCancelResult` + payloads.
- Create `nexus/broker/run_cancel_rpc.go` — the `run.cancel` handler (resolve Job by `run-id` label, delete with grace, mark run cancelled); register in `operator_frames.go`.
- Modify `nexus/broker/static/dashboard/js/views/WatchView.js` — Stop/Force buttons + confirm.
- Modify `nexus/broker/static/dashboard/js/api.js` — `runCancel`.

**Ticket 2b — Dispatch-compose + Reply (JS, + one Go test):**
- Modify `runtime/dispatch/brief_test.go` — round-trip the composed `!dispatch` line through `ParseBrief`.
- Create `nexus/broker/static/dashboard/js/views/panels/DispatchComposePanel.js`.
- Modify `WatchView.js` — mount the compose panel + a timeline reply box.
- Modify `js/api.js` — `dispatchCompose` (post `!dispatch` via chat.send) + `replyToThread` (`aspect.say`).

**Ticket 2c — Fuller agent config + dispatch-enabled flag (Go + JS):**
- Modify the aspects store + create `nexus/broker/admin_dispatch_enabled.go` — `GET/PUT /api/admin/aspects/{name}/dispatch-enabled` + roster surfacing.
- Modify `nexus/broker/dispatch_intercept.go` — gate `submitDispatch` on the flag.
- Create `nexus/broker/static/dashboard/js/views/panels/AgentConfigPanel.js` — per-agent editor reusing admin endpoints.
- Modify `js/views/panels/TeamPanel.js` — Configure affordance + dispatch-enabled toggle.
- Modify `js/api.js` — per-agent config GET/PUT wrappers + `setDispatchEnabled`.

---

# TICKET 2a — Cancel

## Task 1: `K8s.DeleteJob`

**Files:**
- Modify: `runtime/dispatch/k8s.go`
- Test: `runtime/dispatch/k8s_test.go`

- [ ] **Step 1: Write the failing test**

```go
// runtime/dispatch/k8s_test.go (add)
func TestDeleteJobGracefulAndForce(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	ctx := context.Background()
	mk := func(name string) {
		_, _ = k.Client.BatchV1().Jobs("nexus").Create(ctx,
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nexus"}}, metav1.CreateOptions{})
	}
	mk("builder-anvil-run-1")
	if err := k.DeleteJob(ctx, "builder-anvil-run-1", int64p(30)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Client.BatchV1().Jobs("nexus").Get(ctx, "builder-anvil-run-1", metav1.GetOptions{}); err == nil {
		t.Fatal("graceful delete: job should be gone")
	}
	mk("builder-anvil-run-2")
	if err := k.DeleteJob(ctx, "builder-anvil-run-2", int64p(0)); err != nil {
		t.Fatal(err)
	}
	// deleting a missing job is not an error (idempotent)
	if err := k.DeleteJob(ctx, "does-not-exist", int64p(0)); err != nil {
		t.Fatalf("delete missing job should be nil, got %v", err)
	}
}
```

> `int64p` may already exist (jobspec.go uses `int32p`). If not, add `func int64p(v int64) *int64 { return &v }` in the test file or k8s.go.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/dispatch/ -run TestDeleteJob -v`
Expected: FAIL — `DeleteJob` undefined.

- [ ] **Step 3: Implement `DeleteJob`**

```go
// runtime/dispatch/k8s.go (add; needs apierrors which is already imported)
// DeleteJob deletes a builder Job by name with the given grace period (seconds).
// PropagationPolicy=Foreground so the pod is torn down with the Job. A non-zero
// grace lets the pod catch SIGTERM (graceful cancel); 0 forces SIGKILL. Missing
// Job is not an error (idempotent — it may have completed/TTL'd already).
func (k *K8s) DeleteJob(ctx context.Context, name string, gracePeriodSecs *int64) error {
	fg := metav1.DeletePropagationForeground
	err := k.Client.BatchV1().Jobs(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: gracePeriodSecs,
		PropagationPolicy:  &fg,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/dispatch/ -run TestDeleteJob -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add runtime/dispatch/k8s.go runtime/dispatch/k8s_test.go
git commit -m "feat(dispatch): K8s.DeleteJob with grace period (graceful/force cancel)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Guard `runs.MarkDone` against overwriting a terminal status

**Files:**
- Modify: `nexus/runs/runs.go`
- Test: `nexus/runs/runs_test.go`

Cancel marks the run `cancelled`; the async NEX-528 `emitJobDeleted` then fires `RecordRunDone(…, failed)`. First-writer-wins: `MarkDone` must not overwrite a row that's already terminal.

- [ ] **Step 1: Write the failing test**

```go
// nexus/runs/runs_test.go (add)
func TestMarkDoneDoesNotOverwriteTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Insert(ctx, Run{RunID: "run-c", Ticket: "NEX-1", Agent: "anvil", Status: StatusRunning, StartedAt: time.UnixMilli(1)})
	if err := s.MarkDone(ctx, "run-c", StatusCancelled, time.UnixMilli(2), "", 0); err != nil {
		t.Fatal(err)
	}
	// a later failed-mark (from emitJobDeleted) must NOT overwrite cancelled
	if err := s.MarkDone(ctx, "run-c", StatusFailed, time.UnixMilli(3), "", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "run-c")
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled (terminal not overwritten)", got.Status)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/runs/ -run TestMarkDoneDoesNotOverwriteTerminal -v`
Expected: FAIL — status overwritten to `failed`.

- [ ] **Step 3: Add the `WHERE status='running'` guard**

In `nexus/runs/runs.go` `MarkDone`, change the UPDATE to only fire while running:

```go
func (s *SQLStore) MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE runs SET status = ?, completed_at = ?, pr_url = ?, duration_secs = ?
		WHERE run_id = ? AND status = ?`,
		string(status), completedAt.UnixMilli(), prURL, durationSecs, runID, string(StatusRunning))
	if err != nil {
		return fmt.Errorf("runs.MarkDone: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/runs/ -v`
Expected: PASS (new test + the existing `TestInsertThenMarkDone` still passes — its run is `running` when marked).

- [ ] **Step 5: Commit**

```bash
git add nexus/runs/runs.go nexus/runs/runs_test.go
git commit -m "fix(runs): MarkDone first-writer-wins (don't overwrite a terminal status)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `run.cancel` frames + handler

**Files:**
- Modify: `nexus/frames/frames.go`, `nexus/frames/payloads.go`
- Create: `nexus/broker/run_cancel_rpc.go`
- Modify: `nexus/broker/operator_frames.go` (register)
- Test: `nexus/broker/run_cancel_rpc_test.go`

- [ ] **Step 1: Add kinds + payloads**

In `nexus/frames/frames.go`:

```go
	KindRunCancel       Kind = "run.cancel"
	KindRunCancelResult Kind = "run.cancel.result"
```

In `nexus/frames/payloads.go`:

```go
type RunCancelPayload struct {
	RunID string `json:"run_id"`
	Force bool   `json:"force,omitempty"`
}
type RunCancelResultPayload struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}
```

- [ ] **Step 2: Write the failing test (Job resolution + grace selection)**

```go
// nexus/broker/run_cancel_rpc_test.go
package broker

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCancelResolvesJobByRunIDLabelAndGrace(t *testing.T) {
	cs := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "builder-anvil-run-7", Namespace: "nexus",
			Labels: map[string]string{"app": "nexus-builder", "nexus.dispatch/run-id": "run-7"},
		},
	})
	// graceful → non-zero grace
	name, grace, err := resolveCancelTarget(context.Background(), cs, "nexus", "run-7", false)
	if err != nil || name != "builder-anvil-run-7" {
		t.Fatalf("resolve: name=%q err=%v", name, err)
	}
	if grace == nil || *grace == 0 {
		t.Fatalf("graceful grace = %v, want non-zero", grace)
	}
	// force → grace 0
	_, grace0, _ := resolveCancelTarget(context.Background(), cs, "nexus", "run-7", true)
	if grace0 == nil || *grace0 != 0 {
		t.Fatalf("force grace = %v, want 0", grace0)
	}
	// unknown run → not found
	if _, _, err := resolveCancelTarget(context.Background(), cs, "nexus", "run-none", false); err == nil {
		t.Fatal("unknown run should error")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestCancelResolves -v`
Expected: FAIL — `resolveCancelTarget` undefined.

- [ ] **Step 4: Implement the handler + resolver**

```go
// nexus/broker/run_cancel_rpc.go
package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const cancelGraceSecs int64 = 30

// resolveCancelTarget finds the builder Job for a run_id (by label) and returns
// its name + the grace period to use (0 for force, cancelGraceSecs otherwise).
func resolveCancelTarget(ctx context.Context, cs kubernetes.Interface, ns, runID string, force bool) (string, *int64, error) {
	jl, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "nexus.dispatch/run-id=" + runID,
	})
	if err != nil {
		return "", nil, err
	}
	if len(jl.Items) == 0 {
		return "", nil, fmt.Errorf("no active job for run %s", runID)
	}
	grace := cancelGraceSecs
	if force {
		grace = 0
	}
	return jl.Items[0].Name, &grace, nil
}

func (c *wsConn) handleOperatorRunCancel(env frames.Envelope) {
	if c.broker.k8sReader == nil {
		c.operatorError(env, "run.cancel unavailable (no in-cluster client)")
		return
	}
	var p frames.RunCancelPayload
	if err := frames.PayloadAs(env, &p); err != nil || p.RunID == "" {
		c.operatorError(env, "run.cancel: run_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()

	name, grace, err := resolveCancelTarget(ctx, c.broker.k8sReader, c.broker.k8sNamespace, p.RunID, p.Force)
	if err != nil {
		// Already finished/aged-out → mark cancelled best-effort, report ok.
		if c.broker.cfg.RunsStore != nil {
			_ = c.broker.cfg.RunsStore.MarkDone(ctx, p.RunID, runs.StatusCancelled, time.Now(), "", 0)
		}
		resp, _ := frames.NewResponse(frames.KindRunCancelResult, env.ID, frames.RunCancelResultPayload{OK: true, Message: "run already ended"})
		c.send(resp)
		return
	}
	// Mark cancelled first so the async emitJobDeleted (RecordRunDone failed)
	// is a no-op (MarkDone is first-writer-wins, Task 2).
	if c.broker.cfg.RunsStore != nil {
		_ = c.broker.cfg.RunsStore.MarkDone(ctx, p.RunID, runs.StatusCancelled, time.Now(), "", 0)
	}
	if err := c.broker.dispatchK8s.DeleteJob(ctx, name, grace); err != nil {
		c.operatorError(env, "run.cancel: delete job: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindRunCancelResult, env.ID, frames.RunCancelResultPayload{OK: true, Message: "cancelled"})
	c.send(resp)
}
```

> `c.broker.dispatchK8s` — the broker needs a handle to the `*dispatch.K8s` (with `DeleteJob`). The broker already builds the in-cluster client for the runner; expose it as a `dispatchK8s *dispatch.K8s` field on `Broker`, set in `New()` next to `k8sReader`. If only a `kubernetes.Interface` is held, wrap it: `&dispatch.K8s{Client: b.k8sReader, Namespace: b.k8sNamespace}`.

Register in `dispatchOperatorFrame` (operator_frames.go):

```go
	case frames.KindRunCancel:
		c.handleOperatorRunCancel(env)
```

- [ ] **Step 5: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestCancelResolves -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 6: Commit**

```bash
git add nexus/frames/frames.go nexus/frames/payloads.go nexus/broker/run_cancel_rpc.go nexus/broker/operator_frames.go nexus/broker/server.go nexus/broker/run_cancel_rpc_test.go
git commit -m "feat(broker): run.cancel RPC (graceful/force) -> DeleteJob + mark cancelled

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Cancel UI — Stop / Force on the run card

**Files:**
- Modify: `nexus/broker/static/dashboard/js/api.js`, `js/views/WatchView.js`

- [ ] **Step 1: Add the api wrapper**

```javascript
// js/api.js (add)
export function runCancel(runId, force = false) {
  return rpc('run.cancel', { run_id: runId, force }).then((p) => p || { ok: false });
}
```

- [ ] **Step 2: Add Stop/Force to WatchView**

In `WatchView.js`, import `runCancel`, and in the timeline header (or run card) for an **active** run (`run.status === 'running'`) render:

```javascript
function CancelControls({ run, onCancelled }) {
  if (!run || run.status !== 'running') return null;
  const stop = (force) => {
    const msg = force ? `Force-kill ${run.ticket}? In-flight work is lost.` : `Stop ${run.ticket}?`;
    if (!window.confirm(msg)) return;
    runCancel(run.run_id, force).then(() => onCancelled && onCancelled());
  };
  return html`
    <span class="run-cancel">
      <button class="btn-stop" onClick=${() => stop(false)}>Stop</button>
      <button class="btn-force" onClick=${() => stop(true)}>Force</button>
    </span>`;
}
```

Render `<${CancelControls} run=${selectedRun} onCancelled=${() => {}} />` in the timeline header; the run flips to `cancelled` via the existing `runs.update` push (no manual refresh). Add `.run-cancel`, `.btn-stop`, `.btn-force` to `css/watch.css`.

- [ ] **Step 3: Verify (dev mode)**

Dispatch a run, open `#/watch`, click Stop → confirm → the card flips to `cancelled` and the agent frees; Force on another → immediate. No console errors.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/api.js nexus/broker/static/dashboard/js/views/WatchView.js nexus/broker/static/dashboard/css/watch.css
git commit -m "feat(dashboard): Stop/Force cancel controls on the run timeline

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

# TICKET 2b — Dispatch-compose + Reply

## Task 5: Round-trip the composed `!dispatch` line

**Files:**
- Modify: `runtime/dispatch/brief_test.go` (or wherever `ParseBrief` is tested)

Pin that the UI's composed line parses to the right Brief, so the form + parser can't drift.

- [ ] **Step 1: Write the test**

```go
// runtime/dispatch/brief_test.go (add)
func TestParseBriefFromComposedLine(t *testing.T) {
	// Exactly what DispatchComposePanel builds.
	line := "!dispatch anvil%codex-cli repo=org/repo ticket=NEX-1 do the thing"
	b, err := ParseBrief([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if b.Agent != "anvil" || b.Provider != "codex-cli" || b.Repo != "org/repo" || b.Ticket != "NEX-1" {
		t.Fatalf("parsed brief = %+v", b)
	}
}
```

> Confirm the exact field mapping against `ParseBrief` (agent`%`provider, `repo=`, `ticket=`, trailing brief). Adjust the asserted fields to the real parser; the point is the composed line round-trips.

- [ ] **Step 2: Run**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/dispatch/ -run TestParseBriefFromComposedLine -v`
Expected: PASS (documents the contract the form targets).

- [ ] **Step 3: Commit**

```bash
git add runtime/dispatch/brief_test.go
git commit -m "test(dispatch): pin the composed !dispatch line round-trips through ParseBrief

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 6: DispatchComposePanel + Reply box

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/panels/DispatchComposePanel.js`
- Modify: `js/api.js`, `js/views/WatchView.js`

- [ ] **Step 1: api wrappers**

```javascript
// js/api.js (add). Operator chat-send already exists in comms/api; reuse it.
export function dispatchCompose({ agent, provider, repo, ticket, brief }) {
  const prov = provider ? `%${provider}` : '';
  const line = `!dispatch ${agent}${prov} repo=${repo} ticket=${ticket} ${brief}`.trim();
  return sendChat(line); // existing operator send-chat helper; if named differently, use it
}

export function replyToThread(thread, content) {
  return rpc('aspect.say', { thread, content });
}
```

> Confirm the existing operator send-chat function name in `api.js`/`comms.js` (the dashboard already posts chat). Reuse it for `dispatchCompose`. Confirm `aspect.say`'s payload field names against `frames.AspectSayPayload` and match them.

- [ ] **Step 2: DispatchComposePanel**

```javascript
// js/views/panels/DispatchComposePanel.js
const { html, useState } = window.__preact;
import { agents } from '../../state.js';
import { dispatchCompose } from '../../api.js';

export function DispatchComposePanel({ onClose }) {
  const roster = (agents.value || []).map((a) => (typeof a === 'string' ? a : a.id));
  const [agent, setAgent] = useState(roster[0] || '');
  const [provider, setProvider] = useState('codex-cli');
  const [repo, setRepo] = useState('');
  const [ticket, setTicket] = useState('');
  const [brief, setBrief] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = () => {
    if (!agent || !repo || !ticket || !brief.trim()) return;
    setBusy(true);
    dispatchCompose({ agent, provider, repo, ticket, brief })
      .then(() => { setBusy(false); onClose && onClose(); })
      .catch(() => setBusy(false));
  };

  return html`
    <aside class="watch-panel dispatch-compose">
      <header>Dispatch a run <button class="panel-close" onClick=${onClose}>×</button></header>
      <label>Agent <select value=${agent} onChange=${(e) => setAgent(e.target.value)}>
        ${roster.map((a) => html`<option value=${a}>${a}</option>`)}
      </select></label>
      <label>Provider <input value=${provider} onInput=${(e) => setProvider(e.target.value)} /></label>
      <label>Repo <input placeholder="org/repo" value=${repo} onInput=${(e) => setRepo(e.target.value)} /></label>
      <label>Ticket <input placeholder="NEX-123" value=${ticket} onInput=${(e) => setTicket(e.target.value)} /></label>
      <label>Brief <textarea value=${brief} onInput=${(e) => setBrief(e.target.value)}></textarea></label>
      <button class="btn-dispatch" disabled=${busy} onClick=${submit}>${busy ? 'Dispatching…' : 'Dispatch'}</button>
    </aside>`;
}
```

- [ ] **Step 3: Mount in WatchView**

Add a "+ Dispatch" toolbar button toggling `DispatchComposePanel`, and a reply box at the foot of the timeline for the selected run:

```javascript
function ReplyBox({ run }) {
  const [text, setText] = useState('');
  if (!run) return null;
  const send = () => { if (text.trim()) { replyToThread(run.thread, text); setText(''); } };
  return html`<div class="timeline-reply">
    <input value=${text} placeholder=${`reply to ${run.ticket}…`} onInput=${(e) => setText(e.target.value)}
      onKeyDown=${(e) => { if (e.key === 'Enter') send(); }} />
    <button onClick=${send}>Send</button>
  </div>`;
}
```

Import `DispatchComposePanel`, `replyToThread`, `useState`; render the toolbar toggle + `<${ReplyBox} run=${selectedRun} />` under the timeline. Add `.dispatch-compose`, `.timeline-reply`, `.btn-dispatch` to `css/watch.css`.

- [ ] **Step 4: Verify (dev mode)**

`+ Dispatch` opens the form → fill + Dispatch → a `!dispatch` post appears in chat and a new run shows in the feed. Reply box sends into the selected run's thread (appears in the timeline). No console errors.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/panels/DispatchComposePanel.js nexus/broker/static/dashboard/js/api.js nexus/broker/static/dashboard/js/views/WatchView.js nexus/broker/static/dashboard/css/watch.css
git commit -m "feat(dashboard): dispatch-compose panel + timeline reply box

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

# TICKET 2c — Fuller agent config + dispatch-enabled flag

## Task 7: `dispatch-enabled` flag — store + admin endpoint

**Files:**
- Modify: the aspects store (where `model-config`/`personality` persist — confirm the store; e.g. `nexus/aspects/...` or the broker config store)
- Create: `nexus/broker/admin_dispatch_enabled.go`
- Test: `nexus/broker/admin_dispatch_enabled_test.go`

- [ ] **Step 1: Write the failing test (endpoint round-trip)**

```go
// nexus/broker/admin_dispatch_enabled_test.go
// Mirror admin_model_config_test.go's rig (GET default true; PUT false; GET false).
func TestDispatchEnabledGetPut(t *testing.T) {
	rig := newAdminRig(t) // reuse the existing admin test rig
	// default enabled
	if got := rig.getDispatchEnabled("anvil"); got != true {
		t.Fatalf("default = %v, want true", got)
	}
	rig.putDispatchEnabled("anvil", false)
	if got := rig.getDispatchEnabled("anvil"); got != false {
		t.Fatalf("after PUT false = %v", got)
	}
}
```

> Use the same admin test harness as `admin_model_config_test.go` (line 88/114 pattern: `GET/PUT rig.url+"/api/admin/aspects/anvil/dispatch-enabled"`). Implement `getDispatchEnabled`/`putDispatchEnabled` helpers in the test mirroring `model-config`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestDispatchEnabled -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement the endpoint + store field**

Add a `dispatch_enabled` boolean to the per-aspect config store (default true), following exactly how `model-config` persists (same store, same migration pattern). Create the handler:

```go
// nexus/broker/admin_dispatch_enabled.go
package broker

import (
	"encoding/json"
	"net/http"
)

type dispatchEnabledBody struct {
	Enabled bool `json:"enabled"`
}

// GET/PUT /api/admin/aspects/{name}/dispatch-enabled. Mirrors admin_model_config.go.
func (b *Broker) handleAdminDispatchEnabled(w http.ResponseWriter, r *http.Request) {
	name := aspectNameFromPath(r) // same extractor model-config uses
	if name == "" {
		http.Error(w, "aspect name required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		enabled := b.aspectDispatchEnabled(name) // store getter; default true
		_ = json.NewEncoder(w).Encode(dispatchEnabledBody{Enabled: enabled})
	case http.MethodPut:
		var body dispatchEnabledBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := b.setAspectDispatchEnabled(name, body.Enabled); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(dispatchEnabledBody{Enabled: body.Enabled})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
```

Register the route next to `model-config` (same admin mux + auth middleware). Add `aspectDispatchEnabled(name) bool` (default true) and `setAspectDispatchEnabled(name, bool)` to the aspects store, and include `dispatch_enabled` in the roster payload (`/api/admin/roster`).

> Confirm the store + `aspectNameFromPath` + the admin route registration against `admin_model_config.go` and match them exactly.

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestDispatchEnabled -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/admin_dispatch_enabled.go nexus/broker/admin_dispatch_enabled_test.go nexus/broker/server.go nexus/aspects/
git commit -m "feat(broker): per-agent dispatch-enabled flag (store + admin endpoint + roster)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 8: Gate `!dispatch` on the flag

**Files:**
- Modify: `nexus/broker/dispatch_intercept.go`
- Test: `nexus/broker/dispatch_intercept_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nexus/broker/dispatch_intercept_test.go (add)
func TestSubmitDispatchRejectsDisabledAgent(t *testing.T) {
	b := newDispatchTestBroker(t) // existing helper
	b.setAspectDispatchEnabled("anvil", false)
	err := b.submitDispatch(context.Background(), "shadow",
		"!dispatch anvil repo=o/r ticket=NEX-1 do it", "NEX-1", 1)
	if err == nil {
		t.Fatal("disabled agent should be rejected")
	}
}
```

> Adapt to the existing `dispatch_intercept_test.go` harness/broker constructor.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestSubmitDispatchRejectsDisabled -v`
Expected: FAIL — no gating yet.

- [ ] **Step 3: Add the gate**

In `submitDispatch` (dispatch_intercept.go), after `ParseBrief` and before `runner.Submit`:

```go
	if !b.aspectDispatchEnabled(brief.Agent) {
		b.log.Info("dispatch: agent dispatch-disabled — rejecting", "agent", brief.Agent, "ticket", brief.Ticket)
		return fmt.Errorf("agent %s is dispatch-disabled", brief.Agent)
	}
```

The caller (`HandleChatSend`) already posts the error back to the thread, so the operator sees "agent X is dispatch-disabled".

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run 'TestSubmitDispatch|Dispatch' && go build ./...`
Expected: PASS + clean build (existing dispatch tests still green — default-enabled agents unaffected).

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/dispatch_intercept.go nexus/broker/dispatch_intercept_test.go
git commit -m "feat(broker): gate !dispatch on the per-agent dispatch-enabled flag

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 9: AgentConfigPanel + TeamPanel Configure

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/panels/AgentConfigPanel.js`
- Modify: `js/views/panels/TeamPanel.js`, `js/api.js`

- [ ] **Step 1: api wrappers (reuse admin endpoints over fetch)**

```javascript
// js/api.js (add). These are REST admin endpoints (not WS RPC) — use the same
// authed fetch the Settings views use (confirm the helper name, e.g. adminGet/adminPut).
export function getAgentModelConfig(name) { return adminGet(`/api/admin/aspects/${name}/model-config`); }
export function putAgentModelConfig(name, body) { return adminPut(`/api/admin/aspects/${name}/model-config`, body); }
export function getAgentPersonality(name) { return adminGet(`/api/admin/aspect/${name}/personality`); }
export function putAgentPersonality(name, body) { return adminPut(`/api/admin/aspect/${name}/personality`, body); }
export function getDispatchEnabled(name) { return adminGet(`/api/admin/aspects/${name}/dispatch-enabled`); }
export function setDispatchEnabled(name, enabled) { return adminPut(`/api/admin/aspects/${name}/dispatch-enabled`, { enabled }); }
```

> Confirm the authed-fetch helper the existing `SettingsAspects.js` uses for admin GET/PUT and reuse it (the bearer cascade from `resolveBearerToken`). Don't invent a new fetch path.

- [ ] **Step 2: AgentConfigPanel**

```javascript
// js/views/panels/AgentConfigPanel.js
const { html, useState, useEffect } = window.__preact;
import { getAgentModelConfig, putAgentModelConfig, getDispatchEnabled, setDispatchEnabled } from '../../api.js';

export function AgentConfigPanel({ agent, onClose }) {
  const [cfg, setCfg] = useState(null);
  const [enabled, setEnabled] = useState(true);
  useEffect(() => {
    if (!agent) return;
    getAgentModelConfig(agent).then(setCfg).catch(() => setCfg({}));
    getDispatchEnabled(agent).then((d) => setEnabled(!!(d && d.enabled))).catch(() => {});
  }, [agent]);
  if (!agent) return null;
  const saveModel = () => putAgentModelConfig(agent, cfg);
  const toggle = () => { const v = !enabled; setEnabled(v); setDispatchEnabled(agent, v); };
  return html`
    <aside class="watch-panel agent-config">
      <header>${agent} config <button class="panel-close" onClick=${onClose}>×</button></header>
      <label>Dispatchable <input type="checkbox" checked=${enabled} onChange=${toggle} /></label>
      ${cfg ? html`
        <label>Provider <input value=${cfg.provider || ''} onInput=${(e) => setCfg({ ...cfg, provider: e.target.value })} /></label>
        <label>Model <input value=${cfg.model || ''} onInput=${(e) => setCfg({ ...cfg, model: e.target.value })} /></label>
        <label>Judge model <input value=${cfg.judge_model || ''} onInput=${(e) => setCfg({ ...cfg, judge_model: e.target.value })} /></label>
        <button onClick=${saveModel}>Save model config</button>
      ` : html`<div>loading…</div>`}
    </aside>`;
}
```

> Match the model-config field names (`provider`/`model`/`judge_model`/`judge_provider`) to the real `adminModelConfigReq` JSON. Personality/provider-binding/mcp can be added as further field groups in the same panel; keep each group load-then-save.

- [ ] **Step 3: TeamPanel Configure affordance + dispatch state**

In `TeamPanel.js`, add a "Configure" button per agent (opens `AgentConfigPanel` for that agent) and show the dispatch-enabled state (a dot/label per agent, from the roster payload). Import `AgentConfigPanel`; manage the open-agent state.

- [ ] **Step 4: Verify (dev mode)**

Team panel → Configure on an agent opens the editor; toggle Dispatchable off → a `!dispatch` to that agent is rejected with "agent X is dispatch-disabled"; edit provider/model → Save persists (reload shows it). No console errors.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/panels/AgentConfigPanel.js nexus/broker/static/dashboard/js/views/panels/TeamPanel.js nexus/broker/static/dashboard/js/api.js
git commit -m "feat(dashboard): per-agent config editor + dispatch-enabled toggle in Team panel

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: Integration verification (build + deploy + dogfood)

- [ ] **Step 1: Full build + test + vet**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/... ./runtime/... && go test ./nexus/runs/ ./nexus/broker/ ./runtime/dispatch/`
Expected: all PASS, clean vet.

- [ ] **Step 2: Deploy to dMon**

Rebuild broker (`deploy/broker/build.sh`) + worker (`deploy/worker/build.sh`) + `kubectl rollout restart deploy/nexus-broker`. (Per `project_per_agent_home_live` / `project_work_ui_redesign`.)

- [ ] **Step 3: Dogfood each action**

In `#/watch`: (1) `+ Dispatch` a throwaway → it appears; (2) Stop it → flips to `cancelled`, agent frees; dispatch another → Force → immediate; (3) Reply into a run's thread → appears in the timeline; (4) Team → Configure → toggle an agent Dispatchable off → a `!dispatch` to it is rejected; edit provider/model → persists.

- [ ] **Step 4: Push the branch**

```bash
git push -u origin design/work-ui-phase2
```

---

## Self-Review notes (for the executor)

- **Spec coverage:** dispatch-from-UI (T5, T6) · cancel graceful/force (T1, T3, T4) · MarkDone race guard (T2) · reply (T6) · dispatch-enabled flag + gating (T7, T8) · fuller per-agent config (T9). All spec sections map to a task.
- **Type consistency:** `RunCancelPayload`/`RunCancelResultPayload` (T3) consumed by `runCancel` (T4); `runs.StatusCancelled` + the `MarkDone` guard (T2) used by run.cancel (T3); `dispatch-enabled` endpoint (T7) consumed by the gate (T8) + the UI (T9).
- **Confirm-against-live-code seams (flagged inline):** the broker's k8s handle for `DeleteJob` (`dispatchK8s` vs wrapping `k8sReader`) (T3); `ParseBrief` field mapping (T5); the operator send-chat helper name in api.js (T6); `AspectSayPayload` fields (T6); the aspects config store + `aspectNameFromPath` + admin route registration (T7); the existing admin-fetch helper (T9). These mirror existing patterns (`admin_model_config.go`, Phase 1 `runs_rpc.go`); confirm names, don't invent.
