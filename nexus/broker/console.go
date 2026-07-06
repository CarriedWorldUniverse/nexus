// Package broker: the M1 Unit 8 operator console v0 (PHASE2-DESIGN.md
// §9a). One deliberately-boring server-rendered surface, two panes:
//
//  1. Approval queue — docs in awaiting_approval (unit-2's DocRegister),
//     rendered + inline-editable, wired to unit-2's operator-only
//     verdict endpoints (/api/admin/docs/{id}/approve[-with-changes]|reject).
//  2. Fleet + graph status — unit-5's worker_status table
//     (GET /api/admin/workers's data source) plus a work-graph status
//     note (TODO — see registerConsole below).
//
// This is a rendering layer: it reads the same DocRegister/
// WorkerStatusStore the existing REST endpoints already read, and adds
// no new data model of its own.
//
// Four review findings this file exists to NOT repeat (the admin-htmx-ui
// UI we reviewed and rejected — see README.md for the full mapping):
//
//  1. requireAdmin on EVERY route registered here, console shell +
//     static assets + fragments alike — never bare b.auth. The reviewed
//     UI exposed credential-delete to any aspect token; this package
//     gates every single mux.Handle through b.requireAdmin.
//  2. Vendored HTMX (embedded under static/console/, see consoleStaticFS
//     below) — no CDN <script src="https://...">. Must work air-gapped.
//  3. Correct embed sub-FS: consoleStaticFS is fs.Sub'd to
//     "static/console" before being handed to http.FileServer, and
//     console_test.go explicitly exercises GET /console/ end to end to
//     catch a repeat of the reviewed UI's 404-on-embed-root bug.
//  4. html/template (never fmt.Fprintf into HTML) for every dynamic
//     value — doc titles, agent names, etc. are auto-escaped in their
//     HTML context. The one exception, RenderedHTML, is goldmark output
//     with raw-HTML passthrough left at its default (disabled), so
//     markdown bodies can't smuggle live tags either.
package broker

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"time"

	"github.com/yuin/goldmark"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// consoleStaticRawFS holds the console's static assets: the shell HTML,
// vendored htmx + json-enc extension, and the CSS. Rooted at the module
// path (static/console/...); everything under this package reaches into
// it via fs.Sub("static/console") — see registerConsole — which is the
// exact seam the reviewed admin-htmx-ui got wrong (rooted the FS at the
// embed directive's own path and then re-prefixed it again downstream,
// so every asset 404'd).
//
//go:embed static/console/index.html static/console/console.css static/console/htmx.min.js static/console/json-enc.js
var consoleStaticRawFS embed.FS

// consoleTemplatesFS holds the two fragment templates (approvals, fleet).
// Parsed once at package init via template.Must — a bad template is a
// build-time-detectable programmer error, not a runtime condition.
//
//go:embed static/console/templates/*.html.tmpl
var consoleTemplatesFS embed.FS

var consoleTemplates = template.Must(template.ParseFS(consoleTemplatesFS, "static/console/templates/*.html.tmpl"))

// consoleMD renders markdown to HTML for the approval-queue pane.
// goldmark's default configuration leaves raw HTML passthrough
// disabled (html.WithUnsafe is NOT set), so any HTML embedded in a
// document's markdown body is escaped rather than executed — the same
// escaping discipline as the surrounding html/template, extended to
// the one field that legitimately needs to emit markup (the rendered
// doc body) rather than escaped text.
var consoleMD = goldmark.New()

