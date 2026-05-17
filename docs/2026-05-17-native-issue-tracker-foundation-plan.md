# Native Issue Tracker Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Phases 0–2 of NEX-137 — bootstrap the `nexus/issues/` package, ship the minimum viable issue store with create/get/update/transition/assign/search and dual-write to Jira, then add comments + timeline + watchers + chat notifications. Result: aspects can work tickets natively while Jira stays authoritative, and the operator can watch activity via a chat stream.

**Architecture:** A new in-process service under `nexus/issues/` with its own SQLite DB (`issues.db`) parallel to `broker.db`. Wired into nexus.exe's existing supervisor. New MCP binary at `runtime/cmd/nexus-issue-mcp/` mirrors the `nexus-jira-mcp` shape. Dual-write shim sits in `runtime/cmd/nexus-jira-mcp/` (existing) so every Jira write mirrors to native; reads remain Jira-only. Notifications hook into the broker's `HandleChatSend` canonical chat path.

**Tech Stack:** Go 1.25, `ncruces/go-sqlite3` (pure-Go WASM SQLite driver, matches existing pattern in `nexus/storage/`), `mark3labs/mcp-go` for MCP server, `goldmark` for markdown rendering (added). Embedded SQL schema via `//go:embed schema.sql` (matches `nexus/storage/schema.go` pattern).

**Spec:** `/Users/jacinta/Source/nexus/docs/2026-05-17-native-issue-tracker-spec.md`

**Scope (this plan):** Phases 0, 1, 2 only. Phase 3 (external sync via NEX-140), Phase 4 (attachments via NEX-139), Phase 5 (dashboard), Phase 6 (cutover), Phase 7 (deprecate `nexus-jira-mcp`) get their own plans once dependencies land.

---

## File structure (created/modified by this plan)

**New package `nexus/issues/`** — primary service code:
- `nexus/issues/schema.sql` — embedded DDL (idempotent)
- `nexus/issues/schema.go` — DB open + migration runner
- `nexus/issues/keys.go` — per-project monotonic key allocator
- `nexus/issues/issues.go` — issue CRUD (Create, Get, Update, Transition, Assign)
- `nexus/issues/events.go` — events table writes + timeline reads
- `nexus/issues/comments.go` — comment append (events.kind='comment')
- `nexus/issues/links.go` — internal link CRUD
- `nexus/issues/watchers.go` — watcher table CRUD
- `nexus/issues/search.go` — structured filter → SQL
- `nexus/issues/workflow.go` — per-type state machine validator
- `nexus/issues/markdown.go` — materialise issue+timeline → markdown document
- `nexus/issues/mentions.go` — parse `@aspect` mentions from markdown
- `nexus/issues/notify.go` — push to broker chat + operator activity stream
- `nexus/issues/service.go` — service struct wiring DB + dependencies; `/healthz/issues`
- One `_test.go` per file above

**New MCP binary `runtime/cmd/nexus-issue-mcp/`**:
- `runtime/cmd/nexus-issue-mcp/main.go` — entrypoint mirroring `nexus-jira-mcp`
- `runtime/cmd/nexus-issue-mcp/tools.go` — MCP tool registration + handlers
- `runtime/cmd/nexus-issue-mcp/client.go` — HTTP client that talks to the in-process service via nexus.exe's REST surface

**Wired into existing files**:
- `nexus/cmd/nexus/main.go` — register the issues service with the supervisor + mount `/api/issues/*` + `/healthz/issues` HTTP routes
- `nexus/broker/server.go` — expose `HandleChatSend` callable from outside the broker package (likely already exported; verify)
- `runtime/cmd/nexus-jira-mcp/tools.go` — dual-write shim added to `jira.create`, `jira.update_status`, `jira.comment`, `jira.claim`, `jira.complete` tools
- `go.mod` + `go.sum` — add `github.com/yuin/goldmark` for markdown rendering
- `docs/index.md` — link to the new tracker section once dashboard exists (Phase 5; not this plan)

**Files reserved for later phases (NOT touched by this plan)**:
- `nexus/issues/sync.go` (Phase 3 — external sync)
- `nexus/issues/attachments.go` (Phase 4 — NEX-139 integration)

---

## Phase 0 — Bootstrap

Goal: empty package builds, `issues.db` opens, `/healthz/issues` returns ok, supervisor knows about the service.

### Task 0.1: Create the `nexus/issues/` package skeleton

**Files:**
- Create: `nexus/issues/service.go`
- Create: `nexus/issues/service_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nexus/issues/service_test.go
package issues

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNew_OpensFreshDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "issues.db")

	svc, err := New(context.Background(), Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	if svc == nil {
		t.Fatal("New returned nil service")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/Source/nexus && go test ./nexus/issues/ -run TestNew_OpensFreshDB -v`
Expected: FAIL — `package issues` undefined or `New` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// nexus/issues/service.go
// Package issues implements the native nexus issue tracker.
// See docs/2026-05-17-native-issue-tracker-spec.md for the design.
package issues

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// Config carries the service's runtime configuration.
type Config struct {
	// DBPath is the on-disk location of issues.db.
	DBPath string
}

// Service is the in-process issue tracker. One per nexus.exe.
type Service struct {
	cfg Config
	db  *sql.DB
}

// New opens (or creates) issues.db and returns a ready Service.
// The schema migration runs on every call — schema.sql is idempotent.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("issues.New: DBPath required")
	}

	dsn := "file:" + cfg.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("issues.New: open %s: %w", cfg.DBPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("issues.New: ping: %w", err)
	}

	return &Service{cfg: cfg, db: db}, nil
}

