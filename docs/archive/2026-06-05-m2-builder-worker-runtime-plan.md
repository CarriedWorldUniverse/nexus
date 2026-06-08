# M2 — Builder-Agent Worker Runtime — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run a named agent as an on-demand k8s "builder" pod that drains a dispatched brief, does the work as that agent, posts the result, and exits on an explicit done-signal.

**Architecture:** Reuse the whole agentfunnel deliberation/comms/tool path. Add (1) a `task_done` comms action wired to a new `funnel.Config.OnTaskDone` callback, (2) an agentfunnel `-builder` flag that sets `OnTaskDone = stop` (+ a safety timeout) so the process exits when the agent declares done, and (3) a worker container image + k8s Job manifest + host-backed shared storage.

**Tech Stack:** Go (nexus funnel + agentfunnel); Docker; k8s (Job + hostPath volume); the M1 custodian seam for creds.

**Spec:** `docs/2026-06-05-m2-builder-worker-runtime-design.md` · **Story:** NEX-436 · **Epic:** NEX-434

---

## File Structure

**nexus (Go):**
- `nexus/frame/funnel/comms.go` — MODIFY: add `ToolNameTaskDone`, register the tool, add a `case` in the comms-action switch that invokes `OnTaskDone`.
- `nexus/frame/funnel/funnel.go` — MODIFY: add `OnTaskDone func(summary string)` to `Config`.
- `nexus/frame/funnel/comms_test.go` — MODIFY: test that the `task_done` action invokes `OnTaskDone`.
- `runtime/cmd/agentfunnel/main.go` — MODIFY: `-builder` flag; in builder mode wire `OnTaskDone` → `stop()` and start a safety-timeout goroutine.

**Deploy (infra):**
- `deploy/worker/Dockerfile` — CREATE: the shared worker image.
- `deploy/worker/build.sh` — CREATE: build + push to the local in-cluster registry.
- `deploy/worker/job.yaml` — CREATE: the builder Job manifest (image + named-agent keyfile Secret + the shared volume).
- `deploy/worker/storage.yaml` — CREATE: the hostPath PV/PVC for `/work` + caches.

Build order: Tasks 1–2 (the Go done-signal path) first — they're the load-bearing runtime change and unit-testable without k8s. Then 3–5 (image + manifests). Then 6 (the live DoD).

---

## Task 1: `task_done` comms action + `OnTaskDone` callback (funnel)

**Files:**
- Modify: `nexus/frame/funnel/comms.go`, `nexus/frame/funnel/funnel.go`
- Test: `nexus/frame/funnel/comms_test.go`

A builder calls `task_done` (optionally with a one-line summary) when the brief is complete. The funnel invokes `Config.OnTaskDone(summary)`. For always-on aspects `OnTaskDone` is nil → the action is a harmless no-op (still records the summary text).

- [ ] **Step 1: Add the Config callback**

In `nexus/frame/funnel/funnel.go`, inside `type Config struct` (next to `SystemPromptFn`/`BindingFn`):
```go
	// OnTaskDone, when non-nil, is invoked when the aspect emits the
	// task_done comms action — the builder-mode completion signal (M2).
	// nil for always-on aspects (the action is then a no-op).
	OnTaskDone func(summary string)
```

- [ ] **Step 2: Write the failing test**