// registerConsole wires GET /console/ (the shell), the two static
// asset routes it references, and the two fragment routes the shell's
// htmx panes load. Every single route here is requireAdmin — including
// the shell and its static assets, unlike the operator dashboard (which
// serves its SPA shell unauthenticated and gates only the WS/API calls
// behind login). That's a deliberate divergence: the spec for this unit
// calls for requireAdmin "on EVERY console + data route", full stop.
//
// Because a bare browser navigation can't attach an Authorization
// header, the shell accepts the admin bearer token once via a ?token=
// query parameter and caches it client-side (see index.html's inline
// script) for every subsequent htmx request. This is a documented v0
// stopgap (README.md) pending a real operator-login/cookie flow —
// not a weakening of requireAdmin, which still runs on every request.
//
// registerConsole is called unconditionally from ListenAndServe (the
// console is useful the moment ANY of DocRegister/WorkerStatusStore is
// configured, and each pane degrades independently when its backing
// config is nil — see the fragment handlers below), mirroring the
// "always mount the shell, gate the data" shape of the dashboard SPA.
func (b *Broker) registerConsole(mux *http.ServeMux) {
	staticFS, err := fs.Sub(consoleStaticRawFS, "static/console")
	if err != nil {
		// Embed is compiled into the binary; a Sub failure here means
		// the go:embed directive above and this path have drifted,
		// which is a build-time-detectable error class. Fail loudly
		// rather than silently drop the console (the exact "static
		// assets missing, in production" failure mode we're avoiding).
		panic("broker: console static embed sub failed: " + err.Error())
	}
	fileSrv := http.FileServer(http.FS(staticFS))

	mux.Handle("GET /console/", b.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, staticFS, "index.html")
	})))
	mux.Handle("GET /console/static/", b.requireAdmin(http.StripPrefix("/console/static/", fileSrv)))

	mux.Handle("GET /console/fragments/approvals", b.requireAdmin(http.HandlerFunc(b.handleConsoleApprovalsFragment)))
	mux.Handle("GET /console/fragments/fleet", b.requireAdmin(http.HandlerFunc(b.handleConsoleFleetFragment)))
}

// --- approval-queue fragment ---

// consoleDocRow is the template-facing shape for one awaiting_approval
// document. RenderedHTML is pre-sanitized markdown (see consoleMD);
// every other field is a plain string/int rendered through
// html/template's normal auto-escaping.
type consoleDocRow struct {
	ID           string
	Kind         string
	Title        string
	Version      int
	WorkItemID   string
	RawMD        string
	RenderedHTML template.HTML
}

type consoleApprovalsData struct {
	AdminID string
	Docs    []consoleDocRow
}

// handleConsoleApprovalsFragment renders the approval-queue pane: every
// document currently awaiting_approval in unit-2's register, its
// content rendered from markdown, and a form whose three buttons POST
// straight to unit-2's operator-only verdict endpoints
// (/api/admin/docs/{id}/approve[-with-changes]|reject — see
// docregister_rest.go). This handler only reads; it never mutates the
// register itself, keeping the "rendering layer over existing
// endpoints" property from the spec.
func (b *Broker) handleConsoleApprovalsFragment(w http.ResponseWriter, r *http.Request) {
	data := consoleApprovalsData{}
	if info, ok := AuthUserFromContext(r.Context()); ok {
		data.AdminID = info.AgentID
	}
	if b.cfg.DocRegister == nil {
		data.Docs = nil
		b.renderConsoleFragment(w, "approvals.html.tmpl", data)
		return
	}
	docs, err := b.cfg.DocRegister.ListDocs(r.Context(), docregister.ListFilter{Status: docregister.StatusAwaitingApproval})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list docs: "+err.Error())
		return
	}
	rows := make([]consoleDocRow, 0, len(docs))
	for _, d := range docs {
		raw, err := b.cfg.DocRegister.GetContent(r.Context(), d.ID)
		if err != nil {
			// A single doc's content failing to fetch shouldn't blank
			// the whole queue; render it with an empty body and move on.
			raw = ""
		}
		var buf bytes.Buffer
		if err := consoleMD.Convert([]byte(raw), &buf); err != nil {
			buf.Reset()
			buf.WriteString(template.HTMLEscapeString(raw))
		}
		rows = append(rows, consoleDocRow{
			ID:           d.ID,
			Kind:         string(d.Kind),
			Title:        d.Title,
			Version:      d.Version,
			WorkItemID:   d.WorkItemID,
			RawMD:        raw,
			RenderedHTML: template.HTML(buf.String()), // #nosec G203 -- goldmark output, raw HTML passthrough disabled
		})
	}
	data.Docs = rows
	b.renderConsoleFragment(w, "approvals.html.tmpl", data)
}

// --- fleet + graph-status fragment ---

