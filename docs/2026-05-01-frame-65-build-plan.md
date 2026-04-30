# §6.5 Frame Harness — Build Plan

**Date:** 2026-05-01
**Status:** Draft for operator sanity-check
**Supersedes:** `2026-04-28-frame-role-spec.md` §7 (early outline before STOP decisions + bridle landed)
**Depends on:** STOP decisions locked (`2026-05-01-frame-stop-decisions.md`), bridle v0.1 + patch 1 (built), hand-dispatch v0.1 (built)
**Tracking:** task #78

---

## 1. Scope

Embed the Frame as a global-context aspect inside the Nexus process. The Frame is *the harness* — it consumes bridle as a Go dependency, runs the deliberation loop (#81), exposes admin REST endpoints (#79 lock), and routes chat per the routing rules (#80 lock).

This plan replaces the early §7 outline in the frame-role spec because the surrounding has shifted:
- **bridle exists** → Parts 5/7 (model integration, deliberation) become "wire bridle in" rather than "build a harness from scratch."
- **#79 locked REST-only** → admin endpoint shape is concrete, not deferred.
- **#80 locked routing** → broker delivery rule is concrete, not deferred.
- **Standard MCP client suffices** for P6 (operator #8512). mcphub (#83) is a future optimization, not a build dependency.

---

## 2. Dependency graph

```
P1 Frame role detection ──┐
                          ├──> P5 Frame embedding (normal mode)
P2 Bootstrap HTTP shell ──┤      │
                          │      ├──> P6 Deliberation loop (bridle integration)
P3 Personality templates ─┤      │      │
                          │      │      ├──> P9 Smoke test
P4 Wizard SPA ────────────┘      │      │
                                 │      │
                                 ├──> P7 Admin REST endpoints
                                 │
                                 └──> P8 Chat routing rules
```

P1–P4 unblock P5. P5 unblocks P6/P7/P8 in parallel. P9 closes.

P6 uses a **standard MCP client** to load tools — mcphub (#83) is a future optimization, not a P6 dependency (operator #8512). The Frame's tool surface comes from per-aspect MCP server configs (same shape as the existing agent-network `.mcp.json` pattern). When mcphub lands, the Frame swaps the static-config path for the dynamic mcphub path; until then, static config is fine.

---

## 3. Parts

### P1 — Frame role detection in Nexus startup

**What:** Scan `<nexus_root>/agents/` at startup for an aspect with `aspect.json:role=frame`. Branch into bootstrap-vs-normal mode based on presence/absence. Error on multiple frames.

**Where:** `nexus/cmd/nexus/main.go` startup, new `nexus/frame/detect.go`.

**Tests:** Unit — frame found, no frame, multiple frames (error), malformed aspect.json.

**Estimated size:** ~200 lines including tests. Smaller than the 500–1000 target — fine, it's the foundation everything else hangs on.

---

### P2 — Bootstrap-mode HTTP shell

**What:** When no frame is detected, Nexus comes up in bootstrap mode: HTTP server on the existing port serving a static wizard page + a single `POST /bootstrap/setup` endpoint that writes the new frame's home folder. No aspects spawn. Dashboard's normal SPA does not load in this mode.

**Where:** `nexus/frame/bootstrap.go`, static wizard files under `nexus/frame/bootstrap_static/`.

**Tests:** HTTP — GET wizard page, POST setup with valid input writes folder, malformed input rejected, second POST after frame already exists rejected.

**Estimated size:** ~400 lines.

---

### P3 — Personality templates

**What:** Template Markdown files for SOUL.md / NEXUS.md (formerly CLAUDE.md per #68) / PRIMER.md, with simple `{{placeholder}}` substitution for the wizard's answers. v1 ships one template — "default" — extensible.

**Where:** `nexus/frame/templates/default/*.md.tmpl`, `nexus/frame/templates/render.go`.

**Tests:** Substitution happy path, missing placeholder behavior (fail vs leave-as-is — pick one and pin it), unknown placeholder, all three files render together.

**Estimated size:** ~200 lines + the template files themselves (~300 lines markdown).

---

### P4 — First-boot wizard SPA view

**What:** Dashboard component (or standalone page per the §8 open question — recommend standalone in `nexus/frame/bootstrap_static/`) that asks the operator the questions in frame-role spec §5.3, submits to `/bootstrap/setup`, displays restart progress.

**Where:** `nexus/frame/bootstrap_static/index.html` + JS. Standalone — does not pollute the dashboard SPA with wizard state.

**Tests:** Manual — render in browser, submit form, verify backend received expected payload. Optional Playwright smoke if it's a low-effort add.

**Estimated size:** ~400 lines including HTML + JS + CSS.

---

### P5 — Frame embedding in normal-mode startup

**What:** When a frame is detected, Nexus instantiates it as an in-process aspect during startup. Direct method-call wiring to broker (no WS), broker recognizes the frame as a registered aspect with `admin=true` (already wired per Drift C). Broker routing rules (P8) consult the frame's identity. Frame has its own `aspect.json` parsed at startup like any other aspect.

**Where:** `nexus/frame/embed.go`, integration into `nexus/cmd/nexus/main.go`. Modifies broker's aspect registry to support an in-process aspect (already mostly there per spec §3.4 — verify and extend).

**Tests:** Unit — frame registers in roster, broker routes to frame via in-process channel, admin token resolves. Integration — Nexus starts with frame, frame appears in `/api/aspects`, frame can post to chat.

**Estimated size:** ~600 lines including tests.

---

### P6 — Deliberation loop (bridle integration)

**What:** Wire bridle into the embedded Frame. The Frame is the first concrete consumer of `bridle.Harness`. Implements the funnel-shape deliberation loop (#81): receive comms → triage → run turn(s) via `bridle.RunTurn` → log-decision turn → output to chat. Comms-inbox-as-array for mid-turn steering.

Depends on:
- bridle (✅ shipped)
- triage rules — hard rules ship in P6; cheap-model triage classification deferred to a follow-up if cost characterization isn't ready
- mcphub (#83) — soft dependency. P6 ships against `provider/claude` (direct API). When mcphub lands, swap to `provider/claudecode` for subscription users.

**Where:** `nexus/frame/funnel/` (deliberation loop, triage, log-decision turn), `nexus/frame/funnel_test.go`.

**Tests:**
- Unit — fake bridle Provider scripting a turn sequence; assert deliberation loop runs, comms-inbox folds in, log-decision turn fires.
- Integration — real bridle + real (claude API) provider on a small canned turn. Cost-budgeted; one round-trip.

**Estimated size:** ~800 lines including tests. Largest part — split if it grows past 1000.

---

### P7 — Admin REST endpoints (#79 lock)

**What:** Implement `/api/admin/*` per #79: `rewind`, `compact`, `shutdown`, `dispatch-status`, `roster`. Admin-flag-gated via Drift C/D's existing `admin=true` token check. Long-running ops use `202 Accepted` + operation-id + `GET /api/admin/op/<id>` for status.

**Where:** `nexus/broker/admin.go`, route wiring in `nexus/broker/server.go`.

**Tests:**
- Auth — non-admin tokens rejected with `admin_required`
- Each endpoint round-trip with valid input
- 202+poll pattern for long-running (mock a slow op)
- Error shapes (404 unknown op-id, 409 conflicting state)

**Estimated size:** ~600 lines.

---

### P8 — Chat routing rules (#80 lock)

**What:** Implement broker's frame-routing rule per #80: deliver chat message to frame iff (a) un-addressed, OR (b) frame is participant (@-mentioned, replied-to-frame's-message, frame is thread member). Existing per-aspect routing for non-frame aspects unchanged.

**Where:** `nexus/broker/route.go` (new) extending `nexus/broker/server.go`'s existing dispatch path.

**Tests:**
- Un-addressed message → routed to frame
- @-aspect-not-frame → not routed to frame (unless frame is in thread)
- @-frame → routed to frame
- Reply to frame's message → routed to frame
- Frame previously posted in thread → subsequent messages in thread routed to frame
- Pure aspect-to-aspect addressed → not routed to frame

**Estimated size:** ~400 lines.

---

### P9 — End-to-end smoke

**What:** Fresh Nexus install → bootstrap wizard → restart → frame online → @-mention round-trip → admin shutdown. Manual checklist + a scripted test that reproduces it via HTTP.

**Where:** `nexus/tests/e2e/frame_smoke_test.go`.

**Tests:** End-to-end. One green run per supported config (default template). Documented checklist for variants.

**Estimated size:** ~300 lines.

---

## 4. Sequencing

**Sequential by default** (per the project build workflow). Parallel only where the dependency graph allows.

Suggested order:
1. **P1, P2, P3, P4 in sequence.** P1 is the foundation; P2 needs P1's branching; P3 is the templates P2 writes from; P4 is the UI for P2's endpoint. Could parallelize P3 against P2/P4 since templates are independent — leave that call to whoever drives the cycle.
2. **P5 next.** Single largest unblocker — once the frame embeds, P6/P7/P8 can land independently.
3. **P6, P7, P8 — operator's call on order.**
   - **P7 first** if the operator wants admin tooling earliest (rewind/shutdown via REST is useful for dev anyway).
   - **P8 first** if the operator wants the frame actually receiving chat earliest (without P8, the frame embeds but the broker doesn't deliver to it correctly).
   - **P6 first** if the operator wants the frame *thinking* earliest (most ambitious, depends on bridle which is ready).
   - **My lean:** P8 → P7 → P6. Routing first so the frame gets messages, admin second because it's mostly plumbing and unblocks dev tooling, deliberation last because it's the highest-value/highest-cost part and benefits from having chat working as a test surface.
4. **P9 closes.**

---

## 5. Open questions to resolve at part time

- **P1:** What's the exact `aspect.json` schema field for `role=frame`? Confirm against existing aspect.json shape — extend if needed.
- **P2/P4:** Standalone wizard page vs SPA-gated view — recommend standalone (frame-role spec §8 already leans this way). Confirm at P2 start.
- **P3:** Missing-placeholder behavior — fail-loud vs leave-as-is. Lean: fail-loud, so a bad template is caught at render-time not at runtime when the frame loads it.
- **P5:** Restart vs in-process re-init after wizard — frame-role spec §8 recommends restart. Confirm at P5 start.
- **P6:** Triage classification — cheap-model triage (sidecar bridle session) vs hard-rules-only for v1. Lean: hard-rules-only first, layer cheap-model when the cost is characterized. Defer to part-time decision.
- **P6 MCP client:** use the standard MCP client (operator #8512). Per-aspect static MCP server config (same shape as agent-network's existing `.mcp.json` pattern). mcphub (#83) is a future swap, not a P6 concern.

---

## 6. Out of scope for §6.5

- **Frame federation across Nexuses.** That's frame-to-frame relay (separate spec).
- **Multiple frames per Nexus.** Spec §3.4 — one per Nexus is a deliberate constraint.
- **Hot personality swap.** Folder swap + restart is the supported flow.
- **Aspect task list (#82) & mcphub (#83).** Tracked separately; integrate as follow-up parts when ready.
- **Live admin event WS (`/admin/events`).** Per #79 lock — chat WS already surfaces these via frame's chat posts. Defer until a concrete dashboard view requires it.

---

## 7. Operator check-points

Per the project workflow ("check in at 1/2 and 3/4"):
- After **P3** (1/4): the foundation + bootstrap path. Sanity-check before building the wizard UI.
- After **P5** (4/9 ≈ 1/2): the embedding works. Sanity-check before parallel cycle on P6/P7/P8.
- After **P8** (7/9 ≈ 3/4): routing wired, admin done, deliberation in flight. Final check before closing P9.

---

## 8. Reviewer prompts

Each part gets a `feature-dev:code-reviewer` pass per project workflow. Per-part focus:

- **P1:** edge cases on aspect.json parsing; multi-frame error reporting clarity.
- **P2:** input validation, path traversal, atomic folder write (no half-written homes if interrupted).
- **P3:** template injection (operator input lands in markdown — escape considerations), placeholder enumeration completeness.
- **P4:** XSS on form inputs, error display, restart progress UX.
- **P5:** in-process aspect lifecycle, broker registry consistency, admin-token reconciliation interaction with Drift C.
- **P6:** bridle contract usage, deliberation loop termination, log-decision-turn cost behavior, retry policy on bridle errors.
- **P7:** admin gate enforcement, op-id collision, status-poll race conditions.
- **P8:** routing rule completeness, false-positive rate (frame should NOT receive aspect-to-aspect addressed), thread-member tracking.
- **P9:** real failure modes (fresh-install, half-installed, wizard-aborted-restart-still-runs).

Filter instructions for reviewers: blockers first, importants second, nits last; skip "add more tests" if coverage is solid.
