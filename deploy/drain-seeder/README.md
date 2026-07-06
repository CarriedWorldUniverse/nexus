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
looks for one that names the same task as the new item
(`findDuplicateWorkItem` in `nexus/cmd/nexus/workitem.go`):

- **Primary:** the new item's would-be `Summary` (`workgraph.Summarize
  (task_spec)` — first line, `TrimSpace`'d, capped at 120 chars) against
  each existing item's real `Summary` (`GetWorkItem`'s read of the ledger
  issue's `summary` column). Preferred over a `TaskSpec`/`Description`
  comparison: `Summary` is a narrow, single-producer field (`CreateWorkItem`
  always derives it this same way, never from caller input), so it has less
  surface for anything else touching the issue between write and read to
  reformat than the wider `Description` field does.
- **Fallback** (only if an existing item has no `Summary`, e.g. very old
  ledger data predating this field): `taskSpecFirstLineMatches` against
  `TaskSpec`/`Description` directly — first line only, whitespace
  (including internal reflow/rewrap) normalized.

On a match: prints `skipped: <existing-id> already open` to stdout and
exits **0** — success, not an error, so the CronJob never alarms on a
skip. No match: creates the item as normal and prints its id, same as
without `--dedupe`.

This closes the earlier no-dedupe caveat: a `*/30 * * * *` schedule can no
longer pile up concurrent duplicate drain items when the pool falls
behind — each fire either creates one drain item or, if one's still open
from a prior fire, skips.

**2026-07-06 hardening:** a live failure was reported — NET-41/NET-42 both
open for role `builder`, yet a `--dedupe` run created NET-43 instead of
skipping, despite the seeder's `--task` text being byte-identical across
fires (confirmed via git history: unchanged since this file's introduction).
Investigation (see `nexus/workgraph/adapter.go`'s `GetWorkItem`/`Summarize`,
the pinned `github.com/CarriedWorldUniverse/ledger` v0.1.4's `toProtoIssue`/
`CreateIssue`/`GetIssue`, and `TestListReady_RoundTripsTaskSpecAndSummary`)
found no confirmed `Description`-round-trip mutation in the code paths this
repo can inspect/exercise — both are exact passthroughs of the stored
columns. Given no live-ledger access to inspect the actual NET-41/42/43 rows
directly, the fix taken is defensive rather than a confirmed-root-cause
patch: match preferentially on the narrower `Summary` field (added to
`workgraph.WorkItem`, populated by `GetWorkItem`) instead of the wider
`TaskSpec`/`Description`, and normalize the `TaskSpec` fallback against
internal whitespace reflow. See PR that introduced this section for the
full file:line writeup.

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
