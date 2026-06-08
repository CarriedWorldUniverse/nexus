# Issue triage + sequencing — nexus repo SOLID/security sweep

**Filed:** 2026-05-05
**Author:** keel
**Reviewing:** plumb's 22-issue audit (#20-#41)
**Decision:** operator wants per-repo issues used as work-tracking surface; comms-tickets stay for cross-repo coordination

---

## 1. Summary by severity

| Severity | Count | Issues |
|----------|-------|--------|
| CRITICAL | 1 | #20 |
| HIGH | 9 | #21, #22, #23, #24, #25, #26, #27, #28, #29 |
| MEDIUM | 11 | #30, #31, #32, #33, #34, #35, #36, #37, #38, #39, #40 |
| LOW | 1 | #41 |

Per operator (#9644): Outpost (#20, #33) deferred — Outpost-routed aspects aren't on the cutover-critical path. **Effective scope drops to 20 issues.**

## 2. Categorization by theme

Five themes drive the fixes. Grouping them this way matters because issues in the same theme touch overlapping files; landing them together avoids merge churn.

### 2A. Auth + identity hardening
**Issues:** #22, #23, #24, #31, #32

Cluster around `nexus/broker/auth.go`, `nexus/broker/server.go`, `nexus/broker/ws.go`. The thread: legacy master token bypasses identity checks (#31), missing identity check on deregister (#32), constant-time token compare needed (#24), origin/TLS gaps (#22, #23).

These are the **cutover security floor**. Without them, "broker on the tailnet" is unsafe even between trusted aspects.

### 2B. Lifecycle + cancellation
**Issues:** #26, #27, #28, #29, #40, #41 (item 2)

`runtime/agent/agent.go`, `runtime/agent/turn.go`, `nexus/broker/ws.go`, `nexus/autospawn/autospawn.go`. Thread: contexts not honored (#27, #39), polling instead of event channels (#29), goroutines unbounded or leaked (#26, #28, #41), broker can't shut down (#40).

Some of these (especially #26 child-leak and #28 unbounded goroutines) are silent-resource-leak class — same shape of bug as the silent-write incident. They produce no errors until the system runs out of handles or the process dies. Cutover floor for stability.

### 2C. Resource limits / DoS resistance
**Issues:** #25, #34

`/connect` has no rate limit (#25), broker tolerates infinite garbage frames (#34). Both are amplifier-shape problems — small attacker effort, large broker-side cost. Cutover floor because the broker faces aspects on tailnet (semi-trusted), not just localhost.

### 2D. Error-handling correctness
**Issues:** #35, #37, #38, #39

Errors get swallowed (#38 claude-api JSON parse, #39 tree API ignores ctx) or echo too much (#35 provider error round-trip). FTS5 (#37) is half error-handling, half input-validation. These are the spec-violating-API-contract class — fixing them is straightforward but the bugs are real.

### 2E. Tech-debt / structural
**Issues:** #30, #36, #41 (item 1)

aspect.json scanning trust model (#30 — security-flavored but the fix is configuration), Provider interface ISP violation (#36), dead time import (#41 item 1).

#30 is more nuanced than pure tech-debt because it touches autospawn's env-inheritance — same surface as #31 (legacy token in env). Should land alongside auth work.

## 3. Sequencing into landable PRs

Goal: minimize merge churn, land high-impact stuff first, allow plumb to review each PR independently.

### PR series

**PR-A — Auth + identity hardening floor** (5 issues)
- #24 ResolveToken constant-time
- #32 deregister identity check
- #31 legacy master token (default off + warn-on-use; admin carve-out audit log)
- #22 WS origin allowlist
- #23 TLS for non-loopback bind

Files touched: `nexus/broker/auth.go`, `nexus/broker/ws.go`, `nexus/broker/server.go`, `nexus/cmd/nexus/main.go`. Co-located. ~150-200 LOC.

Rationale for grouping: all touch the auth surface, and the legacy-token fix needs the constant-time fix and the deregister check to be correct first.

**PR-B — Resource limits + DoS resistance** (2 issues)
- #25 connection cap + per-IP rate limit on /connect, queue-depth cap
- #34 garbage-frame counter + auto-close

Files: `nexus/broker/ws.go`, `nexus/broker/server.go`, `nexus/handqueue/queue.go`. ~100 LOC.

Lands after PR-A so the rate-limit code can use the auth-hardened path.

**PR-C — Lifecycle + cancellation** (5 issues)
- #26 cmd.Wait reaper for autospawn children
- #27 agent turn handler honors lifecycle ctx
- #28 projection bounded queue (single consumer)
- #29 wsclient connect-event channel; reset registered on disconnect
- #40 broker shutdown calls HandQueue.Shutdown

Files: `runtime/agent/agent.go`, `runtime/agent/turn.go`, `nexus/autospawn/autospawn.go`, `runtime/wsclient/wsclient.go`, `nexus/broker/server.go`. ~200-250 LOC.

Largest PR. Could split into PR-C1 (server-side: #26, #40) and PR-C2 (agent-side: #27, #28, #29) if review wants smaller chunks. Operator/plumb choose.

**PR-D — Error-handling correctness** (3 issues, FTS5 separate)
- #38 claude-api JSON parse error returned not swallowed
- #39 tree API honors context
- #35 provider error redaction at dispatch boundary

Files: `runtime/providers/claude-api/claude.go`, `runtime/context/tree/tree.go`, `runtime/handexec/handexec.go`. ~80 LOC.

Independent of PR-A/B/C; can land in parallel.

**PR-E — FTS5 input handling** (1 issue)
- #37 quote-and-escape user text into MATCH; degrade-to-LIKE on syntax error

Files: `nexus/knowledge/knowledge.go`. ~30 LOC + tests. Self-contained.

**PR-F — autospawn config trust + env scrubbing** (1 issue)
- #30 dir-name vs config.Name check; env allowlist for spawned children

Files: `nexus/autospawn/autospawn.go`, `nexus/frame/detect.go`. ~50 LOC.

Touches the same file as PR-C (#26) so should land after to avoid conflict, OR be folded into PR-C if reviewer prefers fewer PRs.

**PR-G — Tech-debt cleanup** (2 issues)
- #36 Provider interface split (ChatProvider / EmbeddingProvider / metadata)
- #41 cleanup batch (dead time import, agent select-default)

Files: `runtime/providers/provider.go` + every consumer call site, plus `nexus/broker/dispatch.go`, `runtime/agent/agent.go`. The Provider split touches a lot of files; PR-G should land last to avoid cascading conflicts.

### Recommended order

1. PR-D (smallest, independent, useful immediately)
2. PR-E (small, independent)
3. PR-A (auth floor; biggest leverage)
4. PR-B (depends on PR-A's surface)
5. PR-C (largest; agent-side ctx changes; could split)
6. PR-F (lands after PR-C touches autospawn.go)
7. PR-G (last; touches everything)

This gets the **security + bug floor** in place (PR-A through PR-D, ~12 issues) before any tech-debt churn. Roughly mirrors plumb's severity ranking.

## 4. Cutover-vs-post-cutover cut

Operator's cutover-quality bar (memory `project_cutover_quality_over_speed.md`) suggests:

**Pre-cutover (must land):**
- All HIGH (PR-A: #22, #23, #24 + PR-B: #25 + PR-C: #26, #27, #28, #29 + PR-A: #21 — though #21 needs more thought, see §5)
- Identity-touching MEDIUMs (PR-A: #31, #32 + PR-F: #30)
- Chat-substrate MEDIUMs (#37 FTS5 — chat search relies on it; arguably the storage spec interaction)
- Lifecycle MEDIUMs (#40)

That's PR-A through PR-D, plus PR-E and PR-F. ~17 issues.

**Post-cutover OK:**
- #34 garbage-frame DoS amplifier (real but rate-limited by single-aspect connection cap once PR-B lands; combined effect minor)
- #35 provider error round-trip (audit-trail concern, not crash-class)
- #36 Provider ISP (refactor; behavior unchanged)
- #38, #39 (real bugs but recovery is manual)
- #41 (cosmetic)

Caveat: every "post-cutover OK" item is still a real bug. "Post-cutover" means "doesn't block the cutover gate"; it doesn't mean "ignore forever".

## 5. Issues needing more thought before fix

**#21 (HIGH) aspect-controlled `home` path becomes worker cmd.Dir** — the suggested fix is `validateRegister` requires `home` under a configured allowed-aspects root. But that constrains where aspects live (operator might want aspects under per-user dirs, etc.). Want operator's call on the policy: hard allowlist vs path-prefix-must-match-discovery vs broker-derives-home-from-its-own-discovery (ignoring payload). I'd lean toward "broker derives home" since the payload `home` is unnecessary — the broker scanned `aspect-dir` and knows where to find each aspect. Worth a short discussion before code.

**#30 (MEDIUM) aspect.json scanning trust** — fix is dir-name == config.Name + env scrubbing. Env scrubbing is the bigger change: today autospawn forwards `os.Environ()`. The right env-allowlist depends on what each provider needs. Sensible default: forward `PATH`, `HOME`, `USERPROFILE`, `TEMP`, plus the per-aspect token; block everything else. Want operator's confirmation on the allowlist before code.

**#31 (MEDIUM) legacy master token semantics** — the suggested fix is "default off, warn on use, treat as non-admin". Three sub-decisions:
1. Default-off means existing deployments break on upgrade; need a one-line "set NEXUS_LEGACY_MASTER=1 to opt in" or similar
2. Warn-on-use cadence (every connect? once per minute?)
3. Treating legacy as non-admin breaks the embedded-frame admin-as-anyone path that today depends on it

Current spec for storage abstraction (#140) mandates single-writer. Per-aspect tokens get us to that — but the migration path from "legacy master = admin = bypass" to "no legacy master, all admin via per-aspect with admin flag" needs operator alignment. Probably a 30-min design conversation.

## 6. What I propose for next session

1. Operator + plumb confirm sequencing (or override)
2. Operator answers the three §5 decisions (so I'm not blocked on #21, #30, #31)
3. I start with PR-D (error-handling, fastest win) and PR-E (FTS5)
4. Then PR-A (auth floor)
5. Plumb reviews each PR; I address feedback per-PR rather than batched
6. PR-A → PR-B → PR-C in sequence, with PR-F folded into PR-C if reviewer prefers
7. PR-G last, post-cutover OK

This is roughly a week of focused work alongside the storage spec implementation (#140 parts 1/2/4 are cutover-blockers from THAT spec). Storage spec impl + nexus repo issue PRs run in parallel since they touch different parts of the codebase.

## 7. Open question for operator

**Should I keep ticketing this as comms-tickets, or move issue-tracking entirely to GitHub issues per repo?** Operator's #9644 implied moving to issues. If yes, I'll close cutover-tickets #135-#142 (the ones that map to per-repo work) and use issues going forward. Cross-repo coordination tickets stay in comms (e.g., #142 GH org migration spans multiple repos). Confirm.

## References

- Plumb's review: 22 issues filed 2026-05-04 (#20-#41 on `nexus-cw/nexus`)
- Storage abstraction spec: `2026-05-05-storage-abstraction-spec.md` (related but distinct work)
- Operator's framing: chat #9642, #9644 (2026-05-05)
- memory: `project_cutover_quality_over_speed.md`