In `nexus/frame/funnel/comms_test.go` (mirror an existing comms-action test's harness — the ones exercising `send_chat`/`react_to`):
```go
func TestTaskDoneInvokesCallback(t *testing.T) {
	var got string
	called := false
	f := newCommsTestFunnel(t, func(cfg *Config) { // mirror existing helper that builds a Funnel with a Config
		cfg.OnTaskDone = func(summary string) { called = true; got = summary }
	})
	// Drive the comms-action handler with a task_done action carrying a summary.
	// Use whatever entry the other comms tests use to invoke one action
	// (e.g. f.handleCommsAction(ctx, action) — match the real method).
	f.dispatchCommsAction(t, ToolNameTaskDone, map[string]any{"summary": "PR #999 opened"})
	if !called || got != "PR #999 opened" {
		t.Fatalf("OnTaskDone called=%v summary=%q", called, got)
	}
}

func TestTaskDoneNoCallbackIsNoOp(t *testing.T) {
	f := newCommsTestFunnel(t, func(cfg *Config) { cfg.OnTaskDone = nil })
	// Must not panic with a nil callback.
	f.dispatchCommsAction(t, ToolNameTaskDone, map[string]any{"summary": "x"})
}
```
> `newCommsTestFunnel` / `dispatchCommsAction` are placeholders for the real test harness in `comms_test.go` — match the helper the existing `send_chat`/`react_to` tests use to construct a `*Funnel` and feed it one comms action. If none exists, the minimal real call is whatever `comms.go`'s switch is reached through.

- [ ] **Step 3: Run test to verify it fails**

Run: `cd nexus && go test ./nexus/frame/funnel/ -run TestTaskDone -v`
Expected: FAIL — `ToolNameTaskDone` undefined.

- [ ] **Step 4: Add the tool name + switch case**

In `nexus/frame/funnel/comms.go`, add to the `CommsToolNames` const block (next to `ToolNameSendChat`):
```go
	ToolNameTaskDone = "task_done"
```
Add a `case` in the comms-action switch (the one at ~line 437 with `case ToolNameSendChat:`), mirroring the no-network-write actions:
```go
	case ToolNameTaskDone:
		summary, _ := args["summary"].(string)
		if f.cfg.OnTaskDone != nil {
			f.cfg.OnTaskDone(summary)
		}
		// Record the summary as the turn's outcome the same way other
		// terminal actions are recorded (match the surrounding cases).
```
Register `task_done` in the comms tool set the model sees (the place where `send_chat` et al. are declared as tools/described — grep `ToolNameSendChat` in comms.go for the tool-list builder and add a `task_done` entry with description: "Call when the dispatched task is fully complete (PR opened + reported). Ends your run.").

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd nexus && go test ./nexus/frame/funnel/ -run TestTaskDone -v && go vet ./nexus/frame/funnel/`
Expected: PASS, vet clean.

- [ ] **Step 6: Commit**

```bash
git add nexus/frame/funnel/comms.go nexus/frame/funnel/funnel.go nexus/frame/funnel/comms_test.go
git commit -m "feat(funnel): task_done comms action + OnTaskDone callback (NEX-436)"
```

---

## Task 2: agentfunnel `-builder` mode

**Files:**
- Modify: `runtime/cmd/agentfunnel/main.go`
- Test: `runtime/cmd/agentfunnel/builder_test.go` (create)

In builder mode the process exits when the agent emits `task_done` (via `OnTaskDone → stop()`), with a safety timeout so a stuck builder can't run forever. `deliberateLoop` already returns on `ctx.Done()`, and `wsClient.Run(ctx)` then returns → `main` exits cleanly.

- [ ] **Step 1: Add the flag + a builder-timeout helper**

Near the other flags in `main.go`:
```go
	builderMode := flag.Bool("builder", false, "builder/one-shot mode: drain the dispatched brief, run to the task_done signal, then exit")
	builderTimeout := flag.Duration("builder-timeout", 30*time.Minute, "max wall-clock for a builder run before forced exit")
```

- [ ] **Step 2: Wire OnTaskDone + the safety timeout (only in builder mode)**

Where the `funnel.Config{...}` is built (the `funnel.New(funnel.Config{...})` call), add — guarded by `*builderMode`:
```go
	// (declare stop earlier or move the signal.NotifyContext above funnel.New
	//  so `stop` is in scope here; ctx/stop are created at main.go:~555)
	cfg := funnel.Config{ /* existing fields */ }
	if *builderMode {
		cfg.OnTaskDone = func(summary string) {
			log.Info("agentfunnel: builder task_done — exiting", "aspect", res.AspectName, "summary", summary)
			stop() // cancel ctx → deliberateLoop + wsClient.Run return → clean exit
		}
	}
	f, err := funnel.New(cfg)
```
And after the goroutines are started (near `go deliberateLoop(ctx, f, log)`), add the safety net:
```go
	if *builderMode {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(*builderTimeout):
				log.Error("agentfunnel: builder timeout — forcing exit", "timeout", *builderTimeout)
				stop()
			}
		}()
	}
```
> Note: `ctx, stop := signal.NotifyContext(...)` is currently created at ~main.go:555, *after* `funnel.New`. Move that block above `funnel.New` so `stop` is in scope for `OnTaskDone`. (Mechanical reorder; no behaviour change for the always-on path.)

- [ ] **Step 3: Write a focused test**

`runtime/cmd/agentfunnel/builder_test.go` — assert the wiring: a `funnel.Config` built in builder mode has a non-nil `OnTaskDone` whose invocation cancels a context. Extract the wiring into a tiny testable helper if needed:
```go
func TestBuilderOnTaskDoneCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onDone := builderOnTaskDone(cancel, slog.Default(), "anvil") // small helper returning the OnTaskDone func
	onDone("PR opened")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("OnTaskDone did not cancel the context")
	}
}
```
Implement `builderOnTaskDone(cancel context.CancelFunc, log *slog.Logger, aspect string) func(string)` and use it from Step 2 instead of an inline closure (keeps it unit-testable).

- [ ] **Step 4: Run + build**

Run: `cd nexus && go test ./runtime/cmd/agentfunnel/ -run TestBuilder -v && go build ./runtime/cmd/agentfunnel`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add runtime/cmd/agentfunnel/main.go runtime/cmd/agentfunnel/builder_test.go
git commit -m "feat(agentfunnel): -builder mode exits on task_done (NEX-436)"
```