type consoleWorkerRow struct {
	Agent                string
	Role                 string
	Personality          string
	State                string
	WorkItemID           string
	Turns                int
	TokensUsed           int
	LastHeartbeatDisplay string
	AuthOk               bool
	CLIVersion           string
	// StatusClass is a CSS hook the fleet template uses to color-code
	// the state badge: "fresh" (state=running and heartbeat recent),
	// "stale" (heartbeat older than consoleStaleAfter), or "terminal"
	// (state reports a finished/failed run). See classifyWorkerStatus.
	StatusClass string
}

type consoleFleetData struct {
	WorkersConfigured bool
	Workers           []consoleWorkerRow
	RunsConfigured    bool
	CompletedRuns     []consoleCompletedRunRow
	GraphStatusNote   string
}

// consoleRecentRunsLimit caps the "Recently completed" section at the
// last 15 runs (spec: "Last ~15 runs, most recent first"), mirroring
// the runs.Store's own List/ListCompleted clamp-and-default pattern.
const consoleRecentRunsLimit = 15

// consoleCompletedRunRow is the template-facing shape for one completed
// dispatch run in the fleet pane's "Recently completed" history table —
// the run-lifecycle counterpart to consoleWorkerRow, sourced from
// runs.Store (runtime/dispatch's RunsRecorder spine) rather than
// worker_status, since completed worker_status rows are deliberately
// deleted at JobDone (see consoleTerminalStates' doc comment above).
type consoleCompletedRunRow struct {
	Agent            string
	Role             string
	Ticket           string
	Outcome          string // "complete" or "failed" (also covers "cancelled")
	OutcomeClass     string // CSS hook: reuses state-fresh/state-terminal badge styles
	DurationDisplay  string
	CompletedDisplay string // age, e.g. "5m ago"
}

// classifyRunOutcome maps a runs.Status to the fleet pane's outcome
// badge class, reusing the live table's fresh/terminal palette: done
// runs render fresh (green), anything else that reached a terminal
// dispatch status (failed/cancelled) renders terminal (grey/red-ish).
func classifyRunOutcome(status runs.Status) string {
	if status == runs.StatusComplete {
		return "fresh"
	}
	return "terminal"
}

// consoleRunDuration renders a run's wall-clock duration from its
// recorded DurationSecs, falling back to completed-minus-started when
// DurationSecs wasn't recorded (defensive; RecordRunDone always passes
// it today).
func consoleRunDuration(r runs.Run) string {
	d := time.Duration(r.DurationSecs) * time.Second
	if r.DurationSecs == 0 && !r.StartedAt.IsZero() && !r.CompletedAt.IsZero() && r.CompletedAt.After(r.StartedAt) {
		d = r.CompletedAt.Sub(r.StartedAt)
	}
	return d.Round(time.Second).String()
}

// consoleRunAge renders how long ago a run completed, relative to now,
// e.g. "5m ago" — same style as the live table's heartbeat age, just
// phrased for a one-shot completion event rather than an ongoing one.
func consoleRunAge(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return now.Sub(t).Round(time.Second).String() + " ago"
}

// graphStatusTODONote documents the work-graph half of this pane's
// state honestly: no broker-facing read endpoint over the work-graph
// exists yet (nexus/workgraph's adapter is consumed by the
// orchestrator, not wired into this package — see README.md's "wired
// vs TODO" note). The spec explicitly allows this: "if none exists...
// document that pane as reading a TODO endpoint — keep the approval
// pane fully wired regardless." The fleet half above IS fully wired.
const graphStatusTODONote = "Work-graph status (ready/dispatched/done) is not yet wired into the broker — " +
	"see README.md's \"graph-status pane\" note. This pane currently shows fleet status only."

