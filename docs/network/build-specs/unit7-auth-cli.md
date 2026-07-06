# M1 Unit 7 — Frontier auth wiring + CLI version knob (build spec)

**Goal:** frontier seats get a durable, monitored auth token; runner images are current-by-default with one-knob rollback. Ref: PHASE2-DESIGN §6, §7.

## Part A — CLI version knob (§7)
- Runner images are pinned per CLI version (`runner:cli-<ver>`), auto-updater off (pods never self-update mid-job).
- The dispatch jobspec selects the image tag from ONE config knob (a broker setting/env, read at dispatch): default = "latest built"; override = a pinned tag. Touch: `runtime/dispatch/jobspec.go` (`JobConfig`/`BuildJob` — the image is currently `os.Getenv("CW_BUILDER_IMAGE")` read once at boot per the audit; make the tag selectable per-dispatch or via a live-read broker setting). `cli_version`/`image_tag` already flow into the worker-status heartbeat (unit 5) — this closes the loop.
- NOTE the `PullNever` finding: images must be pre-loaded on nodes; the knob just SELECTS a tag, node-distribution is the CI's job (Part C).

## Part B — Frontier auth (§6)
- The `claude-oauth` k8s secret already exists (nexus + croft namespaces, from M0.3) carrying `CLAUDE_CODE_OAUTH_TOKEN`. Inject it into every frontier seat (the orchestrator drain Job + reviewer/security pods when they exist). Touch jobspec to add the secret env for frontier-tier dispatches.
- **Source of truth = almanac** (AUDIT correction: internal platform config, not custodian). The nexus almanac client is written but DARK (gated on unset ALMANAC_GRPC_ADDR). Wire the token to be sourced from almanac `SecureParameter` when ALMANAC_GRPC_ADDR is set, falling back to the k8s secret. If activating the dark almanac client is too broad, document the almanac path precisely + use the k8s secret as delivery now (the secret IS the delivery mechanism either way).
- **Fail-loud**: the orchestrator's PreflightAuth (unit 6) already holds+alerts on auth failure; ensure the token health is checkable (auth_ok/token_expires_at in the worker-status heartbeat — unit 5 reports session-JWT health today; wire the frontier-token expiry if reachable).

## Part C — CI image rebuild (documented deliverable)
- A GitHub Actions workflow (or a documented script) that: on a new claude-code release, builds `runner:cli-<ver>` images (arm64+amd64 — robo-dog is arm64!), and handles node distribution given `PullNever` (import to each node's containerd, or switch to a registry-pull policy). Deliver as a workflow file + README; it doesn't need to run.

## Constraints
- cairn line `builder/m1-unit7-auth-cli` off `builder/m1-unit6-orchestrator`. `cairn commit`, no push.
- Additive: default behavior (no knob set, no almanac) = today's behavior.
- Don't break existing dispatch.

## Acceptance
1. `go build ./...` + `go vet` clean; existing tests pass.
2. Unit tests: jobspec selects the image tag from the knob (default vs pinned); the frontier-token secret env is added for frontier dispatches; almanac-sourced token path (fake almanac) falls back to the k8s secret when almanac absent.
3. A CI workflow file (`.github/workflows/runner-images.yml` or similar) + README covering the knob, the auth source (almanac→secret delivery), and the PullNever node-distribution approach.
4. Document the live-verify path (pin a bad tag → dispatch uses it → clear → uses latest; kill the token → PreflightAuth holds).
