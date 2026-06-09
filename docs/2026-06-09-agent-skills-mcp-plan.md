# Agent Skills MCP (sub-project #1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `nexus-skills-mcp` — a stdio MCP server that serves the canonical `SKILL.md` store via `search_skills`/`get_skill` — plus the SKILL.md format and one stub skill, proving the load loop end-to-end.

**Architecture:** Skills are version-controlled markdown under `skills/<name>/SKILL.md` at the repo root (also the dir CLI agents load natively in later sub-projects). A `skills` Go package co-located there `go:embed`s them and provides parse/search/get (the embed can't reach up from `runtime/cmd/`, so it lives with the files). The MCP server (mark3labs/mcp-go, the existing `nexus-*-mcp` pattern) is a thin stdio wrapper exposing two read-only tools. No backend, no keyfile-to-tracker.

**Tech Stack:** Go, `github.com/mark3labs/mcp-go v0.54.1`, `gopkg.in/yaml.v3 v3.0.1`, `embed`.

**Spec:** `docs/2026-06-09-agent-skills-system-design.md` (sub-project #1). **Branch:** `design/agent-skills-system`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

- Create `skills/development/SKILL.md` — the stub skill (real frontmatter + short body); the canonical store later sub-projects fill.
- Create `skills/embed.go` — `package skills`; `//go:embed` the SKILL.md files into an `embed.FS`.
- Create `skills/store.go` — `Skill` type + `parse()` + `Load()` + `Search()` + `Get()` (the reusable store logic; later the funnel-injection path reuses it).
- Create `skills/store_test.go` — unit tests for parse/search/get.
- Create `runtime/cmd/nexus-skills-mcp/main.go` — flags, logger, `NewMCPServer`, `registerTools`, `ServeStdio`.
- Create `runtime/cmd/nexus-skills-mcp/tools.go` — `search_skills` + `get_skill` tools + `mcpErr`/`mcpJSON` helpers.
- Create `runtime/cmd/nexus-skills-mcp/e2e_test.go` — in-process MCP client round-trip (initialize → tools/call).

---

## Task 1: The stub skill + the embed

**Files:**
- Create: `skills/development/SKILL.md`
- Create: `skills/embed.go`

- [ ] **Step 1: Write the stub skill**

```markdown
---
name: development
description: How to implement a dispatched ticket on nexus — test-first, single-ticket discipline, verify before opening a PR.
when_to_use: When you are writing or changing code to implement a ticket, before opening a PR.
---

# development

Implement one ticket per branch, branched off the latest `main`.

## Loop
1. Write a failing test first; run it; see it fail for the right reason.
2. Write the minimal code to pass; run the tests green.
3. No dead code, no unrelated changes — single ticket only.
4. Verify before you open the PR: `go build ./...` and `go test ./...` green. For frontend, browser-verify (Playwright) — do not ship UI unseen.
5. Also load `security` (scan + secret hygiene) and `house-style` (conventions) before you finish.

## Definition of done
Branch pushed, single-ticket PR open, CI green (including security scans), PR description states what changed and how it was verified.
```

- [ ] **Step 2: Write the embed**

```go
// Package skills is the canonical store of nexus dev-lifecycle skills.
// The SKILL.md files double as the dir CLI agents (codex, claude-code)
// load natively; this package embeds them for the nexus-skills-mcp
// server (go:embed can't reach up from runtime/cmd, so it lives here).
package skills

import "embed"

//go:embed all:.
var files embed.FS
```

> `all:.` embeds the whole `skills/` tree (every `SKILL.md` in every subdir); `store.go`'s loader filters to basename `SKILL.md`, so embedding `embed.go`/docs alongside is harmless.

- [ ] **Step 3: Verify it compiles**

Run: `cd /Users/jacinta/Source/nexus && go build ./skills/`
Expected: builds (an unused `files` var is fine — Task 2 consumes it).

- [ ] **Step 4: Commit**

```bash
git add skills/development/SKILL.md skills/embed.go
git commit -m "feat(skills): stub development skill + embed.FS

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: SKILL.md parser (TDD)

**Files:**
- Create: `skills/store.go`
- Create: `skills/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
package skills

import "testing"

func TestParse(t *testing.T) {
	raw := "---\nname: development\ndescription: how to build\nwhen_to_use: when coding\n---\n# development\n\nbody line one.\n"
	s, err := parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "development" || s.Description != "how to build" || s.WhenToUse != "when coding" {
		t.Fatalf("frontmatter: %+v", s)
	}
	if want := "# development\n\nbody line one.\n"; s.Body != want {
		t.Fatalf("body = %q, want %q", s.Body, want)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	if _, err := parse([]byte("# no frontmatter\n")); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}
```

- [ ] **Step 2: Run it — see it fail**

Run: `go test ./skills/ -run TestParse -v`
Expected: FAIL (undefined: parse / Skill).

- [ ] **Step 3: Implement parse + the type**

```go
package skills

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is one parsed SKILL.md: frontmatter fields + the markdown body.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when_to_use"`
	Body        string `yaml:"-"`
}

// parse splits leading `---`-fenced YAML frontmatter from the markdown body.
func parse(raw []byte) (Skill, error) {
	var s Skill
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return s, fmt.Errorf("missing frontmatter fence")
	}
	rest := raw[len("---\n"):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return s, fmt.Errorf("unterminated frontmatter")
	}
	front := rest[:end]
	body := rest[end+len("\n---\n"):]
	if err := yaml.Unmarshal(front, &s); err != nil {
		return s, fmt.Errorf("frontmatter yaml: %w", err)
	}
	if strings.TrimSpace(s.Name) == "" {
		return s, fmt.Errorf("frontmatter: name required")
	}
	s.Body = string(body)
	return s, nil
}
```

- [ ] **Step 4: Run it — green**

Run: `go test ./skills/ -run TestParse -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add skills/store.go skills/store_test.go
git commit -m "feat(skills): SKILL.md frontmatter+body parser (TDD)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Load + Search + Get (TDD)

**Files:**
- Modify: `skills/store.go`
- Modify: `skills/store_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadSearchGet(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no skills loaded from embed")
	}
	// Search is case-insensitive over name+description+when_to_use.
	hits := Search("TICKET")
	if len(hits) == 0 {
		t.Fatal("expected the development skill to match 'ticket'")
	}
	found := false
	for _, h := range hits {
		if h.Name == "development" {
			found = true
		}
	}
	if !found {
		t.Fatalf("development not in hits: %+v", hits)
	}
	body, ok := Get("development")
	if !ok || !strings.Contains(body, "# development") {
		t.Fatalf("Get(development) ok=%v body=%q", ok, body)
	}
	if _, ok := Get("nonexistent"); ok {
		t.Fatal("Get(nonexistent) should be false")
	}
}
```

- [ ] **Step 2: Run — fail**

Run: `go test ./skills/ -run TestLoadSearchGet -v`
Expected: FAIL (undefined: Load/Search/Get).

- [ ] **Step 3: Implement**

Add to `skills/store.go`:

```go
import (
	"io/fs"
	"path"
	"sort"
)

// Load parses every SKILL.md embedded in the store.
func Load() ([]Skill, error) {
	var out []Skill
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Base(p) != "SKILL.md" {
			return nil
		}
		raw, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		s, err := parse(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Search returns skills whose name/description/when_to_use contain query
// (case-insensitive). Empty query returns all (browse mode).
func Search(query string) []Skill {
	all, err := Load()
	if err != nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var hits []Skill
	for _, s := range all {
		hay := strings.ToLower(s.Name + " " + s.Description + " " + s.WhenToUse)
		if strings.Contains(hay, q) {
			hits = append(hits, s)
		}
	}
	return hits
}

// Get returns the full SKILL.md body for an exact skill name.
func Get(name string) (string, bool) {
	all, err := Load()
	if err != nil {
		return "", false
	}
	for _, s := range all {
		if s.Name == name {
			return s.Body, true
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run — green**

Run: `go test ./skills/ -v`
Expected: PASS (all). (The stub skill body says "Implement one ticket per branch" + "single ticket only", so `Search("ticket")` matches via description "implement a dispatched ticket".)

- [ ] **Step 5: Commit**

```bash
git add skills/store.go skills/store_test.go
git commit -m "feat(skills): Load/Search/Get over the embedded store (TDD)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: The MCP server (main.go + tools.go)

**Files:**
- Create: `runtime/cmd/nexus-skills-mcp/main.go`
- Create: `runtime/cmd/nexus-skills-mcp/tools.go`

- [ ] **Step 1: main.go** (mirror `nexus-issue-mcp/main.go`, minus the keyfile/backend)

```go
// Command nexus-skills-mcp serves the canonical nexus dev-lifecycle skill
// store over stdio MCP. Read-only, no backend: it embeds skills/<name>/SKILL.md
// and exposes search_skills + get_skill for progressive disclosure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

const aspectMCPName = "nexus-skills"

func main() {
	var (
		logLevel = flag.String("log-level", "info", "slog level")
		logFile  = flag.String("log-file", "", "Write logs here (default stderr).")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-skills-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = ctx

	srv := mcpserver.NewMCPServer(aspectMCPName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, log)
	log.Info("nexus-skills-mcp ready")

	if err := mcpserver.ServeStdio(srv); err != nil &&
		!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
}

func buildLogger(level, file string) (*slog.Logger, func(), error) {
	w := os.Stderr
	closer := func() {}
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closer = func() { _ = f.Close() }
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), closer, nil
}
```

> Confirm `buildLogger` against `nexus-issue-mcp/main.go`'s version and match it exactly if it differs (it builds an identical text handler today).

- [ ] **Step 2: tools.go**

```go
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/skills"
)

func registerTools(srv *mcpserver.MCPServer, log *slog.Logger) {
	srv.AddTool(mcpgo.NewTool("search_skills",
		mcpgo.WithDescription("Search the nexus dev-lifecycle skill library. Returns matching skills as [{name, description}]. Call get_skill with a name to load the full skill."),
		mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Topic or phase, e.g. 'review', 'security', 'merge'. Empty lists all.")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		hits := skills.Search(req.GetString("query", ""))
		out := make([]map[string]string, 0, len(hits))
		for _, s := range hits {
			out = append(out, map[string]string{"name": s.Name, "description": s.Description})
		}
		return mcpJSON(out), nil
	})

	srv.AddTool(mcpgo.NewTool("get_skill",
		mcpgo.WithDescription("Load the full SKILL.md body for a skill by exact name (from search_skills)."),
		mcpgo.WithString("name", mcpgo.Required()),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		name := req.GetString("name", "")
		body, ok := skills.Get(name)
		if !ok {
			return mcpErr("no such skill: " + name), nil
		}
		return mcpgo.NewToolResultText(body), nil
	})
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return &mcpgo.CallToolResult{IsError: true, Content: []mcpgo.Content{mcpgo.TextContent{Type: "text", Text: msg}}}
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpErr("marshal: " + err.Error())
	}
	return mcpgo.NewToolResultText(string(b))
}
```

> Confirm `mcpErr`/`mcpJSON` against `nexus-issue-mcp/tools.go:338,342` and copy their exact construction (the `mcpgo.Content`/`TextContent` shape may differ by mcp-go version — match the working server, don't guess).

- [ ] **Step 3: Build**

Run: `go mod tidy && go build ./...`
Expected: clean. (`yaml.v3` moves from indirect to direct; the new binary builds.)

- [ ] **Step 4: Commit**

```bash
git add runtime/cmd/nexus-skills-mcp/ go.mod go.sum
git commit -m "feat(skills): nexus-skills-mcp server (search_skills + get_skill)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: End-to-end round-trip (in-process MCP client)

**Files:**
- Create: `runtime/cmd/nexus-skills-mcp/e2e_test.go`

- [ ] **Step 1: Write the round-trip test**

Use mcp-go's in-process client against the same `MCPServer` (no subprocess needed). Confirm the client API against the installed `mcp-go v0.54.1` (`client.NewInProcessClient` / `server.NewTestServer` — use whichever the version exposes; the issue/jira servers' tests are the reference if present).

```go
package main

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestEndToEnd(t *testing.T) {
	srv := mcpserver.NewMCPServer("nexus-skills", "test", mcpserver.WithToolCapabilities(true))
	registerTools(srv, nil)

	// search_skills("ticket") must surface development.
	res, err := srv.HandleToolCall(context.Background(), mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "search_skills", Arguments: map[string]any{"query": "ticket"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("search_skills: err=%v res=%+v", err, res)
	}
	if txt := toolText(res); !strings.Contains(txt, "development") {
		t.Fatalf("search result missing development: %s", txt)
	}

	// get_skill("development") returns the body.
	res, err = srv.HandleToolCall(context.Background(), mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "get_skill", Arguments: map[string]any{"name": "development"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_skill: err=%v res=%+v", err, res)
	}
	if txt := toolText(res); !strings.Contains(txt, "# development") {
		t.Fatalf("get_skill body wrong: %s", txt)
	}
}

func toolText(r *mcpgo.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
```

> `srv.HandleToolCall` / `CallToolParams` / `TextContent` are the v0.54.1 shapes to confirm — if the method name differs, use the in-process client (`client.NewInProcessClient(srv)` → `Initialize` → `CallTool`). The point is one real round-trip through the registered tools, not the exact call surface.

- [ ] **Step 2: Run the full suite**

Run: `go test ./skills/ ./runtime/cmd/nexus-skills-mcp/ -v`
Expected: PASS (parser, search/get, end-to-end).

- [ ] **Step 3: Manual stdio smoke (optional but recommended)**

Build + drive it over real stdio to confirm the JSON-RPC framing works the way an aspect will call it:

```bash
go build -o /tmp/nexus-skills-mcp ./runtime/cmd/nexus-skills-mcp
printf '%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_skills","arguments":{"query":"ticket"}}}' \
 | /tmp/nexus-skills-mcp 2>/dev/null
```
Expected: a JSON-RPC result whose content lists `development`.

- [ ] **Step 4: Commit**

```bash
git add runtime/cmd/nexus-skills-mcp/e2e_test.go
git commit -m "test(skills): end-to-end search_skills + get_skill round-trip

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Push

- [ ] **Step 1: Full build + test**

Run: `go build ./... && go test ./skills/ ./runtime/cmd/nexus-skills-mcp/`
Expected: clean + PASS.

- [ ] **Step 2: Push the branch**

```bash
git push -u origin design/agent-skills-system
```

---

## Decomposition

**One ticket** covers sub-project #1 (server + format + stub + proof). It produces a working, testable binary. Sub-projects #2 (author the 8 skills), #3 (mcp_profile wiring + NEXUS.md pointer + retire dev-standards), #4 (CI scanners) are separate plans that depend on this.

## Self-Review notes (for the executor)

- **Spec coverage (sub-project #1):** SKILL.md format (T1/T2) · embedded canonical store (T1/T3) · `search_skills`/`get_skill` (T4) · stub skill (T1) · end-to-end proof (T5). The native-CLI loading + API-aspect MCP wiring are sub-projects #3, not here.
- **Confirm-against-installed-version seams (flagged inline):** `go:embed all:.` vs `*/SKILL.md` (T1); `buildLogger` exact body (T4); `mcpErr`/`mcpJSON` `Content`/`TextContent` shape (T4 — copy from the working issue server); the in-process test call surface `HandleToolCall` vs `NewInProcessClient` (T5). Match the installed `mcp-go v0.54.1` + the existing servers; don't invent.
- **Type consistency:** `Skill{Name,Description,WhenToUse,Body}` (T2) is used unchanged by Load/Search/Get (T3) and the tools (T4); `skills.Search`/`skills.Get` are the package's exported surface.
- **Risk:** the embed-can't-reach-up constraint is handled by putting the embed in `skills/` (T1), not `runtime/cmd/`. If `go:embed all:.` pulls in `embed.go`, the `path.Base(p) != "SKILL.md"` filter (T3) excludes it.