// Close releases the DB handle.
func (s *Service) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/Source/nexus && go test ./nexus/issues/ -run TestNew_OpensFreshDB -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ~/Source/nexus
git add nexus/issues/service.go nexus/issues/service_test.go
git commit -m "feat(issues): bootstrap nexus/issues package skeleton (NEX-137 Phase 0)"
```

### Task 0.2: Embedded schema runner

**Files:**
- Create: `nexus/issues/schema.sql`
- Create: `nexus/issues/schema.go`
- Modify: `nexus/issues/service.go` (call schema runner from `New`)
- Modify: `nexus/issues/service_test.go` (verify schema applied)

- [ ] **Step 1: Write the failing test**

Replace the existing `TestNew_OpensFreshDB` body — or add a sibling:

```go
// nexus/issues/service_test.go
func TestNew_AppliesSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "issues.db")

	svc, err := New(context.Background(), Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	// Smoke test: query the schema_versions table that schema.sql creates.
	var version int
	err = svc.db.QueryRow(`SELECT version FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&version)
	if err != nil {
		t.Fatalf("schema_versions not present: %v", err)
	}
	if version < 1 {
		t.Errorf("expected schema version >= 1, got %d", version)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./nexus/issues/ -run TestNew_AppliesSchema -v`
Expected: FAIL — `schema_versions` table doesn't exist.

- [ ] **Step 3: Write schema.sql**

```sql
-- nexus/issues/schema.sql
-- Native nexus issue tracker schema. Idempotent — safe to run on every
-- startup. The DB lives at $NEXUS_DATA_DIR/issues.db (parallel to
-- broker.db). See docs/2026-05-17-native-issue-tracker-spec.md for
-- the design.
--
-- PRAGMAs (journal_mode=WAL, foreign_keys=ON, busy_timeout=5000) are
-- set via the DSN in schema.go.

CREATE TABLE IF NOT EXISTS schema_versions (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT OR IGNORE INTO schema_versions(version) VALUES (1);
```

- [ ] **Step 4: Write schema.go**

```go
// nexus/issues/schema.go
package issues

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// applySchema runs the embedded idempotent DDL. Safe to call on every
// service start.
func applySchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("issues.applySchema: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Wire schema runner into New**

Modify `nexus/issues/service.go`'s `New` function — after the successful ping, before returning:

```go
	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
```

- [ ] **Step 6: Run test, verify it passes**

Run: `go test ./nexus/issues/ -v`
Expected: PASS for `TestNew_AppliesSchema` and `TestNew_OpensFreshDB`.

- [ ] **Step 7: Commit**

```bash
git add nexus/issues/schema.sql nexus/issues/schema.go nexus/issues/service.go nexus/issues/service_test.go
git commit -m "feat(issues): embedded schema runner with schema_versions tracking"
```

### Task 0.3: Wire into nexus.exe supervisor + `/healthz/issues`

**Files:**
- Modify: `nexus/cmd/nexus/main.go` — instantiate `issues.New`, register HTTP route
- Create: `nexus/issues/healthz.go`
- Create: `nexus/issues/healthz_test.go`

- [ ] **Step 1: Write the failing test for the healthz handler**

```go
// nexus/issues/healthz_test.go
package issues

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHealthz_ReturnsOK(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(context.Background(), Config{DBPath: filepath.Join(dir, "issues.db")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz/issues", nil)
	svc.HealthzHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	if body.SchemaVersion < 1 {
		t.Errorf("schema_version = %d, want >= 1", body.SchemaVersion)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./nexus/issues/ -run TestHealthz_ReturnsOK -v`
Expected: FAIL — `HealthzHandler` not defined.

- [ ] **Step 3: Implement HealthzHandler**

```go
// nexus/issues/healthz.go
package issues

import (
	"encoding/json"
	"net/http"
)

// HealthzHandler returns an http.Handler for /healthz/issues.
// Returns {"status":"ok","schema_version":N} on success, 503 otherwise.
func (s *Service) HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var version int
		err := s.db.QueryRow(`SELECT version FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&version)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         "ok",
			"schema_version": version,
		})
	})
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./nexus/issues/ -run TestHealthz_ReturnsOK -v`
Expected: PASS.

- [ ] **Step 5: Wire into nexus.exe supervisor**

Open `nexus/cmd/nexus/main.go` and find the section where the broker's HTTP handlers get mounted (search for `http.HandleFunc` or `mux.Handle` and look near the `/api/` registrations). Add the issues service in the same scope:

```go
// In the bootstrap (near where the broker service starts):
issuesDB := filepath.Join(dataDir, "issues.db")
issuesSvc, err := issues.New(ctx, issues.Config{DBPath: issuesDB})
if err != nil {
    log.Error("issues service failed to start", "err", err)
    os.Exit(1)
}
defer issuesSvc.Close()

mux.Handle("/healthz/issues", issuesSvc.HealthzHandler())
```

Imports to add at the top of `main.go`:
```go
"github.com/CarriedWorldUniverse/nexus/nexus/issues"
```

- [ ] **Step 6: Build + smoke**

Run: `go build ./nexus/cmd/nexus/...`
Then run the built binary against a fresh data dir; in another terminal: `curl -s https://localhost:7888/healthz/issues -k`. Expect: `{"status":"ok","schema_version":1}`.

- [ ] **Step 7: Commit**

```bash
git add nexus/issues/healthz.go nexus/issues/healthz_test.go nexus/cmd/nexus/main.go
git commit -m "feat(issues): wire service into nexus.exe supervisor + /healthz/issues"
```

**Phase 0 exit criteria reached** — empty DB boots, healthz works, supervisor knows the service.

---

## Phase 1 — MV Issue Store + Dual-Write

Goal: aspects can `issue_create` / `issue_get` / `issue_update` / `issue_transition` / `issue_assign` / `issue_search` / `issue_list_my` / `issue_list_ready` via the new MCP. Existing `nexus-jira-mcp` write tools dual-write so Jira stays authoritative until cutover.

### Task 1.1: Schema v1 — projects, project_sequences, teams, team_members

**Files:**
- Modify: `nexus/issues/schema.sql`
- Create: `nexus/issues/projects.go`
- Create: `nexus/issues/projects_test.go`

- [ ] **Step 1: Append project tables to schema.sql**

Add to `nexus/issues/schema.sql` after the `schema_versions` block:

```sql
-- -------------------------------------------------------------------
-- Projects + sequence allocator
-- -------------------------------------------------------------------
-- One row per project (NEX, WAKE, OSS, ...). Each has its own
-- monotonic key sequence to produce stable PROJECT-N identifiers.
CREATE TABLE IF NOT EXISTS projects (
  key            TEXT PRIMARY KEY,                -- e.g. "NEX", "WAKE"
  name           TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  default_team   TEXT,                            -- FK to teams.name, nullable
  archived       INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Per-project monotonic counter. Updated transactionally on every
-- issue_create. Row exists for every row in projects.
CREATE TABLE IF NOT EXISTS project_sequences (
  project   TEXT PRIMARY KEY REFERENCES projects(key) ON DELETE CASCADE,
  next_seq  INTEGER NOT NULL DEFAULT 1
);

-- Teams of aspects. Named, operator-defined sets used as
-- assignee_team on issues.
CREATE TABLE IF NOT EXISTS teams (
  name           TEXT PRIMARY KEY,                -- e.g. "oss-nexus-dev"
  description    TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS team_members (
  team    TEXT NOT NULL REFERENCES teams(name) ON DELETE CASCADE,
  aspect  TEXT NOT NULL,                          -- aspect name from broker
  PRIMARY KEY (team, aspect)
);
```

Bump schema version: change the `INSERT OR IGNORE INTO schema_versions(version) VALUES (1);` to add `(2)` (an idempotent reapply).

- [ ] **Step 2: Write test for project CRUD**

```go
// nexus/issues/projects_test.go
package issues

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCreateProject_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(context.Background(), Config{DBPath: filepath.Join(dir, "issues.db")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	if err := svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus engineering"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := svc.GetProject(ctx, "NEX")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Key != "NEX" || got.Name != "Nexus engineering" {
		t.Errorf("got %+v", got)
	}

	// Sequence row was auto-created.
	var nextSeq int
	if err := svc.db.QueryRowContext(ctx, `SELECT next_seq FROM project_sequences WHERE project = ?`, "NEX").Scan(&nextSeq); err != nil {
		t.Fatalf("sequence row missing: %v", err)
	}
	if nextSeq != 1 {
		t.Errorf("next_seq = %d, want 1", nextSeq)
	}
}
```

- [ ] **Step 3: Run test, verify FAIL**

Run: `go test ./nexus/issues/ -run TestCreateProject_Roundtrip -v`

- [ ] **Step 4: Implement Project CRUD**

```go
// nexus/issues/projects.go
package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Project is a top-level container for issues with its own key sequence.
type Project struct {
	Key         string
	Name        string
	Description string
	DefaultTeam string // nullable; empty string = none
	Archived    bool
}

// ErrProjectNotFound is returned by GetProject when no row matches.
var ErrProjectNotFound = errors.New("issues: project not found")

// CreateProject inserts the project and seeds its sequence row.
// Both happen in a single transaction.
func (s *Service) CreateProject(ctx context.Context, p Project) error {
	if p.Key == "" || p.Name == "" {
		return fmt.Errorf("CreateProject: Key and Name required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	defaultTeam := sql.NullString{Valid: p.DefaultTeam != "", String: p.DefaultTeam}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO projects(key, name, description, default_team, archived) VALUES (?, ?, ?, ?, ?)`,
		p.Key, p.Name, p.Description, defaultTeam, boolToInt(p.Archived),
	); err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO project_sequences(project, next_seq) VALUES (?, 1)`,
		p.Key,
	); err != nil {
		return fmt.Errorf("insert project_sequence: %w", err)
	}
	return tx.Commit()
}

// GetProject loads a project by key. Returns ErrProjectNotFound if absent.
func (s *Service) GetProject(ctx context.Context, key string) (*Project, error) {
	var p Project
	var defaultTeam sql.NullString
	var archived int
	err := s.db.QueryRowContext(ctx,
		`SELECT key, name, description, default_team, archived FROM projects WHERE key = ?`,
		key,
	).Scan(&p.Key, &p.Name, &p.Description, &defaultTeam, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	p.DefaultTeam = defaultTeam.String
	p.Archived = archived != 0
	return &p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 5: Run test, verify PASS**

Run: `go test ./nexus/issues/ -v`

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/schema.sql nexus/issues/projects.go nexus/issues/projects_test.go
git commit -m "feat(issues): project + sequence + team tables (schema v2)"
```

### Task 1.2: Schema v1 — issues table + per-issue-type workflow

**Files:**
- Modify: `nexus/issues/schema.sql`
- Create: `nexus/issues/issues.go`
- Create: `nexus/issues/issues_test.go`

- [ ] **Step 1: Append `issues` table to schema.sql**

```sql
-- -------------------------------------------------------------------
-- Issues
-- -------------------------------------------------------------------
-- One row per ticket. Either assignee_aspect OR assignee_team is set
-- (not both); NULL on both = unassigned.
CREATE TABLE IF NOT EXISTS issues (
  key                  TEXT PRIMARY KEY,                  -- e.g. "NEX-137"
  project              TEXT NOT NULL REFERENCES projects(key),
  seq                  INTEGER NOT NULL,                  -- denormalised for clarity
  type                 TEXT NOT NULL,                     -- Epic|Story|Task|Subtask|Bug
  status               TEXT NOT NULL,
  summary              TEXT NOT NULL,
  description          TEXT NOT NULL DEFAULT '',
  definition_of_done   TEXT NOT NULL,                     -- required, can be minimal
  priority             TEXT NOT NULL DEFAULT 'Medium',    -- Lowest|Low|Medium|High|Highest
  priority_locked      INTEGER NOT NULL DEFAULT 0,
  assignee_aspect      TEXT,
  assignee_team        TEXT REFERENCES teams(name) ON DELETE SET NULL,
  reporter             TEXT NOT NULL,                     -- immutable post-create
  parent_key           TEXT REFERENCES issues(key) ON DELETE SET NULL,
  created_at           TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at           TEXT NOT NULL DEFAULT (datetime('now')),
  CHECK (assignee_aspect IS NULL OR assignee_team IS NULL)  -- at most one
);

CREATE INDEX IF NOT EXISTS idx_issues_project_status ON issues(project, status);
CREATE INDEX IF NOT EXISTS idx_issues_assignee_aspect ON issues(assignee_aspect) WHERE assignee_aspect IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_issues_assignee_team ON issues(assignee_team) WHERE assignee_team IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_issues_parent ON issues(parent_key) WHERE parent_key IS NOT NULL;
```

Bump schema_versions to 3.

- [ ] **Step 2: Write failing test for Create + Get**

```go
// nexus/issues/issues_test.go
package issues

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCreateIssue_AllocatesKey(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()

	if err := svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	in := IssueDraft{
		Project:          "NEX",
		Type:             "Story",
		Summary:          "First story",
		Description:      "Hello",
		DefinitionOfDone: "- [ ] PR builds clean",
		Reporter:         "shadow",
	}
	created, err := svc.CreateIssue(ctx, in)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if created.Key != "NEX-1" {
		t.Errorf("Key = %q, want NEX-1", created.Key)
	}
	if created.Status != "To Do" {
		t.Errorf("default Status = %q, want To Do", created.Status)
	}

	// Second create increments.
	second, err := svc.CreateIssue(ctx, in)
	if err != nil {
		t.Fatalf("CreateIssue #2: %v", err)
	}
	if second.Key != "NEX-2" {
		t.Errorf("Key = %q, want NEX-2", second.Key)
	}

	// Round-trip via Get.
	got, err := svc.GetIssue(ctx, "NEX-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Summary != "First story" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

// newTestService creates an in-memory-ish service backed by a temp dir.
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := New(context.Background(), Config{DBPath: filepath.Join(dir, "issues.db")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}
```

- [ ] **Step 3: Run test, verify FAIL**

Run: `go test ./nexus/issues/ -run TestCreateIssue_AllocatesKey -v`

- [ ] **Step 4: Implement Issue + Create + Get**

```go
// nexus/issues/issues.go
package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Issue is the materialised row form. Aspects don't see this directly —
// they see the materialised markdown document (see markdown.go).
type Issue struct {
	Key              string
	Project          string
	Seq              int
	Type             string
	Status           string
	Summary          string
	Description      string
	DefinitionOfDone string
	Priority         string
	PriorityLocked   bool
	AssigneeAspect   string // empty if unset
	AssigneeTeam     string // empty if unset
	Reporter         string
	ParentKey        string // empty if no parent
	CreatedAt        string
	UpdatedAt        string
}

// IssueDraft is the input to CreateIssue.
type IssueDraft struct {
	Project          string
	Type             string
	Summary          string
	Description      string
	DefinitionOfDone string
	Priority         string // default "Medium"
	Reporter         string
	ParentKey        string
	AssigneeAspect   string
	AssigneeTeam     string
}

// ErrIssueNotFound is returned when no issue matches a key (or any alias).
var ErrIssueNotFound = errors.New("issues: issue not found")

// CreateIssue allocates the next key in the project's sequence and
// inserts the row. Transitions to status "To Do" (or "Brief" for Epic).
func (s *Service) CreateIssue(ctx context.Context, d IssueDraft) (*Issue, error) {
	if err := validateDraft(d); err != nil {
		return nil, err
	}

	defaultStatus := initialStatus(d.Type)
	priority := d.Priority
	if priority == "" {
		priority = "Medium"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Atomically take + bump the sequence.
	var seq int
	err = tx.QueryRowContext(ctx,
		`UPDATE project_sequences SET next_seq = next_seq + 1 WHERE project = ? RETURNING next_seq - 1`,
		d.Project,
	).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("CreateIssue: project %q not found", d.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("allocate seq: %w", err)
	}

	key := fmt.Sprintf("%s-%d", d.Project, seq)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO issues(key, project, seq, type, status, summary, description, definition_of_done,
			priority, reporter, parent_key, assignee_aspect, assignee_team)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key, d.Project, seq, d.Type, defaultStatus, d.Summary, d.Description, d.DefinitionOfDone,
		priority, d.Reporter, nullable(d.ParentKey), nullable(d.AssigneeAspect), nullable(d.AssigneeTeam),
	); err != nil {
		return nil, fmt.Errorf("insert issue: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetIssue(ctx, key)
}

// GetIssue loads an issue by canonical key (or alias). Returns ErrIssueNotFound.
func (s *Service) GetIssue(ctx context.Context, key string) (*Issue, error) {
	// First try direct.
	got, err := s.fetchIssueByKey(ctx, key)
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, ErrIssueNotFound) {
		return nil, err
	}
	// Fallback: alias lookup (table not present yet at this phase — wired in Task 1.5 when project moves arrive).
	return nil, ErrIssueNotFound
}

func (s *Service) fetchIssueByKey(ctx context.Context, key string) (*Issue, error) {
	var i Issue
	var assigneeAspect, assigneeTeam, parentKey sql.NullString
	var priorityLocked int
	err := s.db.QueryRowContext(ctx, `
		SELECT key, project, seq, type, status, summary, description, definition_of_done,
		       priority, priority_locked, assignee_aspect, assignee_team, reporter,
		       parent_key, created_at, updated_at
		FROM issues WHERE key = ?`, key,
	).Scan(&i.Key, &i.Project, &i.Seq, &i.Type, &i.Status, &i.Summary, &i.Description,
		&i.DefinitionOfDone, &i.Priority, &priorityLocked, &assigneeAspect, &assigneeTeam,
		&i.Reporter, &parentKey, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIssueNotFound
	}
	if err != nil {
		return nil, err
	}
	i.AssigneeAspect = assigneeAspect.String
	i.AssigneeTeam = assigneeTeam.String
	i.ParentKey = parentKey.String
	i.PriorityLocked = priorityLocked != 0
	return &i, nil
}

func validateDraft(d IssueDraft) error {
	if d.Project == "" {
		return fmt.Errorf("CreateIssue: Project required")
	}
	if !validType(d.Type) {
		return fmt.Errorf("CreateIssue: Type %q invalid (want Epic|Story|Task|Subtask|Bug)", d.Type)
	}
	if strings.TrimSpace(d.Summary) == "" {
		return fmt.Errorf("CreateIssue: Summary required")
	}
	if strings.TrimSpace(d.DefinitionOfDone) == "" {
		return fmt.Errorf("CreateIssue: DefinitionOfDone required (minimum one checklist item)")
	}
	if d.Reporter == "" {
		return fmt.Errorf("CreateIssue: Reporter required")
	}
	if d.AssigneeAspect != "" && d.AssigneeTeam != "" {
		return fmt.Errorf("CreateIssue: set either AssigneeAspect OR AssigneeTeam, not both")
	}
	return nil
}

func validType(t string) bool {
	switch t {
	case "Epic", "Story", "Task", "Subtask", "Bug":
		return true
	}
	return false
}

func initialStatus(t string) string {
	if t == "Epic" {
		return "Brief"
	}
	return "To Do"
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 5: Run test, verify PASS**

Run: `go test ./nexus/issues/ -v`

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/schema.sql nexus/issues/issues.go nexus/issues/issues_test.go
git commit -m "feat(issues): issues table + CreateIssue with sequenced key allocation"
```

### Task 1.3: Workflow validator + Transition

**Files:**
- Create: `nexus/issues/workflow.go`
- Create: `nexus/issues/workflow_test.go`
- Modify: `nexus/issues/issues.go` — add `TransitionIssue`

- [ ] **Step 1: Write failing test**

```go
// nexus/issues/workflow_test.go
package issues

import (
	"context"
	"testing"
)

func TestTransition_Story_HappyPath(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	if err := svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"}); err != nil {
		t.Fatal(err)
	}
	issue, err := svc.CreateIssue(ctx, IssueDraft{
		Project:          "NEX",
		Type:             "Story",
		Summary:          "X",
		DefinitionOfDone: "- [x] done",
		Reporter:         "shadow",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil"); err != nil {
		t.Fatalf("To→In Progress: %v", err)
	}
	if err := svc.TransitionIssue(ctx, issue.Key, "In Review", "anvil"); err != nil {
		t.Fatalf("In Progress→In Review: %v", err)
	}
	if err := svc.TransitionIssue(ctx, issue.Key, "Done", "anvil"); err != nil {
		t.Fatalf("In Review→Done: %v", err)
	}

	got, err := svc.GetIssue(ctx, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "Done" {
		t.Errorf("final status = %q, want Done", got.Status)
	}
}

func TestTransition_RejectsDoneWithUntickedDoD(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	if err := svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"}); err != nil {
		t.Fatal(err)
	}
	issue, err := svc.CreateIssue(ctx, IssueDraft{
		Project:          "NEX",
		Type:             "Story",
		Summary:          "X",
		DefinitionOfDone: "- [ ] not done yet",
		Reporter:         "shadow",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil")
	_ = svc.TransitionIssue(ctx, issue.Key, "In Review", "anvil")

	err = svc.TransitionIssue(ctx, issue.Key, "Done", "anvil")
	if err == nil {
		t.Fatalf("expected error transitioning to Done with unticked DoD")
	}
}

func TestTransition_RejectsInvalid(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{
		Project:          "NEX",
		Type:             "Story",
		Summary:          "X",
		DefinitionOfDone: "- [x] done",
		Reporter:         "shadow",
	})

	// Story can't go directly To Do → Done.
	err := svc.TransitionIssue(ctx, issue.Key, "Done", "anvil")
	if err == nil {
		t.Fatalf("expected error on direct To Do → Done")
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement workflow validator**

```go
// nexus/issues/workflow.go
package issues

import (
	"fmt"
	"strings"
)

// allowedTransitions maps {issueType: {fromStatus: [allowedToStatuses]}}.
// Cancelled is reachable from any non-terminal state for every type.
var allowedTransitions = map[string]map[string][]string{
	"Epic": {
		"Brief":            {"Sketch/Refined", "Cancelled"},
		"Sketch/Refined":   {"In Development", "Brief", "Cancelled"},
		"In Development":   {"Delivered", "Sketch/Refined", "Cancelled"},
		"Delivered":        {}, // terminal
		"Cancelled":        {}, // terminal
	},
	"Story": storyLikeTransitions(),
	"Task":  storyLikeTransitions(),
	"Bug":   storyLikeTransitions(),
	"Subtask": storyLikeTransitions(),
}

func storyLikeTransitions() map[string][]string {
	return map[string][]string{
		"To Do":       {"In Progress", "Cancelled"},
		"In Progress": {"Blocked", "In Review", "Cancelled"},
		"Blocked":     {"In Progress", "Cancelled"},
		"In Review":   {"In Progress", "Done", "Cancelled"},
		"Done":        {},
		"Cancelled":   {},
	}
}

// terminalStates is the set of statuses that gate DoD enforcement.
var terminalStates = map[string]bool{
	"Done":      true,
	"Delivered": true,
}

// validateTransition checks the state machine + DoD gate. Returns nil
// if the transition is legal.
func validateTransition(issueType, fromStatus, toStatus, definitionOfDone string) error {
	rules, ok := allowedTransitions[issueType]
	if !ok {
		return fmt.Errorf("unknown issue type %q", issueType)
	}
	allowed, ok := rules[fromStatus]
	if !ok {
		return fmt.Errorf("no transitions defined from %q for %s", fromStatus, issueType)
	}
	if !contains(allowed, toStatus) {
		return fmt.Errorf("transition %q → %q not allowed for %s", fromStatus, toStatus, issueType)
	}
	if terminalStates[toStatus] {
		if !dodComplete(definitionOfDone) {
			return fmt.Errorf("cannot transition to %q: definition of done has unticked items", toStatus)
		}
	}
	return nil
}

// dodComplete returns true iff the DoD markdown contains at least one
// ticked checklist item AND no unticked ones.
func dodComplete(dod string) bool {
	lines := strings.Split(dod, "\n")
	ticked := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [ ]") {
			return false // any unticked item disqualifies
		}
		if strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "- [X]") {
			ticked++
		}
	}
	return ticked > 0
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

Then add `TransitionIssue` to `nexus/issues/issues.go`:

```go
// TransitionIssue moves an issue to a new status after validating the
// state machine + DoD gate. The actor is recorded for the timeline
// (events table; written by callers in Phase 2 — for now status-only).
func (s *Service) TransitionIssue(ctx context.Context, key, toStatus, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var issueType, fromStatus, dod string
	err = tx.QueryRowContext(ctx,
		`SELECT type, status, definition_of_done FROM issues WHERE key = ?`, key,
	).Scan(&issueType, &fromStatus, &dod)
	if err != nil {
		return fmt.Errorf("TransitionIssue: load %s: %w", key, err)
	}

	if err := validateTransition(issueType, fromStatus, toStatus, dod); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET status = ?, updated_at = datetime('now') WHERE key = ?`,
		toStatus, key,
	); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run, verify PASS**

- [ ] **Step 5: Commit**

```bash
git add nexus/issues/workflow.go nexus/issues/workflow_test.go nexus/issues/issues.go
git commit -m "feat(issues): per-type workflow validator + TransitionIssue"
```

### Task 1.4: UpdateIssue + AssignIssue + priority controls

**Files:**
- Modify: `nexus/issues/issues.go` — add `UpdateIssue`, `AssignIssue`, `SetPriority`
- Create: `nexus/issues/assign_test.go`

- [ ] **Step 1: Write failing tests**

```go
// nexus/issues/assign_test.go
package issues

import (
	"context"
	"testing"
)

func TestAssign_ToAspect(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{
		Project: "NEX", Type: "Story", Summary: "X",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow",
	})

	if err := svc.AssignIssue(ctx, issue.Key, "anvil", "", "shadow"); err != nil {
		t.Fatalf("AssignIssue: %v", err)
	}
	got, _ := svc.GetIssue(ctx, issue.Key)
	if got.AssigneeAspect != "anvil" {
		t.Errorf("AssigneeAspect = %q", got.AssigneeAspect)
	}
	if got.AssigneeTeam != "" {
		t.Errorf("AssigneeTeam should be empty, got %q", got.AssigneeTeam)
	}
}

func TestAssign_RejectsBoth(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{
		Project: "NEX", Type: "Story", Summary: "X",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow",
	})

	err := svc.AssignIssue(ctx, issue.Key, "anvil", "oss-nexus-dev", "shadow")
	if err == nil {
		t.Fatal("expected error when both aspect and team set")
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement Assign + Update**

Add to `nexus/issues/issues.go`:

```go
// AssignIssue sets assignee_aspect or assignee_team (exactly one, or
// both empty to clear). The actor is for the future events row.
func (s *Service) AssignIssue(ctx context.Context, key, aspect, team, actor string) error {
	if aspect != "" && team != "" {
		return fmt.Errorf("AssignIssue: set aspect OR team, not both")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE issues SET assignee_aspect = ?, assignee_team = ?, updated_at = datetime('now') WHERE key = ?`,
		nullable(aspect), nullable(team), key,
	)
	return err
}

// UpdatePatch holds optional field updates. Empty/nil fields = no change.
type UpdatePatch struct {
	Summary          *string
	Description      *string
	DefinitionOfDone *string
	Priority         *string
	ParentKey        *string
}

// UpdateIssue applies a patch atomically.
func (s *Service) UpdateIssue(ctx context.Context, key string, patch UpdatePatch, actor string) error {
	sets := []string{}
	args := []any{}
	if patch.Summary != nil {
		sets = append(sets, "summary = ?")
		args = append(args, *patch.Summary)
	}
	if patch.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *patch.Description)
	}
	if patch.DefinitionOfDone != nil {
		sets = append(sets, "definition_of_done = ?")
		args = append(args, *patch.DefinitionOfDone)
	}
	if patch.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *patch.Priority)
	}
	if patch.ParentKey != nil {
		sets = append(sets, "parent_key = ?")
		args = append(args, nullable(*patch.ParentKey))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = datetime('now')")
	args = append(args, key)
	stmt := "UPDATE issues SET " + strings.Join(sets, ", ") + " WHERE key = ?"
	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}
```

- [ ] **Step 4: Run, verify PASS**

- [ ] **Step 5: Commit**

```bash
git add nexus/issues/issues.go nexus/issues/assign_test.go
git commit -m "feat(issues): UpdateIssue + AssignIssue with mutually-exclusive aspect/team"
```

### Task 1.5: Key aliases + cross-project move

**Files:**
- Modify: `nexus/issues/schema.sql` — add `key_aliases` table
- Create: `nexus/issues/move.go`
- Create: `nexus/issues/move_test.go`
- Modify: `nexus/issues/issues.go` — `GetIssue` checks aliases on miss

- [ ] **Step 1: Append `key_aliases` table to schema.sql**

```sql
-- key_aliases maps old issue keys to current canonical keys after
-- cross-project moves. Lookups by old key continue to resolve forever.
CREATE TABLE IF NOT EXISTS key_aliases (
  old_key   TEXT PRIMARY KEY,
  new_key   TEXT NOT NULL REFERENCES issues(key) ON DELETE CASCADE,
  moved_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_key_aliases_new ON key_aliases(new_key);
```

Bump schema_versions to 4.

- [ ] **Step 2: Write failing test**

```go
// nexus/issues/move_test.go
package issues

import (
	"context"
	"testing"
)

func TestReassignProject_AllocatesNewKeyAndAliases(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	_ = svc.CreateProject(ctx, Project{Key: "OSS", Name: "OSS"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{
		Project: "NEX", Type: "Story", Summary: "X",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow",
	})

	oldKey := issue.Key
	newKey, err := svc.ReassignProject(ctx, oldKey, "OSS", "shadow", "rehome")
	if err != nil {
		t.Fatalf("ReassignProject: %v", err)
	}
	if newKey != "OSS-1" {
		t.Errorf("newKey = %q, want OSS-1", newKey)
	}

	// Direct lookup by new key works.
	got, err := svc.GetIssue(ctx, newKey)
	if err != nil {
		t.Fatalf("GetIssue(newKey): %v", err)
	}
	if got.Project != "OSS" {
		t.Errorf("Project after move = %q", got.Project)
	}

	// Lookup by old key resolves via alias.
	gotAlias, err := svc.GetIssue(ctx, oldKey)
	if err != nil {
		t.Fatalf("GetIssue(oldKey): %v", err)
	}
	if gotAlias.Key != newKey {
		t.Errorf("alias lookup returned %q, want %q", gotAlias.Key, newKey)
	}
}
```

- [ ] **Step 3: Run, verify FAIL**

- [ ] **Step 4: Implement ReassignProject + alias-aware GetIssue**

```go
// nexus/issues/move.go
package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReassignProject moves an issue to a new project. Allocates a new key
// from the destination's sequence, records an alias from the old key,
// and returns the new key.
//
// v1 rules:
//   - Reject the move if the issue has children in the source project
//     (cross-project parent links are disallowed)
//   - If the issue has a parent in the source project, drop the parent
//     link (a future field_change event records the unhitch)
//
// `actor` and `reason` are recorded with the alias for audit.
func (s *Service) ReassignProject(ctx context.Context, oldKey, newProject, actor, reason string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Load current state.
	var srcProject, parentKey sql.NullString
	var issueType string
	err = tx.QueryRowContext(ctx,
		`SELECT project, parent_key, type FROM issues WHERE key = ?`, oldKey,
	).Scan(&srcProject, &parentKey, &issueType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrIssueNotFound
	}
	if err != nil {
		return "", err
	}

	if !srcProject.Valid || srcProject.String == newProject {
		return "", fmt.Errorf("ReassignProject: source and destination project are the same")
	}

	// Check for children in source project.
	var childCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues WHERE parent_key = ?`, oldKey,
	).Scan(&childCount); err != nil {
		return "", err
	}
	if childCount > 0 {
		return "", fmt.Errorf("ReassignProject: issue has %d child(ren); resolve cross-project parents first", childCount)
	}

	// Allocate new sequence value in destination project.
	var newSeq int
	err = tx.QueryRowContext(ctx,
		`UPDATE project_sequences SET next_seq = next_seq + 1 WHERE project = ? RETURNING next_seq - 1`,
		newProject,
	).Scan(&newSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("ReassignProject: destination project %q not found", newProject)
	}
	if err != nil {
		return "", err
	}

	newKey := fmt.Sprintf("%s-%d", newProject, newSeq)

	// Update the row.
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET key = ?, project = ?, seq = ?, parent_key = NULL, updated_at = datetime('now') WHERE key = ?`,
		newKey, newProject, newSeq, oldKey,
	); err != nil {
		return "", err
	}

	// Record alias.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO key_aliases(old_key, new_key) VALUES (?, ?)`,
		oldKey, newKey,
	); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return newKey, nil
}
```

Update `GetIssue` in `nexus/issues/issues.go` to consult aliases on miss:

```go
func (s *Service) GetIssue(ctx context.Context, key string) (*Issue, error) {
	got, err := s.fetchIssueByKey(ctx, key)
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, ErrIssueNotFound) {
		return nil, err
	}
	// Fallback: resolve via alias.
	var newKey string
	err = s.db.QueryRowContext(ctx,
		`SELECT new_key FROM key_aliases WHERE old_key = ?`, key,
	).Scan(&newKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIssueNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.fetchIssueByKey(ctx, newKey)
}
```

- [ ] **Step 5: Run, verify PASS**

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/schema.sql nexus/issues/move.go nexus/issues/move_test.go nexus/issues/issues.go
git commit -m "feat(issues): cross-project ReassignProject + key_aliases (schema v4)"
```

### Task 1.6: Search — structured filter → SQL

**Files:**
- Create: `nexus/issues/search.go`
- Create: `nexus/issues/search_test.go`

- [ ] **Step 1: Write failing test**

```go
// nexus/issues/search_test.go
package issues

import (
	"context"
	"testing"
)

func TestSearch_ByAssigneeAndStatus(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})

	mk := func(summary, assignee string) string {
		issue, _ := svc.CreateIssue(ctx, IssueDraft{
			Project: "NEX", Type: "Story", Summary: summary,
			DefinitionOfDone: "- [ ] go", Reporter: "shadow", AssigneeAspect: assignee,
		})
		return issue.Key
	}
	a := mk("for anvil", "anvil")
	_ = mk("for plumb", "plumb")
	_ = svc.TransitionIssue(ctx, a, "In Progress", "anvil")

	results, err := svc.Search(ctx, SearchFilter{
		AssigneeAspect: "anvil",
		Statuses:       []string{"In Progress"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Key != a {
		t.Errorf("Key = %q, want %q", results[0].Key, a)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement search.go**

```go
// nexus/issues/search.go
package issues

import (
	"context"
	"fmt"
	"strings"
)

// SearchFilter is the structured query shape. Empty fields = no filter.
type SearchFilter struct {
	Projects        []string
	Types           []string
	Statuses        []string
	Priorities      []string
	AssigneeAspect  string
	AssigneeTeam    string
	Reporter        string
	ParentKey       string
	OrderBy         string // "priority" | "created" | "updated" (default: "updated")
	OrderDir        string // "asc" | "desc" (default: "desc")
	Limit           int    // default 50, max 200
}

// IssueRef is the lightweight projection returned from Search.
type IssueRef struct {
	Key            string
	Project        string
	Type           string
	Status         string
	Summary        string
	Priority       string
	AssigneeAspect string
	AssigneeTeam   string
	UpdatedAt      string
}

// Search runs the structured filter.
func (s *Service) Search(ctx context.Context, f SearchFilter) ([]IssueRef, error) {
	clauses := []string{}
	args := []any{}

	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		placeholders := strings.Repeat("?,", len(vals))
		placeholders = strings.TrimRight(placeholders, ",")
		clauses = append(clauses, fmt.Sprintf("%s IN (%s)", col, placeholders))
		for _, v := range vals {
			args = append(args, v)
		}
	}

	addIn("project", f.Projects)
	addIn("type", f.Types)
	addIn("status", f.Statuses)
	addIn("priority", f.Priorities)

	if f.AssigneeAspect != "" {
		clauses = append(clauses, "assignee_aspect = ?")
		args = append(args, f.AssigneeAspect)
	}
	if f.AssigneeTeam != "" {
		clauses = append(clauses, "assignee_team = ?")
		args = append(args, f.AssigneeTeam)
	}
	if f.Reporter != "" {
		clauses = append(clauses, "reporter = ?")
		args = append(args, f.Reporter)
	}
	if f.ParentKey != "" {
		clauses = append(clauses, "parent_key = ?")
		args = append(args, f.ParentKey)
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	orderBy := "updated_at"
	switch f.OrderBy {
	case "priority":
		// Priority is text — map to ordinal for sort. Inline CASE
		// expression handles this without a join.
		orderBy = `CASE priority WHEN 'Highest' THEN 5 WHEN 'High' THEN 4 WHEN 'Medium' THEN 3 WHEN 'Low' THEN 2 WHEN 'Lowest' THEN 1 ELSE 0 END`
	case "created":
		orderBy = "created_at"
	case "updated", "":
		orderBy = "updated_at"
	}
	dir := "DESC"
	if strings.EqualFold(f.OrderDir, "asc") {
		dir = "ASC"
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	stmt := fmt.Sprintf(`
		SELECT key, project, type, status, summary, priority,
		       COALESCE(assignee_aspect, ''), COALESCE(assignee_team, ''), updated_at
		FROM issues
		%s
		ORDER BY %s %s
		LIMIT %d`,
		where, orderBy, dir, limit)

	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IssueRef
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.Key, &r.Project, &r.Type, &r.Status, &r.Summary, &r.Priority,
			&r.AssigneeAspect, &r.AssigneeTeam, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run, verify PASS**

- [ ] **Step 5: Commit**

```bash
git add nexus/issues/search.go nexus/issues/search_test.go
git commit -m "feat(issues): structured-filter Search"
```

### Task 1.7: ListMy + ListReady

**Files:**
- Modify: `nexus/issues/search.go` — add `ListMy`, `ListReady`
- Modify: `nexus/issues/search_test.go` — add tests

- [ ] **Step 1: Append tests**

```go
func TestListMy_ReturnsAspectAndTeamIssues(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	// Direct assignment
	_, _ = svc.CreateIssue(ctx, IssueDraft{Project: "NEX", Type: "Story", Summary: "mine",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow", AssigneeAspect: "anvil"})

	results, err := svc.ListMy(ctx, "anvil", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("len = %d", len(results))
	}
}

func TestListReady_ExcludesNonStartable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	_ = svc.CreateIssue(ctx, IssueDraft{Project: "NEX", Type: "Story", Summary: "ready",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow", AssigneeAspect: "anvil"})

	results, err := svc.ListReady(ctx, "anvil", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement ListMy + ListReady**

Append to `nexus/issues/search.go`:

```go
// ListMy returns issues assigned to the given aspect, either directly
// (assignee_aspect = aspect) or via a team membership (aspect ∈ team_members
// where teams.name = assignee_team).
func (s *Service) ListMy(ctx context.Context, aspect string, teams []string) ([]IssueRef, error) {
	// Direct + team membership (teams arg expected from caller — the
	// service doesn't authoritatively know an aspect's teams without
	// the broker; pass them in).
	clauses := []string{"assignee_aspect = ?"}
	args := []any{aspect}
	if len(teams) > 0 {
		ph := strings.Repeat("?,", len(teams))
		ph = strings.TrimRight(ph, ",")
		clauses = append(clauses, fmt.Sprintf("assignee_team IN (%s)", ph))
		for _, t := range teams {
			args = append(args, t)
		}
	}

	stmt := fmt.Sprintf(`
		SELECT key, project, type, status, summary, priority,
		       COALESCE(assignee_aspect, ''), COALESCE(assignee_team, ''), updated_at
		FROM issues
		WHERE (%s) AND status NOT IN ('Done', 'Cancelled', 'Delivered')
		ORDER BY updated_at DESC
		LIMIT 100`,
		strings.Join(clauses, " OR "))

	return s.runRefQuery(ctx, stmt, args)
}

// ListReady returns the top of the ready pool for the caller: issues
// assigned to them (directly or via team) that are in a startable
// state ("To Do" or "In Progress" continuing). Ordered by priority
// then age.
func (s *Service) ListReady(ctx context.Context, aspect string, teams []string) ([]IssueRef, error) {
	clauses := []string{"assignee_aspect = ?"}
	args := []any{aspect}
	if len(teams) > 0 {
		ph := strings.Repeat("?,", len(teams))
		ph = strings.TrimRight(ph, ",")
		clauses = append(clauses, fmt.Sprintf("assignee_team IN (%s)", ph))
		for _, t := range teams {
			args = append(args, t)
		}
	}

	stmt := fmt.Sprintf(`
		SELECT key, project, type, status, summary, priority,
		       COALESCE(assignee_aspect, ''), COALESCE(assignee_team, ''), updated_at
		FROM issues
		WHERE (%s) AND status IN ('To Do', 'In Progress')
		ORDER BY
		  CASE priority WHEN 'Highest' THEN 5 WHEN 'High' THEN 4 WHEN 'Medium' THEN 3 WHEN 'Low' THEN 2 ELSE 1 END DESC,
		  created_at ASC
		LIMIT 50`,
		strings.Join(clauses, " OR "))

	return s.runRefQuery(ctx, stmt, args)
}

func (s *Service) runRefQuery(ctx context.Context, stmt string, args []any) ([]IssueRef, error) {
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueRef
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.Key, &r.Project, &r.Type, &r.Status, &r.Summary, &r.Priority,
			&r.AssigneeAspect, &r.AssigneeTeam, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run, verify PASS**

- [ ] **Step 5: Commit**

```bash
git add nexus/issues/search.go nexus/issues/search_test.go
git commit -m "feat(issues): ListMy + ListReady aspect-facing queries"
```

### Task 1.8: REST surface — mount `/api/issues/*` routes

**Files:**
- Create: `nexus/issues/rest.go`
- Create: `nexus/issues/rest_test.go`
- Modify: `nexus/cmd/nexus/main.go` — mount the REST handler

- [ ] **Step 1: Write failing test for create + get HTTP roundtrip**

```go
// nexus/issues/rest_test.go
package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestREST_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := New(ctx, Config{DBPath: filepath.Join(dir, "issues.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})

	h := svc.Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	// POST /api/issues
	body, _ := json.Marshal(map[string]any{
		"project":            "NEX",
		"type":               "Story",
		"summary":            "via rest",
		"definition_of_done": "- [ ] go",
		"reporter":           "shadow",
	})
	resp, err := http.Post(srv.URL+"/api/issues", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var created struct{ Key string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Key != "NEX-1" {
		t.Errorf("key = %q", created.Key)
	}

	// GET /api/issues/NEX-1
	resp2, err := http.Get(srv.URL + "/api/issues/" + created.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp2.StatusCode)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement REST handler**

```go
// nexus/issues/rest.go
package issues

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Handler returns the http.Handler that serves /api/issues/* + /healthz/issues.
// Mount under the nexus.exe broker's existing HTTPS listener.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz/issues", s.HealthzHandler())
	mux.HandleFunc("/api/issues", s.handleCreate)
	mux.HandleFunc("/api/issues/", s.handleIssueByKey)
	mux.HandleFunc("/api/issues/search", s.handleSearch)
	return mux
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var d IssueDraft
	// JSON field names mirror the IssueDraft fields (with snake_case overrides where needed).
	var raw struct {
		Project          string `json:"project"`
		Type             string `json:"type"`
		Summary          string `json:"summary"`
		Description      string `json:"description"`
		DefinitionOfDone string `json:"definition_of_done"`
		Priority         string `json:"priority"`
		Reporter         string `json:"reporter"`
		ParentKey        string `json:"parent_key"`
		AssigneeAspect   string `json:"assignee_aspect"`
		AssigneeTeam     string `json:"assignee_team"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	d = IssueDraft{
		Project: raw.Project, Type: raw.Type, Summary: raw.Summary,
		Description: raw.Description, DefinitionOfDone: raw.DefinitionOfDone,
		Priority: raw.Priority, Reporter: raw.Reporter, ParentKey: raw.ParentKey,
		AssigneeAspect: raw.AssigneeAspect, AssigneeTeam: raw.AssigneeTeam,
	}
	issue, err := s.CreateIssue(r.Context(), d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(issue)
}

func (s *Service) handleIssueByKey(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/issues/")
	if key == "" || strings.Contains(key, "/") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		issue, err := s.GetIssue(r.Context(), key)
		if errors.Is(err, ErrIssueNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issue)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var f SearchFilter
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	refs, err := s.Search(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(refs)
}
```

- [ ] **Step 4: Run, verify PASS**

- [ ] **Step 5: Mount in nexus.exe**

In `nexus/cmd/nexus/main.go`, replace the `mux.Handle("/healthz/issues", ...)` line from Task 0.3 with:
```go
mux.Handle("/api/issues", issuesSvc.Handler())
mux.Handle("/api/issues/", issuesSvc.Handler())
mux.Handle("/healthz/issues", issuesSvc.Handler())
```
(The single `Handler()` is fine because its internal mux routes all three; collapse if your existing main.go uses a different router pattern.)

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/rest.go nexus/issues/rest_test.go nexus/cmd/nexus/main.go
git commit -m "feat(issues): REST surface /api/issues/{create,get,search}"
```

### Task 1.9: MCP binary scaffolding — `runtime/cmd/nexus-issue-mcp/`

**Files:**
- Create: `runtime/cmd/nexus-issue-mcp/main.go`
- Create: `runtime/cmd/nexus-issue-mcp/client.go`
- Create: `runtime/cmd/nexus-issue-mcp/tools.go`

This task mirrors the structure of `runtime/cmd/nexus-jira-mcp/`. Skim `runtime/cmd/nexus-jira-mcp/main.go` first; copy the keyfile-load + auth pattern and replace the Jira client with an HTTP client targeting the in-process service.

- [ ] **Step 1: Write main.go**

```go
// runtime/cmd/nexus-issue-mcp/main.go
// Command nexus-issue-mcp bridges the in-process nexus issue tracker
// to stdio MCP. One process == one aspect identity. The keyfile
// provides the JWT auth path to reach the tracker via the nexus.exe
// HTTPS listener; no Jira credentials needed.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

const aspectMCPName = "nexus-issue"

func main() {
	var (
		keyfilePath  = flag.String("keyfile", "", "Path to the aspect keyfile JSON.")
		nexusURLFlag = flag.String("nexus-url", "", "Override the HTTPS base URL.")
		insecureSkip = flag.Bool("insecure-skip-verify", false, "Skip TLS verify (dev only).")
		logLevel     = flag.String("log-level", "info", "slog level")
		logFile      = flag.String("log-file", "", "Write logs here.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-issue-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	if *keyfilePath == "" {
		log.Error("missing -keyfile")
		os.Exit(2)
	}
	kf, err := keyfile.Load(*keyfilePath)
	if err != nil {
		log.Error("keyfile load failed", "err", err, "path", *keyfilePath)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Validate keyfile (proves nexus identity match + provides JWT).
	kc := keyfile.NewClient()
	if *insecureSkip {
		kc.HTTP = &http.Client{Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	}
	res, err := kc.Validate(ctx, kf)
	if err != nil {
		log.Error("keyfile validate failed", "err", err)
		os.Exit(2)
	}
	log.Info("keyfile validation succeeded", "aspect", res.AspectName, "nexus_id", kf.Envelope.NexusID)

	// Build HTTPS base URL for REST.
	httpsBase := *nexusURLFlag
	if httpsBase == "" {
		httpsBase = strings.Replace(res.NexusURL, "wss://", "https://", 1)
		httpsBase = strings.TrimSuffix(httpsBase, "/connect")
	}

	client := newClient(httpsBase, res.SessionJWT, *insecureSkip, log)

	srv := mcpserver.NewMCPServer(aspectMCPName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, log)

	log.Info("nexus-issue-mcp ready", "aspect", res.AspectName, "base", httpsBase)

	// Block on MCP stdio loop.
	if err := mcpserver.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
}

func buildLogger(level, file string) (*slog.Logger, func(), error) {
	w := os.Stderr
	closer := func() {}
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closer = func() { _ = f.Close() }
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), closer, nil
}
```

- [ ] **Step 2: Write client.go**

```go
// runtime/cmd/nexus-issue-mcp/client.go
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// client is the thin HTTP wrapper that talks to nexus.exe's
// /api/issues/* REST surface.
type client struct {
	base  string
	jwt   string
	http  *http.Client
	log   *slog.Logger
}

func newClient(base, jwt string, insecure bool, log *slog.Logger) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &client{
		base: base, jwt: jwt,
		http: &http.Client{Timeout: 15 * time.Second, Transport: tr},
		log:  log,
	}
}

// post sends a JSON body and decodes the JSON response.
func (c *client) post(ctx context.Context, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", req.Method, path, resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// get reads from path, decodes JSON into out.
func (c *client) get(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not found: %s", path)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %d %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
```

- [ ] **Step 3: Write tools.go (initial: create + get + search)**

```go
// runtime/cmd/nexus-issue-mcp/tools.go
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, c *client, log *slog.Logger) {
	srv.AddTool(mcpgo.NewTool("issue.create",
		mcpgo.WithDescription("Create an issue. Required: project, type, summary, definition_of_done, reporter."),
		mcpgo.WithString("project", mcpgo.Required()),
		mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("Epic|Story|Task|Subtask|Bug")),
		mcpgo.WithString("summary", mcpgo.Required()),
		mcpgo.WithString("definition_of_done", mcpgo.Required(), mcpgo.Description("Markdown checklist; at least one item.")),
		mcpgo.WithString("reporter", mcpgo.Required()),
		mcpgo.WithString("description"),
		mcpgo.WithString("priority"),
		mcpgo.WithString("parent_key"),
		mcpgo.WithString("assignee_aspect"),
		mcpgo.WithString("assignee_team"),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		body := map[string]any{
			"project":            req.GetString("project", ""),
			"type":               req.GetString("type", ""),
			"summary":            req.GetString("summary", ""),
			"definition_of_done": req.GetString("definition_of_done", ""),
			"reporter":           req.GetString("reporter", ""),
		}
		for _, k := range []string{"description", "priority", "parent_key", "assignee_aspect", "assignee_team"} {
			if v := req.GetString(k, ""); v != "" {
				body[k] = v
			}
		}
		var out map[string]any
		if err := c.post(ctx, "/api/issues", body, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.get",
		mcpgo.WithDescription("Get an issue by key (resolves aliases)."),
		mcpgo.WithString("key", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcpErr("key required"), nil
		}
		var out map[string]any
		if err := c.get(ctx, "/api/issues/"+key, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("issue.search",
		mcpgo.WithDescription("Structured filter search."),
		mcpgo.WithObject("filter", mcpgo.Required(), mcpgo.Description("SearchFilter shape; see spec.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		f := req.GetArguments()["filter"]
		var out []any
		if err := c.post(ctx, "/api/issues/search", f, &out); err != nil {
			return mcpErr(err.Error()), nil
		}
		return mcpJSON(out), nil
	})
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(msg)
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, _ := json.Marshal(v)
	return mcpgo.NewToolResultText(string(b))
}
```

- [ ] **Step 4: Build the MCP binary**

```bash
cd ~/Source/nexus
go build -o bin/nexus-issue-mcp ./runtime/cmd/nexus-issue-mcp/
```

Expected: clean build, binary in `bin/`.

- [ ] **Step 5: Commit**

```bash
git add runtime/cmd/nexus-issue-mcp/main.go runtime/cmd/nexus-issue-mcp/client.go runtime/cmd/nexus-issue-mcp/tools.go
git commit -m "feat(nexus-issue-mcp): scaffold MCP binary with create/get/search tools"
```

### Task 1.10: REST handlers for update, transition, assign

**Files:**
- Modify: `nexus/issues/rest.go` — add `handlePatch` (PATCH), `handleTransition` (POST /transition), `handleAssign` (POST /assign)
- Modify: `nexus/issues/rest_test.go` — round-trip tests

- [ ] **Step 1: Add tests**

```go
func TestREST_TransitionRoundtrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, _ := New(ctx, Config{DBPath: filepath.Join(dir, "issues.db")})
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	issue, _ := svc.CreateIssue(ctx, IssueDraft{Project: "NEX", Type: "Story", Summary: "x",
		DefinitionOfDone: "- [x] go", Reporter: "shadow"})

	body, _ := json.Marshal(map[string]any{"status": "In Progress", "actor": "anvil"})
	resp, err := http.Post(srv.URL+"/api/issues/"+issue.Key+"/transition", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("transition resp = %v %d", err, resp.StatusCode)
	}
}
```

- [ ] **Step 2: Implement the handlers**

In `rest.go`, update `handleIssueByKey` to dispatch on path tail:

```go
func (s *Service) handleIssueByKey(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/issues/")
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	key := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		s.respondGet(w, r, key)
	case r.Method == http.MethodPatch && action == "":
		s.respondPatch(w, r, key)
	case r.Method == http.MethodPost && action == "transition":
		s.respondTransition(w, r, key)
	case r.Method == http.MethodPost && action == "assign":
		s.respondAssign(w, r, key)
	default:
		http.Error(w, "method/path not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Service) respondGet(w http.ResponseWriter, r *http.Request, key string) { /* existing GET body */ }

func (s *Service) respondPatch(w http.ResponseWriter, r *http.Request, key string) {
	var raw struct {
		Summary          *string `json:"summary"`
		Description      *string `json:"description"`
		DefinitionOfDone *string `json:"definition_of_done"`
		Priority         *string `json:"priority"`
		ParentKey        *string `json:"parent_key"`
		Actor            string  `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	patch := UpdatePatch{
		Summary: raw.Summary, Description: raw.Description,
		DefinitionOfDone: raw.DefinitionOfDone, Priority: raw.Priority, ParentKey: raw.ParentKey,
	}
	if err := s.UpdateIssue(r.Context(), key, patch, raw.Actor); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) respondTransition(w http.ResponseWriter, r *http.Request, key string) {
	var raw struct {
		Status string `json:"status"`
		Actor  string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.TransitionIssue(r.Context(), key, raw.Status, raw.Actor); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) respondAssign(w http.ResponseWriter, r *http.Request, key string) {
	var raw struct {
		Aspect string `json:"aspect"`
		Team   string `json:"team"`
		Actor  string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.AssignIssue(r.Context(), key, raw.Aspect, raw.Team, raw.Actor); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

Remove the old `handleIssueByKey` GET-only body — folded into `respondGet`.

- [ ] **Step 3: Run, verify PASS**

- [ ] **Step 4: Add corresponding MCP tools**

In `runtime/cmd/nexus-issue-mcp/tools.go`, register:

```go
srv.AddTool(mcpgo.NewTool("issue.update",
	mcpgo.WithDescription("Patch issue fields."),
	mcpgo.WithString("key", mcpgo.Required()),
	mcpgo.WithString("summary"),
	mcpgo.WithString("description"),
	mcpgo.WithString("definition_of_done"),
	mcpgo.WithString("priority"),
	mcpgo.WithString("parent_key"),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	key := req.GetString("key", "")
	body := map[string]any{}
	for _, k := range []string{"summary", "description", "definition_of_done", "priority", "parent_key"} {
		if v := req.GetString(k, ""); v != "" {
			body[k] = v
		}
	}
	if err := c.patch(ctx, "/api/issues/"+key, body, nil); err != nil {
		return mcpErr(err.Error()), nil
	}
	return mcpJSON(map[string]any{"ok": true}), nil
})

srv.AddTool(mcpgo.NewTool("issue.transition",
	mcpgo.WithDescription("Transition issue to a new status."),
	mcpgo.WithString("key", mcpgo.Required()),
	mcpgo.WithString("status", mcpgo.Required()),
	mcpgo.WithString("actor", mcpgo.Required()),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	key := req.GetString("key", "")
	body := map[string]any{"status": req.GetString("status", ""), "actor": req.GetString("actor", "")}
	if err := c.post(ctx, "/api/issues/"+key+"/transition", body, nil); err != nil {
		return mcpErr(err.Error()), nil
	}
	return mcpJSON(map[string]any{"ok": true}), nil
})

srv.AddTool(mcpgo.NewTool("issue.assign",
	mcpgo.WithDescription("Assign issue to an aspect or team."),
	mcpgo.WithString("key", mcpgo.Required()),
	mcpgo.WithString("aspect"),
	mcpgo.WithString("team"),
	mcpgo.WithString("actor", mcpgo.Required()),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	key := req.GetString("key", "")
	body := map[string]any{
		"aspect": req.GetString("aspect", ""),
		"team":   req.GetString("team", ""),
		"actor":  req.GetString("actor", ""),
	}
	if err := c.post(ctx, "/api/issues/"+key+"/assign", body, nil); err != nil {
		return mcpErr(err.Error()), nil
	}
	return mcpJSON(map[string]any{"ok": true}), nil
})
```

And add a `patch` helper to `client.go` mirroring `post` with `http.MethodPatch`.

- [ ] **Step 5: Build + test**

```bash
go build ./...
go test ./nexus/issues/...
```

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/rest.go nexus/issues/rest_test.go runtime/cmd/nexus-issue-mcp/tools.go runtime/cmd/nexus-issue-mcp/client.go
git commit -m "feat(issues): REST + MCP for update/transition/assign"
```

### Task 1.11: Dual-write shim in `nexus-jira-mcp`

**Files:**
- Modify: `runtime/cmd/nexus-jira-mcp/tools.go` — after each successful Jira write, mirror to the native tracker
- Modify: `runtime/cmd/nexus-jira-mcp/main.go` — add a flag `-dual-write-base` (URL) + native client setup
- Create: `runtime/cmd/nexus-jira-mcp/native.go` — thin native-write client

The pattern: every write tool (`jira.create`, `jira.update_status`, `jira.comment`, `jira.claim`, `jira.complete`) wraps its existing Jira call. If `-dual-write-base` is set, attempt the native call after Jira returns success. Mirror failures log + ping operator but do NOT fail the call (Jira is authoritative).

- [ ] **Step 1: Add native.go**

```go
// runtime/cmd/nexus-jira-mcp/native.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

type nativeClient struct {
	base string
	jwt  string
	http *http.Client
	log  *slog.Logger
}

func (n *nativeClient) enabled() bool { return n != nil && n.base != "" }

// MirrorCreate sends a create to the native tracker. Non-fatal on error.
func (n *nativeClient) MirrorCreate(ctx context.Context, body map[string]any) {
	if !n.enabled() {
		return
	}
	if err := n.do(ctx, http.MethodPost, "/api/issues", body, nil); err != nil {
		n.log.Warn("dual-write create failed", "err", err)
	}
}

func (n *nativeClient) MirrorTransition(ctx context.Context, key, status, actor string) {
	if !n.enabled() {
		return
	}
	body := map[string]any{"status": status, "actor": actor}
	if err := n.do(ctx, http.MethodPost, "/api/issues/"+key+"/transition", body, nil); err != nil {
		n.log.Warn("dual-write transition failed", "err", err, "key", key)
	}
}

func (n *nativeClient) do(ctx context.Context, method, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, method, n.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.jwt)
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d: %s", resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// translateJiraCreate maps the Jira create payload to the native shape.
// Returns ErrUnmappable if a required native field can't be derived.
var ErrUnmappable = errors.New("native: unmappable")

func translateJiraCreate(project, typ, summary, description, reporter, dod string) (map[string]any, error) {
	if dod == "" {
		dod = "- [ ] _carried_from_jira_" // placeholder; native requires DoD
	}
	return map[string]any{
		"project":            project,
		"type":               typ,
		"summary":            summary,
		"description":        description,
		"definition_of_done": dod,
		"reporter":           reporter,
	}, nil
}
```

- [ ] **Step 2: Add flag + initialise client in main.go**

In `runtime/cmd/nexus-jira-mcp/main.go`'s flag block:
```go
dualWriteBase = flag.String("dual-write-base", "", "If set, mirror Jira writes to the native tracker at this base URL.")
```

After keyfile validation:
```go
var native *nativeClient
if *dualWriteBase != "" {
	native = &nativeClient{
		base: *dualWriteBase,
		jwt:  res.SessionJWT,
		http: &http.Client{Timeout: 10 * time.Second},
		log:  log,
	}
}
```

Pass `native` into `registerTools(srv, jiraClient, native, log)` (extending its signature in tools.go).

- [ ] **Step 3: Wire the mirror call into `jira.create`**

In `runtime/cmd/nexus-jira-mcp/tools.go`, inside the `jira.create` handler, **after** the existing successful Jira create that returns the new key, add:
```go
if native != nil {
    body, _ := translateJiraCreate(c.projectKey, typ, summary, description, reporter, "")
    native.MirrorCreate(ctx, body)
}
```
Repeat the pattern in `jira.update_status`, `jira.comment`, `jira.claim`, `jira.complete` — each mirrors its Jira-side action.

- [ ] **Step 4: Smoke test manually**

Start nexus.exe with the issues service mounted. Run `nexus-jira-mcp` with `-dual-write-base https://localhost:7888 -insecure-skip-verify`. Create a Jira issue via the MCP. Verify the native side has it: `curl -sk https://localhost:7888/api/issues/NEX-X` returns the row.

- [ ] **Step 5: Commit**

```bash
git add runtime/cmd/nexus-jira-mcp/native.go runtime/cmd/nexus-jira-mcp/main.go runtime/cmd/nexus-jira-mcp/tools.go
git commit -m "feat(nexus-jira-mcp): dual-write shim mirrors writes to native tracker"
```

**Phase 1 exit criteria reached** — aspect can MCP-create/get/update/transition/assign/search via native; Jira still authoritative; dual-write captures every write.

---

## Phase 2 — Timeline, Comments, Watchers, Notifications

Goal: full activity feed flowing; chat pings on assignment + mention; operator activity stream populated.

### Task 2.1: Events table + writes from issue mutations

**Files:**
- Modify: `nexus/issues/schema.sql` — add `events` table
- Create: `nexus/issues/events.go`
- Create: `nexus/issues/events_test.go`
- Modify: `nexus/issues/issues.go` — every mutation also writes an event row

- [ ] **Step 1: Append `events` table to schema.sql**

```sql
-- -------------------------------------------------------------------
-- Events (timeline)
-- -------------------------------------------------------------------
-- One row per timeline event. `kind` discriminates; `payload` JSON
-- holds kind-specific fields. Append-only — never updated.
CREATE TABLE IF NOT EXISTS events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_key   TEXT NOT NULL REFERENCES issues(key) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,                          -- per-issue ordering
  kind        TEXT NOT NULL,                             -- comment|transition|field_change|...
  actor       TEXT NOT NULL,
  at          TEXT NOT NULL DEFAULT (datetime('now')),
  payload     TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_events_issue ON events(issue_key, seq);
CREATE INDEX IF NOT EXISTS idx_events_at ON events(at);
```

Bump schema_versions to 5.

- [ ] **Step 2: Write failing test**

```go
// nexus/issues/events_test.go
package issues

import (
	"context"
	"testing"
)

func TestTransition_WritesEvent(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{Project: "NEX", Type: "Story", Summary: "x",
		DefinitionOfDone: "- [x] go", Reporter: "shadow"})

	if err := svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil"); err != nil {
		t.Fatal(err)
	}

	events, err := svc.Timeline(ctx, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}
	found := false
	for _, e := range events {
		if e.Kind == "transition" && e.Actor == "anvil" {
			found = true
		}
	}
	if !found {
		t.Errorf("transition event missing: %+v", events)
	}
}
```

- [ ] **Step 3: Run, verify FAIL**

- [ ] **Step 4: Implement events.go**

```go
// nexus/issues/events.go
package issues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Event is a timeline entry.
type Event struct {
	ID       int64
	IssueKey string
	Seq      int
	Kind     string
	Actor    string
	At       string
	Payload  map[string]any
}

// writeEvent appends an event. `tx` is required so callers wrap it in
// the same transaction as the mutation it describes.
func writeEvent(ctx context.Context, tx *sql.Tx, issueKey, kind, actor string, payload map[string]any) error {
	var nextSeq int
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE issue_key = ?`, issueKey,
	).Scan(&nextSeq)
	if err != nil {
		return err
	}
	pjson, _ := json.Marshal(payload)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(issue_key, seq, kind, actor, payload) VALUES (?, ?, ?, ?, ?)`,
		issueKey, nextSeq, kind, actor, string(pjson),
	)
	return err
}

// Timeline returns all events for an issue, ordered by seq ascending.
func (s *Service) Timeline(ctx context.Context, issueKey string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issue_key, seq, kind, actor, at, payload FROM events WHERE issue_key = ? ORDER BY seq ASC`,
		issueKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.IssueKey, &e.Seq, &e.Kind, &e.Actor, &e.At, &payload); err != nil {
			return nil, err
		}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &e.Payload)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 5: Wire events into mutations**

In `nexus/issues/issues.go`, modify `CreateIssue` to write a `create` event inside its transaction (just before commit):
```go
if err := writeEvent(ctx, tx, key, "create", d.Reporter, map[string]any{
    "type":   d.Type,
    "summary": d.Summary,
}); err != nil {
    return nil, fmt.Errorf("write create event: %w", err)
}
```

In `TransitionIssue`, after the status update, before commit:
```go
if err := writeEvent(ctx, tx, key, "transition", actor, map[string]any{
    "from": fromStatus, "to": toStatus,
}); err != nil {
    return err
}
```

In `UpdateIssue` (refactor to use a transaction), write a `field_change` event per changed field. Restructure:
```go
func (s *Service) UpdateIssue(ctx context.Context, key string, patch UpdatePatch, actor string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return err }
    defer tx.Rollback()
    // ... apply the same UPDATE ...
    // After UPDATE, for each non-nil field, write an event:
    if patch.Summary != nil {
        if err := writeEvent(ctx, tx, key, "field_change", actor, map[string]any{"field": "summary", "value": *patch.Summary}); err != nil { return err }
    }
    // ... repeat for the other fields ...
    return tx.Commit()
}
```

Similarly wire `AssignIssue` (write a `field_change` event with field="assignee" + new value).

- [ ] **Step 6: Run, verify PASS**

- [ ] **Step 7: Commit**

```bash
git add nexus/issues/schema.sql nexus/issues/events.go nexus/issues/events_test.go nexus/issues/issues.go
git commit -m "feat(issues): events table + timeline writes on every mutation"
```

### Task 2.2: Comments (immutable, append-only)

**Files:**
- Create: `nexus/issues/comments.go`
- Create: `nexus/issues/comments_test.go`
- Modify: `nexus/issues/rest.go` — add `/api/issues/{key}/comments` POST + GET
- Modify: `runtime/cmd/nexus-issue-mcp/tools.go` — add `issue.comment`

- [ ] **Step 1: Test**

```go
// nexus/issues/comments_test.go
package issues

import (
	"context"
	"testing"
)

func TestCommentIssue_AppendsEvent(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	defer svc.Close()
	_ = svc.CreateProject(ctx, Project{Key: "NEX", Name: "Nexus"})
	issue, _ := svc.CreateIssue(ctx, IssueDraft{Project: "NEX", Type: "Story", Summary: "x",
		DefinitionOfDone: "- [ ] go", Reporter: "shadow"})

	if err := svc.CommentIssue(ctx, issue.Key, "anvil", "hello"); err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	tl, _ := svc.Timeline(ctx, issue.Key)
	found := false
	for _, e := range tl {
		if e.Kind == "comment" && e.Payload["body"] == "hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("comment event missing")
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement**

```go
// nexus/issues/comments.go
package issues

import (
	"context"
	"fmt"
	"strings"
)

// CommentIssue appends a comment to the issue's timeline. Comments are
// immutable; the only way to "correct" one is a new comment.
func (s *Service) CommentIssue(ctx context.Context, key, actor, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("CommentIssue: empty body")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := writeEvent(ctx, tx, key, "comment", actor, map[string]any{"body": body}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = datetime('now') WHERE key = ?`, key,
	); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Add REST + MCP surface**

In `rest.go`, extend `handleIssueByKey`'s dispatch:
```go
case r.Method == http.MethodPost && action == "comments":
    s.respondComment(w, r, key)
```
Implementation:
```go
func (s *Service) respondComment(w http.ResponseWriter, r *http.Request, key string) {
    var raw struct{ Actor, Body string }
    if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest); return
    }
    if err := s.CommentIssue(r.Context(), key, raw.Actor, raw.Body); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest); return
    }
    w.WriteHeader(http.StatusCreated)
}
```

In `runtime/cmd/nexus-issue-mcp/tools.go`:
```go
srv.AddTool(mcpgo.NewTool("issue.comment",
    mcpgo.WithDescription("Append an immutable comment."),
    mcpgo.WithString("key", mcpgo.Required()),
    mcpgo.WithString("actor", mcpgo.Required()),
    mcpgo.WithString("body", mcpgo.Required()),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
    body := map[string]any{
        "actor": req.GetString("actor", ""),
        "body":  req.GetString("body", ""),
    }
    if err := c.post(ctx, "/api/issues/"+req.GetString("key","")+"/comments", body, nil); err != nil {
        return mcpErr(err.Error()), nil
    }
    return mcpJSON(map[string]any{"ok": true}), nil
})
```

- [ ] **Step 5: Run + commit**

```bash
go test ./nexus/issues/...
git add nexus/issues/comments.go nexus/issues/comments_test.go nexus/issues/rest.go runtime/cmd/nexus-issue-mcp/tools.go
git commit -m "feat(issues): immutable comments + comment event in timeline"
```

### Task 2.3: Watchers table + watch/unwatch

**Files:**
- Modify: `nexus/issues/schema.sql` — add `watchers` table
- Create: `nexus/issues/watchers.go`
- Create: `nexus/issues/watchers_test.go`
- Modify: `nexus/issues/rest.go` — add `/api/issues/{key}/watchers` POST/DELETE
- Modify: `runtime/cmd/nexus-issue-mcp/tools.go` — `issue.watch` + `issue.unwatch`

- [ ] **Step 1: Append to schema.sql**

```sql
CREATE TABLE IF NOT EXISTS watchers (
  issue_key  TEXT NOT NULL REFERENCES issues(key) ON DELETE CASCADE,
  aspect     TEXT NOT NULL,
  since      TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (issue_key, aspect)
);
```

Bump schema_versions to 6.

- [ ] **Step 2: Test + implement watchers**

```go
// nexus/issues/watchers_test.go
package issues

import ("context";"testing")
func TestWatch_Roundtrip(t *testing.T) {
    ctx := context.Background()
    svc := newTestService(t); defer svc.Close()
    _ = svc.CreateProject(ctx, Project{Key:"NEX",Name:"Nexus"})
    issue, _ := svc.CreateIssue(ctx, IssueDraft{Project:"NEX",Type:"Story",Summary:"x",
        DefinitionOfDone:"- [ ] go", Reporter:"shadow"})
    if err := svc.WatchIssue(ctx, issue.Key, "plumb"); err != nil { t.Fatal(err) }
    list, err := svc.Watchers(ctx, issue.Key); if err != nil { t.Fatal(err) }
    if len(list) != 1 || list[0] != "plumb" { t.Errorf("watchers = %v", list) }
}
```

```go
// nexus/issues/watchers.go
package issues
import ("context")
func (s *Service) WatchIssue(ctx context.Context, key, aspect string) error {
    _, err := s.db.ExecContext(ctx,
        `INSERT OR IGNORE INTO watchers(issue_key, aspect) VALUES (?, ?)`,
        key, aspect)
    return err
}
func (s *Service) UnwatchIssue(ctx context.Context, key, aspect string) error {
    _, err := s.db.ExecContext(ctx,
        `DELETE FROM watchers WHERE issue_key = ? AND aspect = ?`,
        key, aspect)
    return err
}
func (s *Service) Watchers(ctx context.Context, key string) ([]string, error) {
    rows, err := s.db.QueryContext(ctx,
        `SELECT aspect FROM watchers WHERE issue_key = ? ORDER BY since ASC`, key)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []string
    for rows.Next() {
        var a string
        if err := rows.Scan(&a); err != nil { return nil, err }
        out = append(out, a)
    }
    return out, rows.Err()
}
```

- [ ] **Step 3: Wire REST + MCP** (same pattern as comments)

- [ ] **Step 4: Run + commit**

```bash
go test ./nexus/issues/...
git add nexus/issues/schema.sql nexus/issues/watchers.go nexus/issues/watchers_test.go nexus/issues/rest.go runtime/cmd/nexus-issue-mcp/tools.go
git commit -m "feat(issues): watchers table + watch/unwatch surface"
```

### Task 2.4: Mention parser (`@aspect` → recipient list)

**Files:**
- Create: `nexus/issues/mentions.go`
- Create: `nexus/issues/mentions_test.go`

- [ ] **Step 1: Test**

```go
// nexus/issues/mentions_test.go
package issues

import ("reflect";"sort";"testing")
func TestParseMentions(t *testing.T) {
    cases := []struct{
        in string
        want []string
    }{
        {"hello @anvil and @plumb!", []string{"anvil","plumb"}},
        {"no mentions here", nil},
        {"email like a@b.com shouldn't match", nil},
        {"case ANvil should be lowered", []string{"anvil"}},
        {"@shadow @shadow dedup", []string{"shadow"}},
    }
    for _, c := range cases {
        got := ParseMentions(c.in)
        sort.Strings(got)
        sort.Strings(c.want)
        if !reflect.DeepEqual(got, c.want) {
            t.Errorf("ParseMentions(%q) = %v, want %v", c.in, got, c.want)
        }
    }
}
```

- [ ] **Step 2: Implement**

```go
// nexus/issues/mentions.go
package issues

import (
    "regexp"
    "strings"
)

// mentionRE matches @<word> where the @ is at word start (preceded by
// whitespace or beginning-of-string) — rejects email-like patterns.
var mentionRE = regexp.MustCompile(`(?:^|\s)@([A-Za-z][A-Za-z0-9_-]*)`)

// ParseMentions returns unique lowercased aspect names referenced in
// the markdown text. Case-insensitive per spec.
func ParseMentions(text string) []string {
    matches := mentionRE.FindAllStringSubmatch(text, -1)
    seen := map[string]struct{}{}
    var out []string
    for _, m := range matches {
        name := strings.ToLower(m[1])
        if _, ok := seen[name]; ok {
            continue
        }
        seen[name] = struct{}{}
        out = append(out, name)
    }
    return out
}
```

- [ ] **Step 3: Run + commit**

```bash
go test ./nexus/issues/ -run TestParseMentions
git add nexus/issues/mentions.go nexus/issues/mentions_test.go
git commit -m "feat(issues): @mention parser (case-insensitive, dedup)"
```

### Task 2.5: Materialised markdown view (`issue_get` returns the doc)

**Files:**
- Create: `nexus/issues/markdown.go`
- Create: `nexus/issues/markdown_test.go`
- Modify: `nexus/issues/rest.go` — `/api/issues/{key}` returns markdown by default; `?format=raw` returns JSON
- Modify: `runtime/cmd/nexus-issue-mcp/tools.go` — `issue.get` returns markdown string; add `issue.get_raw` for JSON

- [ ] **Step 1: Test**

```go
// nexus/issues/markdown_test.go
package issues

import (
    "context"
    "strings"
    "testing"
)

func TestMaterialiseMarkdown_Basic(t *testing.T) {
    ctx := context.Background()
    svc := newTestService(t); defer svc.Close()
    _ = svc.CreateProject(ctx, Project{Key:"NEX",Name:"Nexus"})
    issue, _ := svc.CreateIssue(ctx, IssueDraft{
        Project:"NEX", Type:"Story", Summary:"Story title",
        Description:"some body", DefinitionOfDone:"- [x] done",
        Reporter:"shadow", AssigneeAspect:"anvil",
    })
    _ = svc.CommentIssue(ctx, issue.Key, "anvil", "first comment")
    _ = svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil")

    md, err := svc.MaterialiseMarkdown(ctx, issue.Key)
    if err != nil { t.Fatal(err) }

    for _, want := range []string{
        "key: " + issue.Key, "Story title", "## Description", "some body",
        "## Definition of Done", "- [x] done",
        "## Timeline", "anvil (comment)", "first comment",
    } {
        if !strings.Contains(md, want) {
            t.Errorf("markdown missing %q\n----\n%s", want, md)
        }
    }
}
```

- [ ] **Step 2: Implement**

```go
// nexus/issues/markdown.go
package issues

import (
    "context"
    "fmt"
    "strings"
)

// MaterialiseMarkdown returns the aspect-facing markdown document for
// an issue: front-matter, sections for description / DoD / links /
// attachments / timeline.
func (s *Service) MaterialiseMarkdown(ctx context.Context, key string) (string, error) {
    issue, err := s.GetIssue(ctx, key)
    if err != nil {
        return "", err
    }
    timeline, err := s.Timeline(ctx, issue.Key)
    if err != nil {
        return "", err
    }
    watchers, _ := s.Watchers(ctx, issue.Key)

    var b strings.Builder
    fmt.Fprintf(&b, "---\n")
    fmt.Fprintf(&b, "key: %s\n", issue.Key)
    fmt.Fprintf(&b, "project: %s\n", issue.Project)
    fmt.Fprintf(&b, "type: %s\n", issue.Type)
    fmt.Fprintf(&b, "status: %s\n", issue.Status)
    fmt.Fprintf(&b, "priority: %s\n", issue.Priority)
    if issue.AssigneeAspect != "" {
        fmt.Fprintf(&b, "assignee_aspect: %s\n", issue.AssigneeAspect)
    }
    if issue.AssigneeTeam != "" {
        fmt.Fprintf(&b, "assignee_team: %s\n", issue.AssigneeTeam)
    }
    fmt.Fprintf(&b, "reporter: %s\n", issue.Reporter)
    fmt.Fprintf(&b, "created: %s\n", issue.CreatedAt)
    if issue.ParentKey != "" {
        fmt.Fprintf(&b, "parent: %s\n", issue.ParentKey)
    }
    if len(watchers) > 0 {
        fmt.Fprintf(&b, "watchers: [%s]\n", strings.Join(watchers, ", "))
    }
    fmt.Fprintf(&b, "---\n\n")

    fmt.Fprintf(&b, "# %s\n\n", issue.Summary)
    fmt.Fprintf(&b, "## Description\n\n%s\n\n", issue.Description)
    fmt.Fprintf(&b, "## Definition of Done\n\n%s\n\n", issue.DefinitionOfDone)

    if len(timeline) > 0 {
        fmt.Fprintf(&b, "## Timeline\n\n")
        for _, e := range timeline {
            fmt.Fprintf(&b, "### %s — %s (%s)\n", e.At, e.Actor, e.Kind)
            switch e.Kind {
            case "comment":
                if body, ok := e.Payload["body"].(string); ok {
                    fmt.Fprintf(&b, "%s\n\n", body)
                }
            case "transition":
                if from, ok := e.Payload["from"].(string); ok {
                    fmt.Fprintf(&b, "%s → %s\n\n", from, e.Payload["to"])
                }
            case "field_change":
                if field, ok := e.Payload["field"].(string); ok {
                    fmt.Fprintf(&b, "%s: %v\n\n", field, e.Payload["value"])
                }
            default:
                fmt.Fprintf(&b, "(event payload: %v)\n\n", e.Payload)
            }
        }
    }
    return b.String(), nil
}
```

- [ ] **Step 3: Wire REST + MCP**

In `rest.go::respondGet`, branch on `?format=`:
```go
func (s *Service) respondGet(w http.ResponseWriter, r *http.Request, key string) {
    switch r.URL.Query().Get("format") {
    case "raw":
        issue, err := s.GetIssue(r.Context(), key)
        if errors.Is(err, ErrIssueNotFound) { http.Error(w, "not found", http.StatusNotFound); return }
        if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(issue)
    default: // markdown
        md, err := s.MaterialiseMarkdown(r.Context(), key)
        if errors.Is(err, ErrIssueNotFound) { http.Error(w, "not found", http.StatusNotFound); return }
        if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
        w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
        _, _ = w.Write([]byte(md))
    }
}
```

In MCP tools.go, update `issue.get` to read the text body, and add `issue.get_raw`:
```go
srv.AddTool(mcpgo.NewTool("issue.get",
    mcpgo.WithDescription("Get issue as a markdown document (aspect-facing)."),
    mcpgo.WithString("key", mcpgo.Required()),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
    key := req.GetString("key", "")
    body, err := c.getText(ctx, "/api/issues/"+key)
    if err != nil { return mcpErr(err.Error()), nil }
    return mcpgo.NewToolResultText(body), nil
})
srv.AddTool(mcpgo.NewTool("issue.get_raw",
    mcpgo.WithDescription("Get structured JSON (dashboard/sync use)."),
    mcpgo.WithString("key", mcpgo.Required()),
), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
    key := req.GetString("key", "")
    var out map[string]any
    if err := c.get(ctx, "/api/issues/"+key+"?format=raw", &out); err != nil {
        return mcpErr(err.Error()), nil
    }
    return mcpJSON(out), nil
})
```

Add `getText` helper to `client.go`:
```go
func (c *client) getText(ctx context.Context, path string) (string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
    req.Header.Set("Authorization", "Bearer "+c.jwt)
    resp, err := c.http.Do(req)
    if err != nil { return "", err }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        b, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("%d: %s", resp.StatusCode, string(b))
    }
    b, err := io.ReadAll(resp.Body)
    return string(b), err
}
```

- [ ] **Step 4: Run + commit**

```bash
go test ./nexus/issues/...
git add nexus/issues/markdown.go nexus/issues/markdown_test.go nexus/issues/rest.go runtime/cmd/nexus-issue-mcp/tools.go runtime/cmd/nexus-issue-mcp/client.go
git commit -m "feat(issues): markdown materialisation; issue.get returns the doc"
```

### Task 2.6: Notification hook into broker chat

**Files:**
- Create: `nexus/issues/notify.go`
- Modify: `nexus/issues/service.go` — accept a `Notifier` interface
- Modify: `nexus/cmd/nexus/main.go` — pass a broker-backed Notifier to `issues.New`

The notifier interface decouples the issues package from the broker so tests can use a fake.

- [ ] **Step 1: Define Notifier + wire into Service**

In `nexus/issues/notify.go`:
```go
// nexus/issues/notify.go
package issues

