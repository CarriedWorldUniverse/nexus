# M1 Unit 4 — Pool leasing + cap (build spec)

**Goal:** a pool of N (=3) interchangeable worker slots leased per-dispatch as derived identities, capped at N concurrent — NOT the current per-agent-name serialization. Ref: PHASE2-DESIGN §4.

## Touchpoints (from the nexus-core audit — verify against current main)
- `runtime/dispatch/runner.go` — `canRun` (~L299), `agentBusy` map (per-agent-name serialization, NEX-464), `liveHands` (~L316), `SpawnMaxConcurrent` (~L98, per-parent cap default 4). ADD a **pool-cap dimension**: at most N (=3, configurable) concurrent POOL leases, distinct from the per-agent-name serialization that `!dispatch <named-agent>` rides (do NOT break named-agent dispatch).
- Derived identities: reuse `aspects.DerivedName`/`freeHandNames` and `MintDerivedCredential` (`nexus/cmd/nexus/main.go` ~L482) — lease a slot as `<pool-parent>.sub-1..N` under a pool parent identity. `spawn.go` `IsDerivedName` (~L103) currently BLOCKS sub-of-sub — ensure pool leasing doesn't trip that.
- The lease lifecycle: acquire on dispatch of a pool work-item, release on job completion (`OnJobDone` ~L361 already frees agents + drains queue — hook the pool-slot release there).

## Design
- A pool = a fixed set of derived-identity slots (`pool.sub-1..3`) leased round-robin/first-free. A pool work-item (role-based, not addressed to a named agent) acquires a free slot; if none free, it queues (existing `queue` + drain).
- Keep the two dispatch modes coexisting: `!dispatch <named-agent>` = per-name serialization (unchanged); pool dispatch (role-based, from the orchestrator) = pool-cap leasing (new).
- Accountability preserved: the leased slot identity + role + work_item stamped on the run (mirror how named dispatch records identity).

## Constraints
- cairn line off main: `builder/m1-unit4-pool-leasing`. `cairn commit`, no push.
- Additive: named-agent dispatch semantics unchanged; pool leasing is the new path. Default pool size 3, configurable (a broker setting/env, read at boot is acceptable).
- Careful: `canRun`'s `agentBusy` map is shared — don't let pool-cap logic break the per-name guarantee for named dispatch.

## Acceptance
1. `go build ./...` + `go vet` clean; `runner_test.go` + `spawn_test.go` still pass.
2. Unit tests: N concurrent pool work-items lease N distinct slots; the (N+1)th queues until one frees (drains on OnJobDone); named-agent dispatch still serializes per name AND coexists with pool leasing; a released slot is reusable.
3. README documenting the pool model, the lease/release lifecycle, and how it coexists with named dispatch.
4. Document the live-verify path (dispatch N+1 pool items, observe N running + 1 queued, then a completion frees the queued one).
