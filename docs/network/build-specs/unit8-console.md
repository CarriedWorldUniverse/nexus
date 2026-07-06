# M1 Unit 8 — Operator console v0 (build spec)

**Goal:** ONE deliberately-boring server-rendered surface with two panes — the operator's approval queue + the fleet/graph status. Not the Phase-5 UI; the minimal read+approve surface over what exists. Ref: PHASE2-DESIGN §9a.

## Two panes
1. **Approval queue** — docs in `awaiting_approval` (unit-2 `GET /api/docs?status=awaiting_approval`), each rendered (markdown → HTML), inline-editable, with three verdict buttons wiring to unit-2's operator-only endpoints: `POST /api/admin/docs/{id}/approve`, `.../approve-with-changes` (sends edited MD), `.../reject` (sends reasons).
2. **Fleet + graph status** — the worker fleet from unit-5 `GET /api/admin/workers` (state, role, work_item, last_heartbeat, auth_ok, cli_version), and the work-graph (ready/dispatched/done items — read via the workgraph adapter or a status endpoint; if none exists, a simple broker endpoint that lists work-items by status).

## Design constraints (learn from the admin-htmx-ui we reviewed + rejected)
- **`requireAdmin` on EVERY route** (the console + its data) — operator-only. This is the whole point; do NOT use bare `b.auth`.
- **Vendor HTMX** (inline or embedded `static/`), NOT a CDN `<script>` — sovereignty; must work air-gapped.
- Server-rendered, HTMX-style, minimal CSS. Boring by design. No SPA.
- Serve the embedded static correctly (the admin-htmx-ui bug: `//go:embed` FS rooted wrong so the shell 404'd — get the sub-FS right, test that the shell actually serves).
- Escape all interpolated content (`html/template`, not raw `fmt.Fprintf` into HTML) — the admin-htmx-ui XSS lesson.

## Touchpoints
- `nexus/broker/` — a new console handler + embedded `static/console/` (index.html + vendored htmx + minimal css), registered on the mux under `requireAdmin`. Reuse unit-2's `/api/docs` + unit-5's `/api/admin/workers` for data (the console is mostly a rendering layer over existing JSON).

## Constraints
- cairn line `builder/m1-unit8-console` off `builder/m1-finalwave` (has doc register + worker status + everything). `cairn commit`, no push.
- requireAdmin everywhere; vendored htmx; html/template escaping; correct embed sub-FS.

## Acceptance
1. `go build ./...` + `go vet` clean; existing tests pass.
2. Unit tests: the console shell serves (non-404 — explicitly test the embed path, the admin-htmx-ui failure); routes reject non-admin (requireAdmin); the approval-queue pane renders awaiting_approval docs; verdict buttons POST to the right unit-2 endpoints; content is html/template-escaped (a doc title with markup doesn't execute).
3. README: the two panes, the endpoints consumed, the requireAdmin/vendored-htmx/escaping decisions (explicitly noting they fix the admin-htmx-ui review findings), the live-verify path.
4. Document the live-verify path (open /console as admin → see awaiting-approval docs + fleet rows → approve a doc → status flips).