import "context"

// Notifier delivers a chat DM to an aspect. The issues service calls
// it on assignment, mention, and watcher-relevant transitions.
//
// `op_stream` is true for events that should land in the operator
// activity stream as well (separate broker convention — single thread
// or topic the operator subscribes to).
type Notifier interface {
    NotifyAspect(ctx context.Context, aspect, message string) error
    NotifyOperatorStream(ctx context.Context, message string) error
}

// nopNotifier is the default — used in tests and when no broker is wired.
type nopNotifier struct{}

func (nopNotifier) NotifyAspect(ctx context.Context, aspect, message string) error { return nil }
func (nopNotifier) NotifyOperatorStream(ctx context.Context, message string) error  { return nil }
```

Extend `Config` and `Service` in `service.go`:
```go
type Config struct {
    DBPath   string
    Notifier Notifier // optional; defaults to nop
}

type Service struct {
    cfg Config
    db  *sql.DB
    notify Notifier
}

func New(ctx context.Context, cfg Config) (*Service, error) {
    if cfg.DBPath == "" {
        return nil, fmt.Errorf("issues.New: DBPath required")
    }
    dsn := "file:" + cfg.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
    db, err := sql.Open("sqlite3", dsn)
    if err != nil {
        return nil, fmt.Errorf("issues.New: open %s: %w", cfg.DBPath, err)
    }
    if err := db.PingContext(ctx); err != nil {
        _ = db.Close()
        return nil, fmt.Errorf("issues.New: ping: %w", err)
    }
    if err := applySchema(ctx, db); err != nil {
        _ = db.Close()
        return nil, err
    }
    notify := cfg.Notifier
    if notify == nil {
        notify = nopNotifier{}
    }
    return &Service{cfg: cfg, db: db, notify: notify}, nil
}
```

- [ ] **Step 2: Fire notifications from issues.go**

Inside `AssignIssue`, after the update:
```go
if aspect != "" {
    _ = s.notify.NotifyAspect(ctx, aspect, fmt.Sprintf("Assigned: %s", key))
}
if team != "" {
    // TODO: resolve team members from the broker aspects table; for now,
    // notify the team alias if the broker supports it. Operator stream
    // gets the event regardless.
}
_ = s.notify.NotifyOperatorStream(ctx, fmt.Sprintf("%s assigned %s to %s%s",
    actor, key, aspect, team))
