# Robo-dog Sovereign Data Node — design (M0.6 expanded)

**Decision (operator 2026-07-04):** don't just put cairn on robo-dog — **move herald + ledger there too**, co-located on `/data`, tailnet-exposed. robo-dog becomes the sovereign VCS + identity + tracker node (the 1.9T drive); dMon stays control-plane + compute. GitHub = downstream mirror.

## Why this shape
cairn hard-depends on herald (SSH auth: casket fingerprint → agent lookup) + ledger (PR-as-issue) via `grpc.NewClient` (lazy dial — cairn boots without them but auth is dead). A hostNetwork robo-dog pod can't reach cwb ClusterIP services (cross-node flannel doesn't tunnel the tailnet — the ollama precedent). So co-locate cairn's deps ON robo-dog instead of reaching across the boundary.

## The three services (all clean single-container SQLite, from cwb-core)
| Svc | image | DB | data PVC | TLS secret | ports | upstream deps |
|---|---|---|---|---|---|---|
| herald | herald:sha-2a9b631 | /var/lib/herald/herald.db | herald-data 1Gi | herald-tls | 8098 grpc / 8099 http | none (self-contained) |
| ledger | ledger:sha-c2c4c6f | /var/lib/ledger/ledger.db | ledger-data 1Gi | ledger-tls | 8081 | none (self-contained) |
| cairn | cairn:sha-ae4e9a1 | /var/lib/nexus/cairn.db + repos /var/lib/nexus/repos | cairn-data 5Gi | cairn-client-tls | 8102 grpc / 8100 http / 2222 ssh | herald + ledger |

herald/ledger being dependency-free makes them trivially relocatable; cairn slots on top.

## Hard problems found (must solve before deploy)
1. **arm64 images — RESOLVED 2026-07-04.** The deployed sha tags (cairn:sha-ae4e9a1, herald:sha-2a9b631, ledger:sha-c2c4c6f) are **amd64-only** and will NOT run on robo-dog. BUT the **`:main` tag for all three is a proper multi-arch OCI index (amd64+arm64)** — verified by a live `k3s ctr pull` of `ledger:main` on robo-dog (aarch64): pulled clean, `linux/amd64,linux/arm64`. → **Use `:main` images for the sovereign node.** Caveat: `:main` is NEWER than cwb-core's deployed shas, so the robo-dog stack runs slightly ahead — fine for a fresh node, note minor behavior deltas at cutover. No image build needed.
2. **mTLS cert SANs.** `cairn-client-tls` SAN = `cairn.cwb.svc` / `.cluster.local` / `cairn` — NO localhost / tailnet IP. Two fixes:
   - *Internal* (the 3 dialing each other on the same host, hostNetwork → shared localhost): `hostAliases` mapping `herald.cwb.svc`/`ledger.cwb.svc`/`cairn.cwb.svc` → 127.0.0.1 so certs verify.
   - *External* (croft / pool builders over tailnet at 100.92.111.3): either the same hostAlias trick client-side, OR re-issue the cairn cert with a tailnet SAN. Decide.
3. **Data migration.** herald identities + ledger issues live in cwb-core's PVCs on dMon. Copy herald.db + ledger.db + cairn repos to robo-dog /data before cutover (or start fresh — but that loses ledger issue history + herald identities; migrate is safer).
4. **Live-platform surgery.** herald/ledger/cairn are containers in the RUNNING cwb-core monolith; other things depend on them (cairn↔ledger PR flow, the mesh). Cut over consumers, then remove from cwb-core — never rip out first.