---

## Task 3: Worker image

**Files:**
- Create: `deploy/worker/Dockerfile`, `deploy/worker/build.sh`

One shared image. Multi-stage: build the Go binaries (agentfunnel + the nexus-*-mcp servers), then a runtime stage with the toolchain + provider CLIs + cw.

- [ ] **Step 1: Write the Dockerfile**

`deploy/worker/Dockerfile`:
```dockerfile
# --- build stage: nexus Go binaries ---
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agentfunnel ./runtime/cmd/agentfunnel \
 && CGO_ENABLED=0 go build -o /out/nexus-issue-mcp ./runtime/cmd/nexus-issue-mcp \
 && CGO_ENABLED=0 go build -o /out/nexus-jira-mcp  ./runtime/cmd/nexus-jira-mcp \
 && CGO_ENABLED=0 go build -o /out/nexus-comms-mcp ./runtime/cmd/nexus-comms-mcp

# --- runtime stage ---
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl gnupg build-essential \
 && rm -rf /var/lib/apt/lists/*
# Go toolchain (for builders that compile Go work)
COPY --from=golang:1.26 /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/usr/local/bin:$PATH
# provider CLIs (claude-code, codex) + cw — install per their distribution
# (npm/curl installers); pin versions. cw built from the cw repo or fetched.
#   e.g. RUN npm i -g @anthropic-ai/claude-code @openai/codex   (pin)
#        COPY --from=<cw-build> /cw /usr/local/bin/cw
COPY --from=build /out/agentfunnel /out/nexus-issue-mcp /out/nexus-jira-mcp /out/nexus-comms-mcp /usr/local/bin/
# NO secrets baked in — creds come from the M1 custodian seam at runtime.
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/agentfunnel"]
```
> The provider-CLI + cw install lines are marked with their real installers — pin exact versions at implementation. cw can be a `COPY --from` of a cw build stage or a release artifact.

- [ ] **Step 2: Build script**

`deploy/worker/build.sh`:
```bash
#!/usr/bin/env bash
set -euo pipefail
REG="${REG:-localhost:5000}"          # local in-cluster registry on dMon
TAG="${TAG:-$(git rev-parse --short HEAD)}"
docker build -f deploy/worker/Dockerfile -t "$REG/nexus-builder:$TAG" .
docker push "$REG/nexus-builder:$TAG"
echo "$REG/nexus-builder:$TAG"
```

- [ ] **Step 3: Build it (on dMon, where the registry + cluster are)**

Run: `REG=localhost:5000 bash deploy/worker/build.sh`
Expected: image builds + pushes; prints the tag.

- [ ] **Step 4: Commit**

```bash
git add deploy/worker/Dockerfile deploy/worker/build.sh
git commit -m "feat(deploy): builder worker image (NEX-436)"
```

---

## Task 4: Shared storage + builder Job manifest

**Files:**
- Create: `deploy/worker/storage.yaml`, `deploy/worker/job.yaml`

- [ ] **Step 1: hostPath storage (single-node dMon)**

`deploy/worker/storage.yaml` — a hostPath PV + PVC for `/work` + caches (RWX on the single node):
```yaml
apiVersion: v1
kind: PersistentVolume
metadata: { name: nexus-builder-work }
spec:
  capacity: { storage: 50Gi }
  accessModes: ["ReadWriteMany"]
  hostPath: { path: /var/lib/nexus-builder }   # on the dMon node
  persistentVolumeReclaimPolicy: Retain
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: nexus-builder-work, namespace: nexus }
spec:
  accessModes: ["ReadWriteMany"]
  resources: { requests: { storage: 50Gi } }
  volumeName: nexus-builder-work
```

- [ ] **Step 2: Builder Job manifest (templated by agent)**