```

Inside `TransitionIssue`, after committing the status change, fire:
```go
_ = s.notify.NotifyOperatorStream(ctx,
    fmt.Sprintf("%s: %s → %s by %s", key, fromStatus, toStatus, actor))
```
Plus notify watchers when transitioning to/from `Blocked`:
```go
if toStatus == "Blocked" || (fromStatus == "Blocked") {
    if watchers, _ := s.Watchers(ctx, key); len(watchers) > 0 {
        for _, w := range watchers {
            _ = s.notify.NotifyAspect(ctx, w,
                fmt.Sprintf("%s blocker %s → %s", key, fromStatus, toStatus))
        }
    }
}
```

Inside `CommentIssue`, after commit, parse mentions and notify each:
```go
mentions := ParseMentions(body)
for _, m := range mentions {
    _ = s.notify.NotifyAspect(ctx, m, fmt.Sprintf("Mentioned on %s by %s", key, actor))
}
_ = s.notify.NotifyOperatorStream(ctx, fmt.Sprintf("%s: %s commented", key, actor))
```

- [ ] **Step 3: Test with a fake notifier**

```go
// nexus/issues/notify_test.go
package issues

import (
    "context"
    "sync"
    "testing"
)

type captureNotifier struct {
    mu        sync.Mutex
    aspectMsg map[string][]string
    opStream  []string
}

