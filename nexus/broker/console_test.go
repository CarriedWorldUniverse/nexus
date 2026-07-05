package broker

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// consoleTestRig wires a Broker with both DocRegister and
// WorkerStatusStore configured, mirroring docregisterTestRig
// (docregister_rest_test.go) and adminWorkersTestRig
// (admin_workers_test.go). One admin token, one non-admin peer token.
type consoleTestRig struct {
	url        string
	adminToken string
	peerToken  string
	docs       *docregister.Register
	workers    *memWorkerStatus
	runs       *memRuns
}

func newConsoleTestRig(t *testing.T) *consoleTestRig {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := docregister.NewSQLStore(db)
	reg := &docregister.Register{Store: store, Content: newFakeDocContent()}
	workers := &memWorkerStatus{}
	runsStore := &memRuns{}

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	r := roster.New()
	b := New(Config{
		Tokens:            tokens,
		Admin:             &AdminCallbacks{},
		DocRegister:       reg,
		WorkerStatusStore: workers,
		RunsStore:         runsStore,
	}, r)

	mux := http.NewServeMux()
	b.registerDocRegisterWorkbench(mux)
	b.registerAdmin(mux)
	b.registerConsole(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &consoleTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		docs:       reg,
		workers:    workers,
		runs:       runsStore,
	}
}

