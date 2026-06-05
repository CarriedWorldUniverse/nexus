# Builder-agent worker runtime (NEX-436 / M2)

On-demand "builder" agents: a dispatch spawns a k8s pod that runs **as the
named agent**, drains the brief from its inbox, does the work, pushes + opens
the PR as that agent, posts the result, and exits on `task_done`. See
`docs/2026-06-05-m2-builder-worker-runtime-design.md`.

## Files
- `Dockerfile` — the shared worker image (toolchain + provider CLIs + agentfunnel + nexus MCP servers + cw; no baked secrets).
- `build.sh` — build the image + load it into single-node k3s (no registry: `podman build` → `k3s ctr import`).
- `storage.yaml` — host-backed RWX PV/PVC (`/work` + caches) on the dMon node.
- `job.yaml` — the builder Job template (`${AGENT}`/`${TASK_ID}` via envsubst).

## Deploy (dMon)
```bash
bash deploy/worker/build.sh                      # build image + import to k3s
sudo kubectl create namespace nexus
sudo kubectl apply -f deploy/worker/storage.yaml # PVC → Bound
```

## Proven (2026-06-05)
- Image **builds** (`localhost/nexus-builder:dev`, ~1.07 GB): `codex-cli 0.137.0`, `go1.26.3`, agentfunnel + nexus-{issue,jira,comms}-mcp + cw, git.
- Image **imported into k3s** and **runs in the cluster** — a smoke pod confirmed all tools present + executable.
- Namespace `nexus` + storage PVC **Bound** (RWX).

## Live DoD — remaining steps (next pass)
A full builder run (anvil drains a real brief → PR → exit) still needs:
1. **Agent identity / keyfile Secret** — `aspect-keyfile-${AGENT}` from the agent's keyfile. Use a **non-contending** identity: the dMon aspects are always-on systemd, so a builder validating as the same name fights its WS slot. Mint a builder identity or stop the systemd aspect for the run.
2. **codex-pod-auth** — the standalone codex normally uses `~/.codex/auth`; in a pod, supply `OPENAI_API_KEY` (brokered via the M1 seam, `kind=provider`) or mount the codex auth. Confirm the standalone codex honours `OPENAI_API_KEY`/`OPENAI_BASE_URL`.
3. **Broker reachability** — handled by the `hostAliases` in `job.yaml` (maps the broker's tailnet name → node IP so both DNS and the TLS cert hostname resolve).
4. **Git credential** — grant the agent a scoped git cred via M1 (`cw credential issue-git-permission`).
5. **Dispatch a brief** to the agent's inbox, then `AGENT=<a> TASK_ID=$(date +%s) envsubst < job.yaml | kubectl apply -f -`; watch it validate → work → push → `task_done` → Completed.