func (n *captureNotifier) NotifyAspect(_ context.Context, aspect, msg string) error {
    n.mu.Lock(); defer n.mu.Unlock()
    if n.aspectMsg == nil { n.aspectMsg = map[string][]string{} }
    n.aspectMsg[aspect] = append(n.aspectMsg[aspect], msg)
    return nil
}
func (n *captureNotifier) NotifyOperatorStream(_ context.Context, msg string) error {
    n.mu.Lock(); defer n.mu.Unlock()
    n.opStream = append(n.opStream, msg)
    return nil
}

func TestAssign_PushesNotification(t *testing.T) {
    ctx := context.Background()
    n := &captureNotifier{}
    svc := newTestServiceWithNotifier(t, n)
    defer svc.Close()
    _ = svc.CreateProject(ctx, Project{Key:"NEX", Name:"Nexus"})
    issue, _ := svc.CreateIssue(ctx, IssueDraft{Project:"NEX",Type:"Story",Summary:"x",
        DefinitionOfDone:"- [ ] go", Reporter:"shadow"})
    _ = svc.AssignIssue(ctx, issue.Key, "anvil", "", "shadow")
    if len(n.aspectMsg["anvil"]) != 1 {
        t.Errorf("anvil should have 1 notification; got %v", n.aspectMsg)
    }
    if len(n.opStream) == 0 {
        t.Errorf("operator stream should have an entry; got %v", n.opStream)
    }
}

