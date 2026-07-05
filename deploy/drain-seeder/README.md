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

## Idempotency — NOT yet handled, by design (v1)

`workgraph.Client.CreateWorkItem` has **no dedupe** — every fire of this
CronJob (once un-suspended) files a brand-new ledger issue, even if the
previous one is still open/unclaimed/in-progress. Left simple
deliberately for v1: shadow-drain itself only ran one drain per fire and
relied on Forbid concurrency + its own in-process queue-empty short
circuit, neither of which this seeder has an equivalent for on the ledger
side.

**Before un-suspending on a real cadence**, pair this with a dedupe check
(e.g. skip the `workitem create` call if a `role=builder,
assignee_aspect=builder` item already sits open/unclaimed in project NET —
a `ListReadyIssues`-shaped check ahead of the create). Until that lands,
either: leave `suspend: true` and fire manually/on-demand, or accept that
a `*/30 * * * *` schedule can pile up duplicate open drain items if the
pool falls behind.

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
