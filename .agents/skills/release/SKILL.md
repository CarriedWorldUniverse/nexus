---
name: release
description: How to deploy merged work to dMon and verify it live before calling it done.
when_to_use: After a PR merges to main, to get the change running and proven on the cluster.
---

# release

Claiming work is complete without verifying it live is not done. Evidence before claims, always.

## The gate (before any "it's deployed" claim)
1. Identify the command/check that proves the claim.
2. Run it fresh and complete this turn.
3. Read the full output. Check exit code, count failures.
4. Only then make the claim — and state it with the evidence.
Words like "should", "probably", "seems to" mean you haven't verified. Stop and verify.

## Deploy to dMon
1. On dMon, pull `main`, build, install to `/usr/local/bin/nexus`.
2. `kubectl rollout restart` the affected workload; wait for the rollout to complete.
3. Confirm it came up: the broker is listening and aspects reconnect.

## Verify live
1. Run the real check — the live task, conformance, or the specific behaviour you changed.
2. Check `env.health` (pods, PVCs, sqld reachable).
3. For an operator-facing change, dogfood it (or hand the operator the exact steps).
4. Only after the live check passes, move the ticket to Done.

## Rules
- The dMon redeploy is the integration test. A green CI is not the same as "works on the cluster".
- Don't tell the operator it's deployed until you've seen it serving.