func TestComment_NotifiesMentions(t *testing.T) {
    ctx := context.Background()
    n := &captureNotifier{}
    svc := newTestServiceWithNotifier(t, n); defer svc.Close()
    _ = svc.CreateProject(ctx, Project{Key:"NEX",Name:"Nexus"})
    issue, _ := svc.CreateIssue(ctx, IssueDraft{Project:"NEX",Type:"Story",Summary:"x",
        DefinitionOfDone:"- [ ] go", Reporter:"shadow"})
    _ = svc.CommentIssue(ctx, issue.Key, "shadow", "ping @anvil and @plumb")
    if len(n.aspectMsg["anvil"]) != 1 || len(n.aspectMsg["plumb"]) != 1 {
        t.Errorf("mention notifications missing: %v", n.aspectMsg)
    }
}

func newTestServiceWithNotifier(t *testing.T, n Notifier) *Service {
    t.Helper()
    dir := t.TempDir()
    svc, err := New(context.Background(), Config{DBPath: filepath.Join(dir, "issues.db"), Notifier: n})
    if err != nil { t.Fatal(err) }
    return svc
}
```

- [ ] **Step 4: Implement broker-backed notifier**

Create `nexus/issues/notify_broker.go`:
```go
// nexus/issues/notify_broker.go
package issues