func (rig *consoleTestRig) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", rig.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// createAwaitingApprovalDoc creates a doc and submits it for approval,
// returning its id. title is passed through verbatim so callers can
// exercise escaping with markup-bearing titles.
func (rig *consoleTestRig) createAwaitingApprovalDoc(t *testing.T, title, workItemID, mdContent string) string {
	t.Helper()
	id, err := rig.docs.CreateDoc(t.Context(), docregister.KindSpec, title, workItemID, mdContent)
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if err := rig.docs.SubmitForApproval(t.Context(), id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	return id
}

// --- finding 3: correct embed sub-FS — the shell must actually serve ---

func TestConsole_ShellServesNonNotFound(t *testing.T) {
	rig := newConsoleTestRig(t)
	resp := rig.get(t, "/console/", rig.adminToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /console/ status = %d; want 200 (the exact admin-htmx-ui embed-sub-FS bug: wrong root => 404)", resp.StatusCode)
	}
	body := bodyString(t, resp)
	if !strings.Contains(body, "Nexus operator console") {
		t.Fatalf("shell body missing expected content: %q", body)
	}
}

func TestConsole_StaticAssetsServe(t *testing.T) {
	rig := newConsoleTestRig(t)

	respJS := rig.get(t, "/console/static/htmx.min.js", rig.adminToken)
	defer respJS.Body.Close()
	if respJS.StatusCode != http.StatusOK {
		t.Fatalf("GET /console/static/htmx.min.js status = %d; want 200 (vendored htmx must actually serve)", respJS.StatusCode)
	}
	js := bodyString(t, respJS)
	if !strings.Contains(js, "htmx") {
		t.Fatal("vendored htmx.min.js does not look like htmx source")
	}

	respCSS := rig.get(t, "/console/static/console.css", rig.adminToken)
	defer respCSS.Body.Close()
	if respCSS.StatusCode != http.StatusOK {
		t.Fatalf("GET /console/static/console.css status = %d; want 200", respCSS.StatusCode)
	}
}

// --- finding 1: requireAdmin on every route, not bare b.auth ---

func TestConsole_AllRoutesRejectNonAdmin(t *testing.T) {
	rig := newConsoleTestRig(t)
	id := rig.createAwaitingApprovalDoc(t, "spec title", "wi-1", "# body")

	paths := []string{
		"/console/",
		"/console/static/htmx.min.js",
		"/console/static/console.css",
		"/console/fragments/approvals",
		"/console/fragments/fleet",
	}
	for _, p := range paths {
		resp := rig.get(t, p, rig.peerToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with non-admin (peer) token: status = %d; want 403 admin_required", p, resp.StatusCode)
		}
		respNoToken := rig.get(t, p, "")
		respNoToken.Body.Close()
		if respNoToken.StatusCode == http.StatusOK {
			t.Errorf("%s with no token: status = 200; want rejected", p)
		}
	}
	_ = id
}

// --- approval-queue pane renders awaiting_approval docs ---

func TestConsole_ApprovalsFragmentRendersAwaitingApprovalDocs(t *testing.T) {
	rig := newConsoleTestRig(t)
	id := rig.createAwaitingApprovalDoc(t, "the operator spec", "wi-42", "# Heading\n\nbody text")

	// A draft (never submitted) doc must NOT show up in the queue.
	draftID, err := rig.docs.CreateDoc(t.Context(), docregister.KindSpec, "still drafting", "wi-43", "wip")
	if err != nil {
		t.Fatalf("CreateDoc draft: %v", err)
	}

	resp := rig.get(t, "/console/fragments/approvals", rig.adminToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fragments/approvals status = %d; want 200", resp.StatusCode)
	}
	body := bodyString(t, resp)

	if !strings.Contains(body, "the operator spec") {
		t.Errorf("approvals fragment missing awaiting_approval doc title; body=%s", body)
	}
	if !strings.Contains(body, "id=\"doc-"+id+"\"") {
		t.Errorf("approvals fragment missing doc card for %s", id)
	}
	if strings.Contains(body, "still drafting") {
		t.Errorf("approvals fragment leaked a draft (non-awaiting_approval) doc: %s", draftID)
	}
	// Rendered markdown made it into the pane (goldmark output).
	if !strings.Contains(body, "<h1>Heading</h1>") {
		t.Errorf("approvals fragment did not render markdown to HTML; body=%s", body)
	}

	// --- finding 4: verdict buttons POST to unit-2's operator endpoints ---
	wantApprove := `hx-post="/api/admin/docs/` + id + `/approve"`
	wantApproveChanges := `hx-post="/api/admin/docs/` + id + `/approve-with-changes"`
	wantReject := `hx-post="/api/admin/docs/` + id + `/reject"`
	for _, want := range []string{wantApprove, wantApproveChanges, wantReject} {
		if !strings.Contains(body, want) {
			t.Errorf("approvals fragment missing verdict wiring %q; body=%s", want, body)
		}
	}
}

// --- finding 4 (escaping): a doc title containing markup renders inert ---

func TestConsole_ApprovalsFragmentEscapesDocTitle(t *testing.T) {
	rig := newConsoleTestRig(t)
	maliciousTitle := `<script>alert(1)</script>`
	rig.createAwaitingApprovalDoc(t, maliciousTitle, "wi-99", "harmless body")

	resp := rig.get(t, "/console/fragments/approvals", rig.adminToken)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("approvals fragment contains an UNESCAPED <script> tag from doc title — stored XSS: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("approvals fragment does not contain the escaped form of the title; body=%s", body)
	}
}

// --- fleet pane reads unit-5's worker_status data ---

func TestConsole_FleetFragmentRendersWorkers(t *testing.T) {
	rig := newConsoleTestRig(t)
	if err := rig.workers.Upsert(t.Context(), workerstatus.Status{
		Agent: "anvil", Role: "builder", Personality: "anvil", State: "running",
		Turns: 3, TokensUsed: 12345,
		LastHeartbeat: time.Now(),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resp := rig.get(t, "/console/fragments/fleet", rig.adminToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fragments/fleet status = %d; want 200", resp.StatusCode)
	}
	body := bodyString(t, resp)
	if !strings.Contains(body, "anvil") {
		t.Errorf("fleet fragment missing worker row; body=%s", body)
	}
	if !strings.Contains(body, "Work-graph status") {
		t.Errorf("fleet fragment missing the documented graph-status TODO note; body=%s", body)
	}
	// The consolidated columns the operator asked for: turns + tokens.
	if !strings.Contains(body, "<td>3</td>") {
		t.Errorf("fleet fragment missing turns column value; body=%s", body)
	}
	if !strings.Contains(body, "<td>12345</td>") {
		t.Errorf("fleet fragment missing tokens_used column value; body=%s", body)
	}
	// A fresh heartbeat (just now, state=running) must be classed "fresh".
	if !strings.Contains(body, "state-fresh") {
		t.Errorf("fleet fragment did not mark a fresh worker as state-fresh; body=%s", body)
	}
}

// TestConsole_FleetFragmentClassifiesStaleAndTerminalWorkers exercises
// the row-coloring cues the operator's panel is built around: a
// heartbeat older than the console's ~2min threshold is "stale", and a
// row whose state reports a finished run is "terminal" regardless of
// heartbeat age.
func TestConsole_FleetFragmentClassifiesStaleAndTerminalWorkers(t *testing.T) {
	rig := newConsoleTestRig(t)
	if err := rig.workers.Upsert(t.Context(), workerstatus.Status{
		Agent: "stale-worker", Role: "builder", State: "running",
		LastHeartbeat: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("Upsert stale: %v", err)
	}
	if err := rig.workers.Upsert(t.Context(), workerstatus.Status{
		Agent: "done-worker", Role: "builder", State: "done",
		LastHeartbeat: time.Now(),
	}); err != nil {
		t.Fatalf("Upsert terminal: %v", err)
	}

	resp := rig.get(t, "/console/fragments/fleet", rig.adminToken)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if !strings.Contains(body, "fleet-row-stale") {
		t.Errorf("fleet fragment did not classify an old heartbeat as stale; body=%s", body)
	}
	if !strings.Contains(body, "fleet-row-terminal") {
		t.Errorf("fleet fragment did not classify a done worker as terminal; body=%s", body)
	}
}

// --- "Recently completed" pane reads runs.Store (runtime/dispatch's
//     RunsRecorder spine), since completed worker_status rows are
//     deliberately deleted at JobDone (reap-loop fix) ---

func TestConsole_FleetFragmentEmptyCompletedRunsState(t *testing.T) {
	rig := newConsoleTestRig(t)

	resp := rig.get(t, "/console/fragments/fleet", rig.adminToken)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if !strings.Contains(body, "Recently completed") {
		t.Errorf("fleet fragment missing the Recently completed section heading; body=%s", body)
	}
	if !strings.Contains(body, "No completed runs yet.") {
		t.Errorf("fleet fragment did not render the empty-completed-runs state; body=%s", body)
	}
}

func TestConsole_FleetFragmentRendersCompletedRunsWithOutcomeAndDuration(t *testing.T) {
	rig := newConsoleTestRig(t)
	started := time.Now().Add(-90 * time.Second)
	rig.runs.rows = map[string]runs.Run{
		"run-done": {
			RunID: "run-done", Ticket: "NEX-101", Agent: "anvil-builder",
			Status: runs.StatusComplete, StartedAt: started,
			CompletedAt: started.Add(90 * time.Second), DurationSecs: 90,
		},
		"run-failed": {
			RunID: "run-failed", Ticket: "NEX-102", Agent: "harrow-reviewer",
			Status: runs.StatusFailed, StartedAt: started,
			CompletedAt: started.Add(30 * time.Second), DurationSecs: 30,
		},
	}

	resp := rig.get(t, "/console/fragments/fleet", rig.adminToken)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if !strings.Contains(body, "NEX-101") || !strings.Contains(body, "NEX-102") {
		t.Errorf("fleet fragment missing completed run tickets; body=%s", body)
	}
	if !strings.Contains(body, "anvil-builder") || !strings.Contains(body, "harrow-reviewer") {
		t.Errorf("fleet fragment missing completed run agents; body=%s", body)
	}
	// Role derived via aspects.SplitWorker from the agent name.
	if !strings.Contains(body, "<td>builder</td>") || !strings.Contains(body, "<td>reviewer</td>") {
		t.Errorf("fleet fragment did not derive role from agent name; body=%s", body)
	}
	if !strings.Contains(body, "state-fresh\">complete") {
		t.Errorf("fleet fragment did not badge a complete run as fresh; body=%s", body)
	}
	if !strings.Contains(body, "state-terminal\">failed") {
		t.Errorf("fleet fragment did not badge a failed run as terminal; body=%s", body)
	}
	if !strings.Contains(body, "1m30s") {
		t.Errorf("fleet fragment missing rendered duration; body=%s", body)
	}
	if strings.Contains(body, "No completed runs yet.") {
		t.Errorf("fleet fragment should not render the empty state when completed runs exist; body=%s", body)
	}
}

func TestConsole_FleetFragmentExcludesLiveRunsFromCompletedSection(t *testing.T) {
	rig := newConsoleTestRig(t)
	rig.runs.rows = map[string]runs.Run{
		"run-live": {
			RunID: "run-live", Ticket: "NEX-200", Agent: "anvil-builder",
			Status: runs.StatusRunning, StartedAt: time.Now(),
		},
	}

	resp := rig.get(t, "/console/fragments/fleet", rig.adminToken)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if strings.Contains(body, "NEX-200") {
		t.Errorf("fleet fragment's Recently completed section leaked a still-running run; body=%s", body)
	}
	if !strings.Contains(body, "No completed runs yet.") {
		t.Errorf("fleet fragment should show the empty-completed state when only live runs exist; body=%s", body)
	}
}
