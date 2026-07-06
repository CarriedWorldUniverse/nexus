# Persona export (M0.2 safety gate) — 2026-07-04

Durable capture of the legacy named-aspect personas before Phase-3 retirement.

**Audit-premise correction:** the audit assumed persona content was stranded behind a *down* `cwb/sqld` pod on a PVC. FALSE. The store is an **embedded libsql inside the live nexus-control broker** (`NEXUS_DB_DSN=http://localhost:8080`, in-process; port 8080 confirmed listening). There is **no on-disk `.db` backing file** for it anywhere in the broker fs (only `ledger.db`), and `NEXUS_ASPECT_DIR=/var/lib/nexus/aspects` does not exist — so the store may be **in-memory / ephemeral** and would be lost on the next broker restart. Hence this export, taken from the live store while up.

**Method:** `nexus personality edit <name> --editor cat` (non-destructive read — cat prints the temp file, writes back unchanged). 11 names captured: anvil, harrow, keel, maren, plumb, shadow, observer, operator, forge, wren, verity.

**Contents:** `personas.txt` = NEXUS.md/SOUL.md/PRIMER.md per aspect. `roster.txt` = `nexus aspect list` (provider/model/version/updated per aspect).

**Migration use (per MIGRATION.md M3):** lift load-bearing LANE knowledge → commonplace base-knowledge (anvil=OSS/versatile builder, plumb=ornith builder, harrow=research/relay, keel=Frame/broker-side, maren=asset-pipeline, forge=AI/model). Cosmetic voice → thin personality labels for chat attribution. NOTE the personas reference an OLDER roster (forge/wren/verity/haft) and Windows paths (`C:\src\...`) — historical, not live topology.
