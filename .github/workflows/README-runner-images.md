# Runner images CI (M1 Unit 7, Part C)

`runner-images.yml` is the documented CI deliverable for PHASE2-DESIGN §7
("CLI version strategy") and the §7 build spec's Part C. It is **not wired to
live secrets/hosts in this repo state** — per the build spec ("it doesn't
need to run") the workflow file + this doc are the deliverable; enabling it
for real is a separate, deliberate step (see "Enabling this for real" below).

## What it does

1. **resolve-version** — resolves the requested `@anthropic-ai/claude-code`
   npm dist-tag (default `latest`) or exact version to a concrete version
   string, so every artifact this run produces is tagged with a real semver,
   never a moving `latest` alias.
2. **build-push** — cross-compiles the nexus binaries (`agentfunnel`,
   `nexus-issue-mcp`, `nexus-jira-mcp`, `nexus-comms-mcp`, `cw`) for both
   `linux/amd64` and `linux/arm64`, then `docker buildx build --platform
   linux/amd64,linux/arm64` from `deploy/worker/Dockerfile` with
   `--build-arg CLAUDE_CODE_VERSION=<resolved>`, and pushes a multi-arch
   manifest to GHCR as `nexus-runner:cli-<ver>` (and `:latest`).
3. **distribute** — pre-loads the freshly-built image onto every cluster
   node's k3s containerd namespace directly from GHCR (`k3s ctr images
   pull` + re-tag to `localhost/nexus-runner:...`), one matrix leg per node
   (dMon = amd64, robo-dog = arm64). This is what makes `imagePullPolicy:
   Never` (see `deploy/worker/job.yaml` / `runtime/dispatch/jobspec.go`)
   still work after a rebuild — see "PullNever node distribution" below.

## The PullNever node-distribution problem (the audit finding)

`runtime/dispatch.BuildJob` sets `ImagePullPolicy: PullNever` on both the
`builder` container and the `codex-auth` init container (today, a
single-node dev posture: `deploy/worker/build.sh` does `podman build` +
`k3s ctr images import` by hand, on dMon only). That's fine for one node; it
silently breaks the moment a Job schedules onto a SECOND node (robo-dog)
that has never had that exact image tag imported — the kubelet just fails
the pod with `ErrImageNeverPull`, no clear signal back to the operator.

Two ways to fix this once there's more than one node, and this workflow
takes the first:

1. **Pre-load every node's containerd from CI (chosen here).** The
   `distribute` job SSHes to each node and runs `k3s ctr images pull` +
   `ctr images tag` straight from GHCR, so the actual dispatch-time pull
   policy stays `PullNever` — no runtime dependency on GHCR (or any
   registry) at dispatch time, only at CI/deploy time. This matches the
   "sovereignty" posture (minimize the load-bearing external dependency
   surface) over a live-pull posture, at the cost of an explicit
   distribution step whenever a new tag needs to reach a new/rebuilt node.
2. **Switch to a registry-pull policy** (`imagePullPolicy: IfNotPresent` or
   `Always`, with an `imagePullSecrets` entry for GHCR) if the fleet grows
   past a couple of always-on nodes and per-node pre-load becomes the
   bottleneck. This is a `runtime/dispatch/jobspec.go` change (the
   `ImagePullPolicy` fields are currently hardcoded `PullNever`) — out of
   scope for this unit; flagging it here as the documented alternative the
   §7 build spec calls out.

Either way, the §7 CLI-version knob (below) only ever **selects a tag**; it
is this workflow's job (or option 2 above) to guarantee that tag actually
exists where the pod schedules.

## The §7 CLI-version knob (how the built image gets used)

`runtime/dispatch.JobConfig.ImageTagPin` (a `func() string`, called fresh on
every `BuildJob`) is the ONE knob PHASE2-DESIGN §7 calls for:

- **Default ("latest built")**: `CW_BUILDER_IMAGE_PIN_FILE` unset (or the
  file empty/missing) → every dispatch uses `cfg.Image`
  (`CW_BUILDER_IMAGE`, whatever the broker's Deployment/CronJob currently
  points at — e.g. `localhost/nexus-runner:latest`, kept current by this
  workflow's `distribute` job re-tagging `:latest` on every successful
  rebuild).
- **Pin on a bad release**: point `CW_BUILDER_IMAGE_PIN_FILE` at a
  ConfigMap-projected volume (e.g. `/etc/nexus-config/image-pin`) and write
  the exact pinned ref (`localhost/nexus-runner:cli-2.1.2`) into that
  ConfigMap key — `kubectl edit configmap/nexus-broker-config` (or
  equivalent). The kubelet syncs a ConfigMap volume to the pod filesystem
  within its sync period (default ~60s, no broker restart, no redeploy) —
  `ImageTagPin` re-reads the file on the very next dispatch.
- **Clear the pin**: blank the ConfigMap key (or delete it) — the next
  dispatch reads `os.ReadFile` failing/empty, falls back to `cfg.Image`
  (`"latest built"`), unchanged.
- `CW_IMAGE_TAG` in every Job's env (feeding the M1 Unit 5 worker-status
  heartbeat's `image_tag` field) always mirrors whichever image actually won
  — the pin when set, `cfg.Image` otherwise — so "which pod is on an old
  CLI" is a heartbeat query, not log archaeology (PHASE2-DESIGN §5/§7).

See `nexus/cmd/nexus/main.go` (`imageTagPinFunc`) and
`runtime/dispatch/jobspec.go` (`JobConfig.ImageTagPin`, `BuildJob`'s `image`
resolution) for the code side of this.

## The §6 frontier-auth source (almanac -> k8s secret delivery)

Not this workflow's job to build, but tightly coupled (the image this
workflow produces is what actually runs `claude` with the injected token) —
see `runtime/dispatch/frontierauth.go`, `nexus/cfgreconcile/frontierauth.go`,
and the top-level unit-7 report for the full almanac-source / k8s-secret-
delivery design and why the dark almanac client wasn't activated wholesale
for this unit.

## Required secrets (if/when enabling this workflow for real)

| Secret | Purpose |
|---|---|
| `NEXUS_CW_PAT` | Clones the private `CarriedWorldUniverse/cw` module to build `cw` for the image (mirrors `deploy/worker/build.sh`'s host-build pattern). |
| `DMON_SSH_HOST` | dMon's SSH host/IP for the `distribute` job's amd64 leg. |
| `ROBODOG_SSH_HOST` | robo-dog's SSH host/IP for the `distribute` job's arm64 leg. |
| `NODE_DEPLOY_SSH_KEY` | Private key authorized on both nodes for the `k3s ctr images pull`/`tag` steps. Scope narrowly (a dedicated deploy key, not the operator's own). |

GHCR push itself uses the workflow-scoped `GITHUB_TOKEN` (no extra secret) —
`packages: write` is already granted at the top of the workflow file.

## Enabling this for real

1. Provision the four secrets above in the repo's Actions secrets.
2. Confirm both nodes' SSH users can run `sudo k3s ctr images pull/tag`
   passwordless (mirrors the existing dMon passwordless-sudo posture — see
   memory `reference_dmon_ssh.md`).
3. Dry-run via `workflow_dispatch` with `distribute: false` first (build +
   push only), inspect the pushed manifest (`docker buildx imagetools
   inspect ghcr.io/.../nexus-runner:cli-<ver>`), THEN re-run with
   `distribute: true`.
4. Point `CW_BUILDER_IMAGE` at `localhost/nexus-runner:latest` (already the
   convention) and confirm a normal dispatch still schedules + runs after
   the first live `distribute` pass.

## Live-verify path (this unit, end to end)

See the top-level unit-7 report / `runtime/dispatch/README.md` for the full
pin -> dispatch -> clear -> dispatch, and kill-the-token -> PreflightAuth
sequences this workflow's output feeds into.
