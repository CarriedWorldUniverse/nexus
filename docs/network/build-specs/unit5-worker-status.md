# M1 Unit 5 — Worker status contract (build spec)

**Goal:** every worker (and orchestrator invocation) publishes ONE machine-readable status shape on a heartbeat; the broker consolidates it; one endpoint serves the fleet. State machine first, events over scraped prose. Ref: PHASE2-DESIGN §5.

## The status shape (from §5)
```
worker_status: {agent, role, personality, work_item_id,
                state: spawning|running|blocked|awaiting_gate|done|failed,
                auth_ok, token_expires_at, provider, model,
                cli_version, image_tag,
                last_heartbeat, started_at, turns, tokens_used}
```

## Touchpoints (verify against the integration line)
- `nexus/frames/payloads.go` — `DispatchStatusPayload` today carries only run_id/status/reason/at. EXTEND it (field-additive only — frames are UNVERSIONED, old receivers ignore unknown JSON fields; NEVER repurpose an existing field) into the full worker_status shape, or add a new `WorkerStatusPayload` + frame kind. Prefer a new payload/kind so the existing dispatch.status contract is untouched.
- `nexus/broker/dispatch_status.go` — `handleDispatchStatusFrame` (NO test today — add one). Upsert incoming worker_status into a new `worker_status` table (mirror `nexus/runs` SQLStore idiom; hand-rolled migration is fine but follow the runs pattern).
- `runtime/cmd/agentfunnel/main.go` — EMIT the status: at boot (state=spawning→running), each turn boundary, and a ~60s wall-clock heartbeat (a funnel-owned `time.Ticker` — bridle has no mid-turn heartbeat hook; read the latest bridle EventSink/TurnResult metrics for turns/tokens_used). Populate role/work_item_id/personality from the Brief (RolePrompt not needed here — use the Role LABEL + WorkItemID + Personality fields wave-2 added). auth_ok/token_expires_at from the frontier token; cli_version from the binary; provider/model from the resolved binding.
- `nexus/broker/admin.go` — new `GET /api/admin/workers` returning the consolidated fleet from the worker_status table. MUST be `requireAdmin` (not `b.auth`) — this is the separation-of-duties lesson; operator/admin only.

## Constraints
- Work on cairn line `builder/m1-unit5-worker-status`, expressed `--from builder/m1-integration` (so you have workgraph + pool + Brief fields). `cairn commit`, no push.
- Field-additive frame discipline; new payload/kind preferred over mutating DispatchStatusPayload.
- Heartbeat is best-effort: a failed status emit must NOT crash or block the worker's real work.
- `/api/admin/workers` = requireAdmin.

## Acceptance
1. `go build ./...` + `go vet` clean; existing dispatch/broker tests pass.
2. Unit tests: the worker_status upsert (dispatch_status.go — the currently-untested path); the /api/admin/workers endpoint returns consolidated rows + rejects non-admin (requireAdmin); the agentfunnel heartbeat ticker emits on cadence (test the emit logic, mock the sink); stale detection helper if you add one.
3. README: the status shape, the heartbeat cadence/seams, the endpoint contract, and how the orchestrator (unit 6) will consume it for auto-reap (stale last_heartbeat → reap).
4. Document the live-verify path (dispatch a worker, GET /api/admin/workers, see its heartbeat rows advance).
