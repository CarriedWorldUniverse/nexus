# Roundtable P1 — Napping Presence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Aspects become addressable-but-napping: a DM/@mention to a scaled-to-zero aspect wakes its pod in seconds; quiet wake-on-mention aspects scale back to zero. Restores plumb/anvil as team members you can talk to.

**Architecture:** Spec `docs/2026-06-11-roundtable-design.md` component 1. All broker-side: roster `napping` status, per-aspect wake policy config, `dispatch.K8s.ScaleDeployment`, a wake hook in chat fan-out, an idle reaper beside the existing `reapStale` ticker. Plus deployment manifests for plumb/anvil (carriedworld-cloud) modeled on keel's.

**Tech Stack:** Go (nexus broker, k8s client-go already in runtime/dispatch), Helm-convention values files (carriedworld-cloud).

---

### Task 1: Roster `napping` status

**Files:** `nexus/roster/roster.go` + its test file.

- [ ] Failing tests: (a) `SetNapping(name)` flips a known aspect to status `napping` (idempotent; unknown name = no-op returning false); (b) a napping aspect that registers goes `napping→live` (existing Register path — assert it already handles any→live, lock it in); (c) the stale sweep does NOT demote `napping` to `down` (napping is deliberate, not staleness — guard the `age >` cases on `Status == "live"`/`"stale"` only).
- [ ] Implement: the status string + `SetNapping` + sweep guard. Follow the file's existing mutex/string-status conventions exactly.
- [ ] Commit `feat(roster): napping status for wake-on-mention aspects`.

### Task 2: `ScaleDeployment` on the dispatch K8s client

**Files:** `runtime/dispatch/k8s.go` + test (the package's existing fake/recorder patterns).

- [ ] Failing test: `ScaleDeployment(ctx, "plumb", 1)` issues an update of the deployment's replicas via the typed client (or scale subresource — match how the package's client is built; it uses client-go's kubernetes.Interface, so `AppsV1().Deployments(ns).UpdateScale` with a fake clientset is the natural shape); scaling to 0 likewise; unknown deployment returns the API error wrapped with context.
- [ ] Implement (namespace = the K8s struct's existing Namespace field).
- [ ] Commit `feat(dispatch): ScaleDeployment for napping-aspect wake/sleep`.

### Task 3: Wake policy config + wake controller

**Files:** `nexus/broker/server.go` (config), new `nexus/broker/wake.go` + `wake_test.go`, hook in `nexus/broker/chat_send.go`.

- [ ] Config: `Config.AspectWakePolicy map[string]string` (values `always-on|wake-on-mention|dispatch-only`; absent = no wake behavior, exactly today's semantics) + `Config.AspectDeployment map[string]string` (aspect → deployment name; default = aspect name). Follow how other config maps are loaded in the broker's config path (find where Config is populated from file/env and mirror it).
- [ ] Failing tests (fake scaler interface so no real k8s): (a) chat fan-out to a recipient with no live conn + policy `wake-on-mention` → exactly one ScaleDeployment(name,1) call; (b) recipient live → no call; (c) second message while a wake is already in flight (within a `wakeDebounce`, default 60s) → no second call; (d) policy absent or `dispatch-only` → no call; (e) wake failure logs and does not fail the send (message is persisted; replay delivers on eventual register).
- [ ] Implement `wake.go`: a small `wakeController{scaler, policies, deployments, mu, lastWake map[string]time.Time}` with `MaybeWake(ctx, aspect)`. Hook: in `HandleChatSend` after persist, for each computed recipient with `b.dispatcher.connFor(rec) == nil`, call `b.wake.MaybeWake(ctx, rec)` (nil-safe when unconfigured). Also call `roster.SetNapping` is NOT done here — napping is set by the reaper; wake just scales.
- [ ] Commit `feat(broker): wake-on-mention controller`.

### Task 4: Idle reaper

**Files:** new `nexus/broker/idle_reaper.go` + test; wire beside the existing `reapStale` ticker in server startup.

- [ ] Failing tests (fake clock + fake scaler): (a) a `wake-on-mention` aspect with no chat to/from it for `IdleTimeout` (config, default 15m) AND no active dispatch run for it AND no in-flight observe turn → ScaleDeployment(name,0) + `roster.SetNapping`; (b) recent message ⇒ no reap; (c) active run (RunsStore ListRunning has the agent) ⇒ no reap; (d) in-flight turn (the observability hub's grouper reports an open turn — find the cheapest queryable signal; if none exists, track last `observe.begin`-without-`end` per aspect in the broker's observe inbound path and expose `turnInFlight(aspect) bool`) ⇒ no reap; (e) `always-on` policy never reaped.
- [ ] Last-activity source: track `lastChatActivity map[aspect]time.Time` updated in HandleChatSend for sender and recipients (cheap, already in the hot path) — do not scan the ChatStore.
- [ ] Implement + wire a ticker (interval = IdleTimeout/3) into broker start, gated on the wake controller being configured.
- [ ] Commit `feat(broker): idle reaper scales quiet wake-on-mention aspects to zero`.

### Task 5: plumb + anvil deployments (carriedworld-cloud)

**Files:** `hosting/services/plumb.values.yaml`, `hosting/services/anvil.values.yaml` (model on `keel.values.yaml`), referencing the existing builder image (`localhost/nexus-builder:dev` — the same image keel runs) with each aspect's keyfile secret (`aspect-keyfile-plumb`/`-anvil` — verify those secrets exist on the cluster; if missing, mint via the broker admin mint path and document), `replicas: 0` initial, a small PVC each for funnel session state (maren's PVC pattern — see nexus `deploy/shadow/pvc.yaml`).
- [ ] Render + `kubectl apply --dry-run=server` against dMon to validate; do NOT scale them up manually — the wake controller owns that.
- [ ] Commit (carriedworld-cloud branch `feat/napping-aspects`).

### Task 6: Broker config rollout + e2e acceptance (dMon)

- [ ] Add the wake-policy + deployment maps and IdleTimeout to the broker's deployed config (wherever the dMon broker reads config — find it in carriedworld-cloud `hosting/services/nexus-broker.yaml` or the image's config file): keel=always-on, maren=always-on (agy keyring requires a live pod — do NOT make maren wake-on-mention), plumb=wake-on-mention, anvil=wake-on-mention, shadow-aspect=always-on.
- [ ] Rebuild + redeploy broker; apply the new manifests.
- [ ] Acceptance: from agora (or turnprobe), DM plumb while its deployment is at 0 → pod starts, plumb registers, reply arrives in the thread, no operator action; after ~15m quiet, deployment back to 0 and roster shows `napping`; DM again → wakes again. Mention during scale-up does not double-wake.

## Out of scope (P2–P4)

spawn/hands, convene, mediation, digest delivery — separate plans after P1 ships.