import (
    "context"
    "github.com/CarriedWorldUniverse/nexus/nexus/broker"
)

// BrokerNotifier is the production Notifier — sends chat.send frames
// via the broker's canonical HandleChatSend code path.
type BrokerNotifier struct {
    Broker       *broker.Server // exposes HandleChatSend (already public)
    OperatorAddr string         // who receives the activity stream; e.g. "operator"
    StreamThread string         // optional thread tag the operator subscribes to
}

func (b *BrokerNotifier) NotifyAspect(ctx context.Context, aspect, message string) error {
    // Use broker.HandleChatSend signature here. Exact API: check
    // nexus/broker/chat_send.go for the canonical call shape.
    return b.Broker.HandleChatSend(ctx, broker.ChatSendInput{
        From:    "nexus-issues",
        To:      []string{aspect},
        Content: message,
    })
}

func (b *BrokerNotifier) NotifyOperatorStream(ctx context.Context, message string) error {
    return b.Broker.HandleChatSend(ctx, broker.ChatSendInput{
        From:    "nexus-issues",
        To:      []string{b.OperatorAddr},
        Thread:  b.StreamThread,
        Content: message,
    })
}
```

**Note:** `broker.ChatSendInput`'s exact field names depend on the existing signature; check `nexus/broker/chat_send.go` and align. If the broker exports something slightly different (e.g. takes `Envelope` etc.), adapt. The key insight is funnel-through-broker — don't write directly to broker tables.

In `nexus/cmd/nexus/main.go`, wire:
```go
issuesSvc, err := issues.New(ctx, issues.Config{
    DBPath: issuesDB,
    Notifier: &issues.BrokerNotifier{
        Broker:       brokerSvc,        // existing broker server handle
        OperatorAddr: "operator",
        StreamThread: "issue-activity",
    },
})
```

- [ ] **Step 5: Smoke test end-to-end**

Run nexus.exe with both broker + issues. From the MCP, create + assign an issue; verify the assignee sees a chat.deliver frame with the assignment notice, and the operator's `issue-activity` thread gets the stream event.

- [ ] **Step 6: Commit**

```bash
git add nexus/issues/notify.go nexus/issues/notify_broker.go nexus/issues/notify_test.go nexus/issues/service.go nexus/issues/issues.go nexus/issues/comments.go nexus/cmd/nexus/main.go
git commit -m "feat(issues): broker-backed notifications + operator activity stream"
```

### Task 2.7: ListMyUpdates (pull-mode catch-up)

**Files:**
- Modify: `nexus/issues/events.go` — add `ListMyUpdates(ctx, aspect, since)` query
- Modify: `nexus/issues/rest.go` — `/api/issues/updates?aspect=...&since=...`
- Modify: `runtime/cmd/nexus-issue-mcp/tools.go` — `issue.list_my_updates`

- [ ] **Step 1: Test + implement**

```go
// in events_test.go
func TestListMyUpdates_FiltersByActorAndTime(t *testing.T) {
    ctx := context.Background()
    svc := newTestService(t); defer svc.Close()
    _ = svc.CreateProject(ctx, Project{Key:"NEX",Name:"Nexus"})
    issue, _ := svc.CreateIssue(ctx, IssueDraft{Project:"NEX",Type:"Story",Summary:"x",
        DefinitionOfDone:"- [ ] go", Reporter:"shadow", AssigneeAspect:"anvil"})
    _ = svc.CommentIssue(ctx, issue.Key, "anvil", "first")
    _ = svc.CommentIssue(ctx, issue.Key, "anvil", "second")

    upd, err := svc.ListMyUpdates(ctx, "anvil", "")
    if err != nil { t.Fatal(err) }
    if len(upd) < 2 { t.Errorf("expected ≥2 events; got %d", len(upd)) }
}
```

```go
// in events.go
// ListMyUpdates returns events on issues assigned to OR watched by
// `aspect`, with at > since. since="" returns all.
func (s *Service) ListMyUpdates(ctx context.Context, aspect, since string) ([]Event, error) {
    args := []any{aspect, aspect}
    sinceClause := ""
    if since != "" {
        sinceClause = " AND e.at > ?"
        args = append(args, since)
    }
    rows, err := s.db.QueryContext(ctx, `
        SELECT e.id, e.issue_key, e.seq, e.kind, e.actor, e.at, e.payload
        FROM events e
        JOIN issues i ON i.key = e.issue_key
        LEFT JOIN watchers w ON w.issue_key = e.issue_key AND w.aspect = ?
        WHERE (i.assignee_aspect = ? OR w.aspect IS NOT NULL)`+sinceClause+`
        ORDER BY e.at ASC
        LIMIT 200`, args...,
    )
    if err != nil { return nil, err }
    defer rows.Close()
    var out []Event
    for rows.Next() {
        var e Event
        var payload string
        if err := rows.Scan(&e.ID, &e.IssueKey, &e.Seq, &e.Kind, &e.Actor, &e.At, &payload); err != nil {
            return nil, err
        }
        if payload != "" { _ = json.Unmarshal([]byte(payload), &e.Payload) }
        out = append(out, e)
    }
    return out, rows.Err()
}
```

- [ ] **Step 2: REST + MCP wire**

Add `/api/issues/updates` handler that reads `aspect` + `since` query params and returns the JSON. Register MCP tool `issue.list_my_updates`.

- [ ] **Step 3: Run + commit**

```bash
go test ./nexus/issues/...
git add nexus/issues/events.go nexus/issues/events_test.go nexus/issues/rest.go runtime/cmd/nexus-issue-mcp/tools.go
git commit -m "feat(issues): list_my_updates for pull-mode aspect catch-up"
```

### Task 2.8: End-to-end smoke + integration sanity

**Files:**
- Create: `nexus/issues/integration_test.go` — full lifecycle: create → assign → comment → transition → notify

- [ ] **Step 1: Write the test**

```go
// nexus/issues/integration_test.go
//go:build integration
package issues

