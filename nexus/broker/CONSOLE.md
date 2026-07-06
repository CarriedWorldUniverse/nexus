# Operator console v0 (M1 Unit 8)

`PHASE2-DESIGN.md` §9a: one deliberately-boring server-rendered surface,
two panes. Not the Phase-5 UI rethink — the minimal read+approve/read+
status surface over what unit 2 (document register) and unit 5 (worker
status) already built. `nexus/broker/console.go` is the whole
implementation; it adds no new data model, only rendering.

## The two panes

1. **Approval queue** (`GET /console/fragments/approvals`) — every
   document currently `awaiting_approval` in unit-2's `DocRegister`
   (`nexus/docregister`, wired via `Config.DocRegister`), its markdown
   body rendered to HTML, an inline-editable textarea seeded with the
   raw markdown, and three buttons that POST directly to unit-2's
   operator-only verdict endpoints (`docregister_rest.go`):
   - `POST /api/admin/docs/{id}/approve`
   - `POST /api/admin/docs/{id}/approve-with-changes` (sends the
     textarea's edited content as `md_content`)
   - `POST /api/admin/docs/{id}/reject` (sends the reject-reason
     textarea's content as a one-element `reasons` array)

   The fragment handler (`handleConsoleApprovalsFragment`) only reads
   (`ListDocs` + `GetContent`); it never calls a verdict method itself —
   the browser's own POST, driven by htmx, is what invokes unit-2's
   endpoints. This keeps the console a pure rendering layer, and means
   the verdict-buttons acceptance criterion is literally "does the
   rendered HTML contain the right `hx-post` URL", which
   `console_test.go` checks directly.

2. **Fleet + graph status** (`GET /console/fragments/fleet`) — reads
   unit-5's `WorkerStatusStore.List` (`Config.WorkerStatusStore`), the
   exact same data source `GET /api/admin/workers` serves, sorted
   most-recently-heartbeated first: agent, role, state, work_item_id,
   last_heartbeat, auth_ok, cli_version.

   **Work-graph half — wired vs TODO:** `nexus/workgraph`'s adapter
   (ledger-backed work-items, `ListReady` etc.) is not wired into the
   broker package at all today — it's consumed by the orchestrator
   process, not exposed as a broker-level read endpoint, and no
   "list work-items by status" query exists on it yet. Per the build
   spec's explicit allowance ("if none exists... document that pane as
   reading a TODO endpoint — keep the approval pane fully wired
   regardless"), this pane's work-graph half is a documented TODO note
   (`graphStatusTODONote` in console.go) rather than invented plumbing.
   The fleet half above IS fully wired. A follow-up unit that adds a
   `requireAdmin`-gated "list work-items by status" endpoint on the
   workgraph adapter can extend `handleConsoleFleetFragment` to read it
   the same way it reads `WorkerStatusStore` today.

## Routes

All requireAdmin (see "fixes" below):

| Route | Purpose |
|---|---|
| `GET /console/` | the shell page (`static/console/index.html`) |
| `GET /console/static/*` | vendored htmx + json-enc extension + CSS |
| `GET /console/fragments/approvals` | approval-queue pane HTML |
| `GET /console/fragments/fleet` | fleet + graph-status pane HTML |

Each pane degrades to a "not configured" placeholder (not a 404/500)
when its backing config is nil — `DocRegister == nil` renders an empty
approval queue, `WorkerStatusStore == nil` renders "not configured" in
the fleet table — mirroring the rest of this package's "config gates
the surface, absence degrades gracefully" convention.

## The four admin-htmx-ui review findings this fixes

We reviewed and rejected an earlier admin-htmx UI for four specific
bugs. This unit exists to demonstrably not repeat them:

1. **`requireAdmin` on EVERY route, not bare `b.auth`.** The reviewed
   UI exposed credential-delete to any aspect token. Every route in
   `registerConsole` — the shell, the static asset handler, and both
   fragment handlers — is wrapped in `b.requireAdmin`, including the
   shell itself (a deliberate divergence from the operator dashboard,
   which serves its SPA shell unauthenticated and gates only the
   WS/API calls). `TestConsole_AllRoutesRejectNonAdmin` exercises every
   one of these routes with a non-admin (peer) token and with no token
   at all, and asserts none of them succeed.

2. **Vendored htmx, not a CDN `<script src="https://...">`.**
   `static/console/htmx.min.js` (htmx 2.0.10) and
   `static/console/json-enc.js` (the `json-enc` extension, used so the
   verdict buttons can POST JSON bodies matching `docVerdictBody`'s
   shape rather than htmx's default form-encoding) are both vendored
   into the embed, fetched once during development and committed —
   the console has zero runtime dependency on any external host, and
   works air-gapped. `TestConsole_StaticAssetsServe` fetches
   `/console/static/htmx.min.js` through the broker and asserts it
   actually contains htmx source, not a redirect stub.

3. **Correct embed sub-FS.** The reviewed UI's `//go:embed` was rooted
   wrong, so the shell 404'd. `consoleStaticRawFS` embeds
   `static/console/{index.html,console.css,htmx.min.js,json-enc.js}`;
   `registerConsole` calls `fs.Sub(consoleStaticRawFS, "static/console")`
   before handing the sub-tree to `http.FileServer`/`http.ServeFileFS`,
   and panics at startup (a build-time-detectable wiring error, not a
   silent runtime 404) if the `Sub` call itself fails.
   `TestConsole_ShellServesNonNotFound` is the direct regression test:
   it asserts `GET /console/` returns 200 with the expected shell
   content, explicitly named after the bug it guards against.

4. **`html/template`, never `fmt.Fprintf` into HTML.** Both fragments
   (`static/console/templates/{approvals,fleet}.html.tmpl`) are parsed
   with `html/template` and executed with `template.Must(template.ParseFS(...))`
   at package init. Every interpolated field (doc title, agent name,
   role, work-item id, kind, CLI version...) is auto-escaped in its
   HTML context by construction. The one field that legitimately emits
   markup — `RenderedHTML`, the doc body rendered from markdown via
   `goldmark.New()` — is passed through as `template.HTML` deliberately,
   but goldmark's default configuration leaves raw-HTML passthrough
   *disabled* (`html.WithUnsafe()` is never set), so a document body
   that embeds a live `<script>` tag renders it as escaped text, not a
   live tag. `TestConsole_ApprovalsFragmentEscapesDocTitle` creates a
   doc titled `<script>alert(1)</script>`, asserts the raw tag never
   appears unescaped in the rendered fragment, and asserts the escaped
   form (`&lt;script&gt;...`) does.

## The bearer-token stopgap (documented tradeoff, not a fifth finding)

`requireAdmin` resolves identity from an `Authorization: Bearer <token>`
header (`b.auth` → `ExtractBearer`) — there is no cookie/session
machinery anywhere in this broker. A plain browser navigation to
`GET /console/` can't attach a custom header, so the shell accepts the
admin token once as `?token=` on the URL
(`https://<broker>/console/?token=<admin-bearer-token>`), caches it in
`localStorage`, scrubs it from the visible URL via
`history.replaceState`, and attaches it as the `Authorization` header
on every subsequent htmx request (fragment loads and verdict POSTs
alike) via an `htmx:configRequest` listener in `index.html`. This is a
v0 stopgap, not a login flow, and it does not weaken `requireAdmin` —
the header is still checked on every single request, it's simply
sourced from a query param on the one request (the initial navigation)
that has no other way to carry it. A real operator-login/cookie flow
(mirroring the dashboard SPA's WebAuthn login) is the natural Phase-5
follow-up; tokens-in-URLs is an accepted tradeoff for an
operator-only, tailnet-local v0 surface, not something meant to survive
into the full UI rethink.

## Live-verify path (documented, not run in this environment)

1. Bring up a broker with both `Config.DocRegister` and
   `Config.WorkerStatusStore` configured (production wiring:
   `nexus/cmd/nexus/main.go`).
2. Mint or note an admin bearer token (the `frame` identity, or any
   token reconciled with `admin=true`).
3. Create a doc and submit it for approval — e.g.
   `POST /api/docs` with `{"kind":"spec","title":"...","work_item_id":"...","md_content":"# ..."}`
   using ANY authenticated token (the workbench is `b.auth`, not
   `requireAdmin`), then `POST /api/docs/{id}/submit`.
4. Open `https://<broker>/console/?token=<admin-bearer-token>` in a
   browser. The approval-queue pane should show the submitted doc:
   rendered markdown, an editable body, and the three verdict buttons.
   The fleet pane should show any workers currently reporting
   heartbeats via `GET /api/admin/workers`'s data source.
5. Click **Approve** (or edit the body first and click **Approve with
   changes**, or fill in a reason and click **Reject**). The button
   POSTs straight to unit-2's `/api/admin/docs/{id}/...` endpoint; on
   success the approvals pane re-fetches itself (an
   `htmx:afterRequest` handler fires a `refresh-approvals` event the
   pane listens for) and the doc should disappear from the queue —
   its status has flipped out of `awaiting_approval`.
6. Confirm the same `/console/*` routes return 403 `admin_required`
   (or reject outright) when hit with a non-admin bearer token instead.