// handleConsoleFleetFragment renders the fleet-status half from
// unit-5's WorkerStatusStore (the exact data GET /api/admin/workers
// serves) plus a documented placeholder for the work-graph half.
func (b *Broker) handleConsoleFleetFragment(w http.ResponseWriter, r *http.Request) {
	data := consoleFleetData{GraphStatusNote: graphStatusTODONote}
	if b.cfg.WorkerStatusStore == nil {
		b.renderConsoleFragment(w, "fleet.html.tmpl", data)
		return
	}
	data.WorkersConfigured = true
	rows, err := b.cfg.WorkerStatusStore.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list workers: "+err.Error())
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastHeartbeat.After(rows[j].LastHeartbeat) })
	now := time.Now()
	out := make([]consoleWorkerRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, consoleWorkerRow{
			Agent:                s.Agent,
			Role:                 s.Role,
			Personality:          s.Personality,
			State:                s.State,
			WorkItemID:           s.WorkItemID,
			Turns:                s.Turns,
			TokensUsed:           s.TokensUsed,
			LastHeartbeatDisplay: formatConsoleTimestamp(s.LastHeartbeat),
			AuthOk:               s.AuthOk,
			CLIVersion:           s.CLIVersion,
			StatusClass:          classifyWorkerStatus(s, now),
		})
	}
	data.Workers = out

	if b.cfg.RunsStore != nil {
		data.RunsConfigured = true
		completed, err := b.cfg.RunsStore.ListCompleted(r.Context(), consoleRecentRunsLimit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list completed runs: "+err.Error())
			return
		}
		rows := make([]consoleCompletedRunRow, 0, len(completed))
		for _, run := range completed {
			role := ""
			if _, rl, ok := aspects.SplitWorker(run.Agent); ok {
				role = rl
			}
			rows = append(rows, consoleCompletedRunRow{
				Agent:            run.Agent,
				Role:             role,
				Ticket:           run.Ticket,
				Outcome:          string(run.Status),
				OutcomeClass:     classifyRunOutcome(run.Status),
				DurationDisplay:  consoleRunDuration(run),
				CompletedDisplay: consoleRunAge(run.CompletedAt, now),
			})
		}
		data.CompletedRuns = rows
	}

	b.renderConsoleFragment(w, "fleet.html.tmpl", data)
}

// consoleStaleAfter is the UI-facing "heartbeat looks stale" threshold
// — a purely cosmetic signal for the operator (color the row, don't
// hide it), distinct from the orchestrator's own reap threshold
// (defaultStaleAfter, 5 minutes, in nexus/orchestrator/orchestrator.go)
// which decides whether to actually requeue a work item. Spec calls
// for "~2min" here.
const consoleStaleAfter = 2 * time.Minute

// consoleTerminalStates are the worker.status `state` values that mean
// "this run is over" rather than "still going". Rows are normally
// retired (deleted) on completion by runtime/dispatch/runner.go's
// OnJobDone, so a terminal row surviving here usually means retirement
// lagged or was skipped — still worth flagging distinctly from a
// merely-stale-but-presumably-still-running row.
var consoleTerminalStates = map[string]bool{
	"done":      true,
	"failed":    true,
	"cancelled": true,
	"canceled":  true,
	"crashed":   true,
	"error":     true,
}

// classifyWorkerStatus buckets one worker row into "fresh", "stale", or
// "terminal" for the fleet pane's state-badge coloring. Terminal wins
// over staleness (a done/failed row is never "just" stale); otherwise a
// heartbeat older than consoleStaleAfter is stale, and anything else —
// including the boot-time "spawning" state — is fresh.
func classifyWorkerStatus(s workerstatus.Status, now time.Time) string {
	if consoleTerminalStates[s.State] {
		return "terminal"
	}
	if s.Stale(now, consoleStaleAfter) {
		return "stale"
	}
	return "fresh"
}

func formatConsoleTimestamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2026-01-02 15:04:05 UTC")
}

// renderConsoleFragment executes the named template into an HTML
// response. html/template auto-escapes every interpolated field per
// its output context, which is the whole point (see package doc,
// finding 4) — this helper exists so both fragment handlers share one
// place that sets the content type and handles execution errors.
func (b *Broker) renderConsoleFragment(w http.ResponseWriter, tmplName string, data any) {
	var buf bytes.Buffer
	if err := consoleTemplates.ExecuteTemplate(&buf, tmplName, data); err != nil {
		writeError(w, http.StatusInternalServerError, "render "+tmplName+": "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}