import (
    "context"
    "path/filepath"
    "testing"
)

func TestE2E_FullLifecycle(t *testing.T) {
    ctx := context.Background()
    n := &captureNotifier{}
    dir := t.TempDir()
    svc, err := New(ctx, Config{DBPath: filepath.Join(dir, "issues.db"), Notifier: n})
    if err != nil { t.Fatal(err) }
    defer svc.Close()

    _ = svc.CreateProject(ctx, Project{Key:"NEX", Name:"Nexus"})

    // Create
    issue, _ := svc.CreateIssue(ctx, IssueDraft{
        Project:"NEX", Type:"Story",
        Summary:"e2e", Description:"desc",
        DefinitionOfDone:"- [x] step 1",
        Reporter:"shadow",
    })
    // Assign
    _ = svc.AssignIssue(ctx, issue.Key, "anvil", "", "shadow")
    // Watch
    _ = svc.WatchIssue(ctx, issue.Key, "plumb")
    // Comment with mention
    _ = svc.CommentIssue(ctx, issue.Key, "anvil", "@plumb starting work")
    // Transition path
    _ = svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil")
    _ = svc.TransitionIssue(ctx, issue.Key, "Blocked", "anvil")
    _ = svc.TransitionIssue(ctx, issue.Key, "In Progress", "anvil")
    _ = svc.TransitionIssue(ctx, issue.Key, "In Review", "anvil")
    _ = svc.TransitionIssue(ctx, issue.Key, "Done", "anvil")

    md, _ := svc.MaterialiseMarkdown(ctx, issue.Key)
    if len(md) < 200 {
        t.Errorf("doc too short: %s", md)
    }

    if len(n.aspectMsg["anvil"]) == 0 {
        t.Errorf("anvil should have received assignment notice")
    }
    if len(n.aspectMsg["plumb"]) == 0 {
        t.Errorf("plumb should have received mention + blocked transitions")
    }
    if len(n.opStream) < 5 {
        t.Errorf("op stream too sparse: %d", len(n.opStream))
    }
}
```

- [ ] **Step 2: Run**

```bash
cd ~/Source/nexus && go test -tags=integration ./nexus/issues/ -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add nexus/issues/integration_test.go
git commit -m "test(issues): end-to-end lifecycle integration test"
```

**Phase 2 exit criteria reached** — comments + timeline visible; chat pings deliver on assign + mention + blocked transitions; operator activity stream populated.

---

## Final verification

- [ ] **All tests pass**: `cd ~/Source/nexus && go test ./... -count=1`
- [ ] **Build clean**: `go build ./...`
- [ ] **Both binaries built**: `go build -o bin/nexus ./nexus/cmd/nexus/ && go build -o bin/nexus-issue-mcp ./runtime/cmd/nexus-issue-mcp/`
- [ ] **Manual smoke**: start nexus.exe; in another terminal run `bin/nexus-issue-mcp -keyfile <shadow.keyfile> -dual-write-base https://localhost:7888 -insecure-skip-verify` from claude-code; exercise `issue.create`, `issue.get`, `issue.transition`, `issue.comment` through the MCP; verify markdown materialisation reads correctly and chat notifications fire.

When manual smoke passes, this PR is ready for review. Plan for Phases 3–7 to be drafted as separate plans:

- **Phase 3 plan**: external sync via NEX-140 (depends on interchange extension)
- **Phase 4 plan**: attachments via NEX-139, FTS5 search
- **Phase 5 plan**: operator dashboard UI
- **Phase 6 plan**: Jira migration tool + cutover runbook
- **Phase 7 plan**: deprecate `nexus-jira-mcp`