## Safe sequence (non-destructive first)
1. Verify/produce arm64 images (pull-test on robo-dog).
2. Lay out `/data/{herald,ledger,cairn}` on robo-dog; copy the TLS secrets + cwb-internal-ca into the target ns.
3. Deploy herald + ledger + cairn on robo-dog (hostNetwork, nodeSelector robo-dog, hostAliases→127.0.0.1, /data hostPath) ALONGSIDE cwb-core's copies — don't touch cwb-core yet.
4. Migrate data (rsync the three DBs + cairn repos from cwb-core PVCs to /data).
5. Expose on tailnet (the node is already 100.92.111.3; cairn-ssh LB pattern exists).
6. Prove: push/pull a repo, SSH auth via herald, PR opens a ledger issue.
7. GitHub mirror CronJob (per-repo mirror push).
8. Cut over consumers (croft remotes, cwb-core's cairn→ledger/herald addrs, the mesh) to the robo-dog endpoints.
9. Remove herald/ledger/cairn containers from cwb-core; retire the dMon PVCs after backup.

Intersects the rebuild: ledger here is ALSO the work-graph store (MIGRATION M1/M2). Build the sovereign node such that nexus's ledger consumption points at THIS ledger.

## STAND-UP PROVEN 2026-07-04 (non-destructive, alongside cwb-core)

All three services deployed on robo-dog and validated — every technical unknown resolved:
- **ledger-rd** (svc ledger-rd:8081) — gRPC serving, fresh ledger.db on /data/sovereign/ledger.
- **herald-rd** (svc herald-rd:8098/8099) — serving; surfaced the REQUIRED identity migration (fresh herald = ephemeral signing key + "genesis skipped, no admin org" → no identities). herald.db + signing key MUST migrate from cwb-core (or re-seed genesis + re-mint), else auth chain is empty.
- **cairn-rd** (ssh:2222 grpc:8102 http:8100) — pointed at the REAL herald.cwb.svc + ledger.cwb.svc, connected over mTLS (cert SANs matched cross-node), serving. Fresh cairn.db + repos on /data. Reuses cairn-secrets/ssh_host_key for stable host identity.

Proven: arm64 :main images boot on GB10; /data hostPath works; mesh TLS certs verify; normal pod networking + cross-node ClusterIP + cluster DNS all work on robo-dog (the ollama hostNetwork was egress-only). The cert-SAN problem DISSOLVED — services use standard .cwb.svc names, existing certs match.

## Remaining sequence (no unknowns; 2 deliberate decisions)
1. **herald identity-root migration (REQUIRED, security-sensitive):** migrate cwb-core herald.db + HERALD_SIGNING_KEY (+ org seed) to /data/sovereign/herald, OR re-seed genesis + re-mint agent identities. DECISION: migrate vs fresh.
2. **ledger + cairn data:** ledger issues (fresh-ok since work-graph is new, or migrate history); cairn repos (fresh + git pull from GitHub, or copy the 1 old repo). DECISION: migrate vs fresh.
3. **Cutover (the clean switch):** flip Service selectors `herald`/`ledger`/`cairn` from app=cwb-core → app=*-rd. Clients keep using .cwb.svc names, certs still match, robo-dog pods serve. Verify each.
4. **Tailnet:** flip cairn-ssh LB selector → cairn-rd (already on robo-dog node 100.92.111.3).
5. **Remove** herald/ledger/cairn containers from cwb-core (DISRUPTIVE: full monolith Recreate — brief CWB restart). Retire dMon PVCs after backup.
6. **GitHub mirror CronJob** (per-repo mirror push).

Nexus-rebuild tie-in: this ledger IS the work-graph store (MIGRATION M1/M2) — point nexus's ledger consumption here.

## herald identity root MIGRATED 2026-07-04 (operator: "migrate")
Copied cwb-core's live herald.db (1.3MB) → robo-dog /data/sovereign/herald (quiesced herald-rd to 0 during copy; single db file, no WAL sidecar). Injected persistent identity config into herald-rd: HERALD_SIGNING_KEY + HERALD_GENESIS_OWNER_PASSWORD (both from herald-secrets), HERALD_ISSUER (dmonextreme gateway, unchanged for token continuity), HERALD_GATEWAY_URL, HERALD_OIDC_CLIENTS. Verified: clean boot (no ephemeral-key warning, no genesis-skip), and DB content — 3 orgs (cwb-admin/carriedworld/docs), 5 users, 10 scope_grants, 2952 refresh_tokens. herald-rd is now the real identity authority. NOTE: HERALD_ISSUER still points at the dMon gateway — revisit if the gateway also moves; for now keeps token `iss` continuity through cutover.

REMAINING: ledger/cairn data (fresh-ok or migrate — minor); cutover (Service selector flips herald/ledger/cairn → *-rd, then cairn-ssh LB); remove 3 containers from cwb-core (disruptive monolith Recreate); GitHub mirror cronjob.

## CUTOVER DONE 2026-07-04 (fresh ledger, migrated cairn repo)
- **cairn data migrated:** full consistent DB set (cairn.db + 4.1MB WAL + shm) + the `doc-store` repo (id 2d1efbec…) copied to /data/sovereign/cairn. (First copy caught a bug — grabbed only the 69KB main db without the WAL where the real data lived; redone with the complete set.) cairn-rd boots clean, serves the repo.
- **ledger:** fresh (work-graph is new).
- **Service cutover (reversible):** flipped selectors `ledger`/`herald`/`cairn`/`cairn-ssh` from app=cwb-core → app=*-rd. Endpoints now the robo-dog pods (10.42.2.x). cairn-ssh LB ext-ip 100.92.111.3 (robo-dog local). Verified: cairn-rd reaches herald.cwb.svc + ledger.cwb.svc (now the -rd pods), repo present.
- **GitHub mirror:** CronJob `cairn-github-mirror` (cwb ns, robo-dog, `23 */6 * * *`) — reads cairn.db for id→slug, pushes each bare repo to `CarriedWorldUniverse/cairn-mirror-<slug>` (auto-creates). PROVEN: created `cairn-mirror-doc-store` on GitHub + pushed main. Script in cm `cairn-mirror-script`, PAT in secret `cairn-mirror-pat`.

## DEFERRED (deliberately): cwb-core container removal
The cwb-core cairn/herald/ledger containers are now ORPHANED (no service points at them) but still running — this is intentional: it's the **instant rollback** (flip selectors back) while the robo-dog stack bakes under real use. Removing them = editing cwb-core deploy → full monolith Recreate (brief CWB-wide blip) + burns the rollback bridge. Do it after a confidence bake, NOT at cutover. Then retire cairn-data/herald-data/ledger-data PVCs on dMon after a backup.

## FOLLOW-UPS
- Commit the -rd Deployments + flipped Services + mirror CronJob to `carriedworld-cloud` (currently live-only = unreproducible, the M0.1 lesson).
- HERALD_ISSUER still points at the dMon gateway — fine for now (token continuity); revisit if the gateway relocates.
- This ledger IS the nexus work-graph store (MIGRATION M1/M2) — point nexus's ledger consumption here.
