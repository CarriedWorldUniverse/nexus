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

## Required secrets (the distribute leg is wired; it skips cleanly when these are unset)

| Secret | Purpose | Status |
|---|---|---|
| `NEXUS_CW_PAT` | Clones the private `CarriedWorldUniverse/cw` module to build `cw` (org-level). | in place |
| `DMON_SSH_HOST` / `ROBODOG_SSH_HOST` | The nodes' **tailnet** IPs (`100.x`) — a GitHub-hosted runner cannot reach them any other way. | in place |
| `NODE_DEPLOY_SSH_KEY` | Private half of a dedicated ed25519 deploy key. On each node the public half sits in `~jacinta/.ssh/authorized_keys` with `command="/usr/local/bin/nexus-image-distribute",no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty` — the key can do exactly one thing. | in place |
| `NODE_SSH_KNOWN_HOSTS` | Both nodes' `ssh-ed25519` host keys (`ssh-keyscan -t ed25519 <ip>`), so the runner pins them rather than trusting on first use. | in place |
| `TS_OAUTH_CLIENT_ID` / `TS_OAUTH_SECRET` | A Tailscale OAuth client (`auth_keys` write scope, tag `tag:ci`) so the runner can join the tailnet ephemerally via `tailscale/github-action`. Create it in the Tailscale admin console (Settings → OAuth clients); `tag:ci` must exist in the ACL with a `tagOwners` entry allowing the client. | **operator** |

GHCR push and the node-side pull both use the workflow-scoped `GITHUB_TOKEN`:
the package is private and linked to this repo, and the token reaches the
forced command on stdin.

## The node side: `deploy/node/nexus-image-distribute`

Installed at `/usr/local/bin/nexus-image-distribute` on dMon (amd64) and
robo-dog (arm64); the forced command behind the deploy key. It takes the
version from `SSH_ORIGINAL_COMMAND` (strictly validated — anything that is not
a version is refused), the GHCR token from stdin, pulls
`ghcr.io/carriedworlduniverse/nexus-runner:cli-<ver>` for the node's own
architecture with `k3s ctr images pull`, and re-tags it
`localhost/nexus-runner:cli-<ver>` and `localhost/nexus-runner:latest`. It
relies on the existing passwordless `sudo k3s` posture on both nodes.

From any host inside the tailnet that holds the key:

```
printf '%s' "$GHCR_TOKEN" | ssh -i deploy_key jacinta@<node-ip> "2.1.260"
```

## Enabling this for real

1. Everything above except the Tailscale OAuth client is in place. Create that
   client and set `TS_OAUTH_CLIENT_ID` / `TS_OAUTH_SECRET`; until then the leg
   prints a notice and skips.
2. Dry-run via `workflow_dispatch` with `distribute: false`, then re-run with
   `distribute: true` and check `sudo k3s ctr images ls | grep nexus-runner` on
   both nodes.
3. **The broker does not use this image yet.** `CW_BUILDER_IMAGE` on the
   `nexus` Deployment points at `localhost/nexus-builder:li1`, the hand-built
   image from `deploy/worker/build.sh`. Repointing it at
   `localhost/nexus-runner:latest` (and confirming a normal dispatch still
   schedules and runs) is the actual cutover — a deliberate step, since it
   changes which image every builder Job runs.

## Live-verify path (this unit, end to end)

See the top-level unit-7 report / `runtime/dispatch/README.md` for the full
pin -> dispatch -> clear -> dispatch, and kill-the-token -> PreflightAuth
sequences this workflow's output feeds into.
