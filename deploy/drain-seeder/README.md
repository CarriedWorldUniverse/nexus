# drain-seeder — the shadow-drain replacement (Phase 3)

## What replaced what

**Before (`deploy/shadow/drain-cronjob.yaml`, ns `nexus`, now SUSPENDED,
live backup at dMon `~/phase3-backup/cronjob-shadow-drain.yaml`):** a
CronJob that ran `agentfunnel -k /etc/nexus/keyfile.json -drain` AS the
named `shadow` aspect — it authenticated with shadow's keyfile
(`aspect-keyfile-shadow`), ran under shadow's ServiceAccount
(`shadow-aspect`), spent shadow's Claude OAuth token
(`shadow-claude-auth`), and drove the whole drain (Jira queue probe +
orchestrate loop) itself, in-process, as one aspect identity. That's the
named-fleet coupling the personality-pool rebuild retires: work items are
no longer tied to a specific aspect's keyfile/SA/image; they're ledger
rows any pool worker of the right role can claim
(`{personality}-{role}`, e.g. `anvil-builder`).

**After (`cronjob.yaml`, this directory):** a CronJob that does ONE thing —
`nexus workitem create --role builder --task "..." --criteria "..."` files
a single ledger work item (a gRPC write to the sovereign ledger, mTLS via
the same `nexus-broker-custodian-client` mesh cert nexus-control already
mounts). No aspect identity, no k8s dispatch authority, no Claude spend in
this container at all. The **standing orchestrator**
(`ORCHESTRATOR_ENABLE=1` on `nexus-control`, see
`deploy/nexus-control/README.md`) is what actually drains the item into a
pool worker on its own cadence (`ORCHESTRATOR_DRAIN_INTERVAL=15s` there) —
this CronJob is just the heartbeat that keeps the queue-drain habit alive
now that no always-on shadow aspect exists to self-schedule it.

## Idempotency — `--dedupe` (closed)

`cronjob.yaml` passes `nexus workitem create ... --dedupe`. Before filing,
`--dedupe` calls `workgraph.Client.ListReady(ctx, role, "")` for the
work item's role (`builder`) — this returns every item still queued or
dispatched/in-flight for that role, per `ListReady`'s doc comment — and
compares each one's `TaskSpec` against the new item's `TaskSpec`, **first
line only**, both sides `strings.TrimSpace`'d (`taskSpecFirstLineMatches`
in `nexus/cmd/nexus/workitem.go`). Rest-of-body differences (e.g. a
timestamp or detail baked further into the task text) don't defeat the
match — only the first line is the task's identity, matching how
`workgraph.summarize` already treats a `TaskSpec`'s first line as its
short-form identity.

On a match: prints `skipped: <existing-id> already open` to stdout and
exits **0** — success, not an error, so the CronJob never alarms on a
skip. No match: creates the item as normal and prints its id, same as
without `--dedupe`.

This closes the earlier no-dedupe caveat: a `*/30 * * * *` schedule can no
longer pile up concurrent duplicate drain items when the pool falls
behind — each fire either creates one drain item or, if one's still open
from a prior fire, skips.

## Operating

```
# un-suspend (deliberate operator action — never automatic)
kubectl patch cronjob drain-seeder -n nexus -p '{"spec":{"suspend":false}}'

# fire one manually, on-demand, without touching the schedule/suspend flag
kubectl create job --from=cronjob/drain-seeder drain-seeder-manual-$(date +%s) -n nexus

# re-suspend
kubectl patch cronjob drain-seeder -n nexus -p '{"spec":{"suspend":true}}'
```

## Env reference

Mirrors `nexus-control`'s own `WORKGRAPH_*` wiring
(`deploy/nexus-control/README.md`, "What the convergence changed"):

| Env | Value | Why |
|---|---|---|
| `WORKGRAPH_LEDGER_ADDR` | `ledger.cwb.svc.cluster.local:8081` | sovereign ledger gRPC address |
| `WORKGRAPH_TLS_CERT/_KEY/_CA` | `/etc/cwb/custodian-client/{tls.crt,tls.key,ca.crt}` | mTLS to the ledger, same mesh client cert nexus-control mounts (secret `nexus-broker-custodian-client`) |

`WORKGRAPH_ORG` / `WORKGRAPH_SUBJECT` / `WORKGRAPH_PROJECT` are left unset
— `nexus workitem create` defaults them to `carriedworld` /
`nexus-orchestrator` / `NET`, the same resolution the orchestrator's own
ledger dial uses, so the seeded item lands in the same org/project the
orchestrator drains from.
