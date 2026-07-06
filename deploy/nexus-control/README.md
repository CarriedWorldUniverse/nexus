# nexus-control — the ONE broker (post-convergence, 2026-07-05)

`nexus-control` (ns `nexus`) is the single production broker: chat +
dashboard + orchestrator + pool dispatch, running `localhost/nexus-broker:li1`
with its own sqld sidecar (PVC `sqld-data`) as the DB of record.

`deployment-pre-li1.yaml` is the live capture taken immediately BEFORE the
li1 convergence flip — the rollback artifact. Rollback: `kubectl set image
deployment/nexus-control broker=localhost/nexus-broker:dev -n nexus` and
remove the pool/orchestrator env vars listed below (DB writes are additive
and safe to leave).

## What the convergence changed (2026-07-05)
- image `:dev` → `:li1` (cairn line `builder/li1-orchestrator-wiring`)
- env added: `POOL_PROVIDER_BASE_URL` (litellm/Ornith), `POOL_MODEL=ornith`,
  `POOL_PROVIDER=openai`, `POOL_PROVIDER_KEY=dummy`, `POOL_PERSONALITIES=
  anvil,plumb,keel,maren,harrow`, `ORCHESTRATOR_ENABLE=1`,
  `WORKGRAPH_LEDGER_ADDR=ledger.cwb.svc.cluster.local:8081`,
  `WORKGRAPH_TLS_*=/etc/cwb/custodian-client/*` (reuses the mounted mesh
  client cert), `ORCHESTRATOR_DRAIN_INTERVAL=15s`,
  `CW_BUILDER_IMAGE=localhost/nexus-builder:li1`
- aspects rows plumb/keel/maren/harrow set provider=openai model=ornith
  (anvil already was) — operator decision: the named fleet retires into
  pool personalities on the shared Ornith brain
- RETIRED: the stale `nexus-broker` twin deployment (created 2026-06-06;
  shared PVC `nexus-broker-data` incl. tailscale state with nexus-control —
  two tailscaled on one node key), its `nexus-broker-li1` Service, and the
  frozen `sqld` (cwb) it pointed at (scaled back to 0). The PVC itself
  stays — nexus-control mounts it.

## Phase 4 deploy gap: `CW_GIT_CRED_NAME` (real REPO tickets)

`runtime/dispatch/runner.go`'s `provisionRun` only issues the scoped git
credential (`cw credential issue-git-permission --aspect <worker> --name
<CW_GIT_CRED_NAME>`) when `cfg.GitCredName != "" && b.Repo != ""` — see
`nexus/cmd/nexus/main.go`'s `JobConfig{GitCredName: os.Getenv
("CW_GIT_CRED_NAME")}`. **This env var is not in nexus-control's env list
above** (it was never added in the 2026-07-05 convergence, which only
wired pool dispatch's provider/orchestrator plumbing, not repo work). Until
it's set, EVERY dispatch through this broker — named or pool — that
carries a repo logs `dispatch: skipping git credential grant; git
credential name not configured` and the worker has no push credential for
`b.Repo`, even though `-repo`/`-branch`/the PR gate all still activate
correctly off `Brief.Repo` alone (see `runtime/dispatch/jobspec.go`'s
`builderArgs` and `runtime/cmd/agentfunnel/main.go`'s `*repoFlag` gate,
neither of which needs `GitCredName`).

**To un-gap:** set `CW_GIT_CRED_NAME=<name>` on `nexus-control`, where
`<name>` is the name of an already-provisioned `kind=git` credential (`cw
credential create` / `POST /api/admin/credentials`, see
`nexus/broker/admin_credentials.go`) granted to whichever aspects/
personalities dispatch repo work (`cw credential issue-git-permission`
scopes a per-worker grant off an existing credential — it does not create
one). This ticket does not invent a name/value for that credential; it is
an operator-owned secret provisioning step, same posture as `WORKGRAPH_*`/
`POOL_*` in "What the convergence changed" above. Once set, Phase 4's pool
repo-ticket threading (`WorkItem.Repo` -> `PoolItem.Repo` -> `Brief.Repo`,
see `nexus/workgraph/README.md`'s repo mapping note and
`runtime/dispatch/README.md`'s pool model) needs no further broker config —
the same `JobConfig.GitCredName` already served named dispatch.

## Live-verified after convergence
- NET-23 → `plumb-builder` leased, ran on Ornith, output CONVERGED-ALPHA-OK
  (token confirmed in log), task_done, Job Complete 20s.
- NET-24 → `keel-builder` leased concurrently (anvil was busy — one job per
  personality held). **Finding:** keel called `task_done` with a confident
  summary but NEVER produced the required token — the task_done exit path
  trusts the model's self-report and does not verify the DoD. Result
  verification is the next unit (check the acceptance criteria against the
  actual posted output before honoring task_done, or verify orchestrator-side
  on the result channel).
