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
