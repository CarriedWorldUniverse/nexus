# Live-Integration 1 — Orchestrator + DocRegister broker wiring (build spec)

**Goal:** make a broker built from the M1 line actually RUN the orchestrator + expose the doc register. This is item 8 of the live-integration list — the wiring that turns M1's tested-in-isolation packages into a running pipeline. ALL env-gated so a broker with none of the new env set behaves exactly as today.

## Current wiring state (on this line, confirmed)
- WIRED: `WorkerStatusStore` (unit 5), `FrontierAuthFunc`/`ImageTagPin` (unit 7) in `nexus/cmd/nexus/main.go`.
- NOT wired: `Config.DocRegister` (nil → endpoints dormant); the **orchestrator is never constructed or started** (only `runner.OnJobDoneHook` exists as a field).

## Deliverables (all in `nexus/cmd/nexus/main.go`, gated on new env)
1. **DocRegister wiring**: when `DOCREGISTER_ENABLE=1` (or a cairn-workdir env is set), construct the `docregister` store (a sqlite store like WorkerStatusStore — reuse the same `db`/migration pattern) + `GitCairnContent` (pointed at a cairn-line workdir via env, e.g. `DOCREGISTER_CAIRN_DIR`), set `Config.DocRegister`. Absent env → nil (dormant), today's behavior.
2. **Workgraph client**: construct a `workgraph.Client` dialing the sovereign ledger when `WORKGRAPH_LEDGER_ADDR` is set (reuse `workgraph.DialCreds` — it reads `WORKGRAPH_TLS_CERT/_KEY/_CA`; default those to the broker's existing mounted cert paths if a sensible default exists, else require the env). Absent → orchestrator not started.
3. **Orchestrator construct + start**: when the workgraph client + `ORCHESTRATOR_ENABLE=1` are present, construct `orchestrator.Orchestrator{Graph: workgraphClient, Dispatcher: runner, WorkerStatus: workerStatusStore, Roles: <from ORCHESTRATOR_ROLES csv, default builder,tester,reviewer,security-reviewer>, AuthProbe: <nil or a simple probe>, Alerter: <LogAlerter default>, Resolver: <nil-safe>, staleAfter/staleThreshold from env}`. Then:
   - Wire `runner.OnJobDoneHook = orch.OnJobDoneHook()` (the event wake — record result + re-drain).
   - Start a **drain goroutine**: a `time.Ticker` (cadence from `ORCHESTRATOR_DRAIN_INTERVAL`, default 30s) calling `orch.DrainOnce(ctx)`, plus the OnJobDone wake already covers event-triggered. Respect ctx cancellation for graceful shutdown. Log each drain's DrainReport summary.
4. Absent all the new env → the broker constructs none of this and behaves identically to today (verify the existing tests still pass).

## Constraints
- cairn line `builder/li1-orchestrator-wiring` off `builder/m1-unit8-console`. `cairn commit`, no push.
- STRICTLY additive + env-gated. The default (no new env) path must be byte-unchanged behavior — existing broker/cmd tests must pass untouched.
- Graceful: construction failures (bad ledger addr, missing cert) should log + skip the orchestrator, NOT crash the broker (it still serves chat/dispatch). Fail-soft on the optional subsystem.
- Don't invent a result channel (that's the next unit) — OnJobDoneHook's existing OK→done/blocked translation is fine for now.

## Acceptance
1. `go build ./...` + `go vet` clean; ALL existing tests pass (the no-new-env path is unchanged).
2. Unit test(s): with the env set (and fakes/mocks for the stores + a fake ledger addr that the workgraph client construction tolerates or is mocked), the wiring constructs the orchestrator + registers OnJobDoneHook + the drain goroutine starts and calls DrainOnce (assert via a mock/short interval); with NO env, none of it is constructed (Config.DocRegister nil, no orchestrator, OnJobDoneHook nil).
3. README/section documenting every new env var, the fail-soft behavior, and the live-verify path (set the env, boot broker, observe a drain-loop log line + /api/admin/workers + /api/docs responding).