`deploy/worker/job.yaml` — `${AGENT}` / `${IMAGE}` substituted at apply (M3 does this programmatically; M2 substitutes by hand/envsubst):
```yaml
apiVersion: batch/v1
kind: Job
metadata: { name: builder-${AGENT}-${TASK_ID}, namespace: nexus }
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: builder
          image: ${IMAGE}
          args: ["-k", "/etc/nexus/keyfile.json", "-builder"]
          env:
            - { name: CW_SEAM_URL, value: "https://broker.nexus:7888" }   # M1 seam
            - { name: GOCACHE, value: "/cache/go" }
          volumeMounts:
            - { name: work, mountPath: /work }
            - { name: cache, mountPath: /cache }
            - { name: keyfile, mountPath: /etc/nexus, readOnly: true }
      volumes:
        - { name: work, persistentVolumeClaim: { claimName: nexus-builder-work } }
        - { name: cache, persistentVolumeClaim: { claimName: nexus-builder-work } }  # same vol, /cache subpath in M3
        - { name: keyfile, secret: { secretName: aspect-keyfile-${AGENT} } }
```
> The per-agent keyfile is a k8s Secret `aspect-keyfile-${AGENT}` (the named agent's keyfile). `CW_SEAM_URL` points at the broker's in-cluster seam. The cw credential helper supplies git/provider creds (M1) — no secrets in the image. `-builder` makes the entrypoint exit on `task_done`.

- [ ] **Step 3: Apply storage (smoke)**

Run (on dMon): `sudo kubectl apply -f deploy/worker/storage.yaml && kubectl get pvc -n nexus nexus-builder-work`
Expected: PVC Bound.

- [ ] **Step 4: Commit**

```bash
git add deploy/worker/storage.yaml deploy/worker/job.yaml
git commit -m "feat(deploy): builder storage + Job manifest (NEX-436)"
```

---

## Task 5: Live DoD — anvil-as-builder runs a real brief

**Files:** none (operational verification on dMon).

- [ ] **Step 1: Provision the agent keyfile Secret**

Create `aspect-keyfile-anvil` from anvil's keyfile, grant anvil a scoped git credential via M1 (`cw issue-git-permission --name <git-cred> --aspect anvil`).

- [ ] **Step 2: Dispatch a brief to anvil's inbox**

Send anvil a real coding brief (a small NEX-433-style fix) as a chat dispatch — it lands pending in anvil's inbox.

- [ ] **Step 3: Apply the builder Job**

Run (on dMon): `AGENT=anvil IMAGE=localhost:5000/nexus-builder:<tag> TASK_ID=$(date +%s) envsubst < deploy/worker/job.yaml | kubectl apply -f -`

- [ ] **Step 4: Verify the DoD**

Watch: `kubectl logs -n nexus job/builder-anvil-<task> -f`. Assert:
- it validates **as anvil**, drains the brief, runs the work;
- pushes a branch + opens a PR **authored by anvil**;
- posts a result to the thread, emits `task_done`;
- the pod exits 0 and the Job reaches **Completed**;
- the pod env held **no raw secret** (creds flowed via cw/seam).

- [ ] **Step 5: Record the result**

Comment the outcome on NEX-436 (Job status, PR link, attribution check). This is the milestone's acceptance.

---

## Self-Review

**Spec coverage:**
- Worker image → Task 3 ✓ · Shared storage → Task 4 ✓ · Builder entrypoint (drain-once/run-to-done/exit) → Tasks 1–2 ✓ · Brief delivery (inbox drain) → reused (Task 5 Step 2) ✓ · Done-signal (`task_done`) → Task 1 ✓ · DoD (manual Job, anvil, real brief, exits) → Task 5 ✓
- Non-goals (M3 controller, fan-out) correctly excluded.

**Type/name consistency:** `ToolNameTaskDone` (= "task_done"), `Config.OnTaskDone func(summary string)`, `builderOnTaskDone`, `-builder`/`-builder-timeout`, image `nexus-builder`, PVC `nexus-builder-work`, Secret `aspect-keyfile-${AGENT}`, `CW_SEAM_URL` — used consistently.

**Bind-to-real-code seams (resolve by reading the named file at execution):**
1. `comms_test.go` harness (`newCommsTestFunnel`/`dispatchCommsAction`) — match the helper the existing send_chat/react_to tests use.
2. The comms-action switch body conventions at `comms.go:~437` — mirror a sibling no-network-write case for how the outcome is recorded.
3. The comms tool-list builder (where `send_chat` is *declared* as a callable tool) — add the `task_done` tool entry there.
4. The `signal.NotifyContext`/`stop` block in `main.go:~555` — move above `funnel.New` so `stop` is in scope.
5. provider-CLI + cw install lines in the Dockerfile — pin exact versions/installers.
These are the only seams; the funnel callback, the flag wiring, the manifests, and the DoD are complete above.
