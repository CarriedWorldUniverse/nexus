# Agent Skills Wiring (sub-project #3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the agents actually use the skills — move the store to the cross-platform `.agents/skills/` location (so codex + claude discover them natively), build the MCP binary, and point every aspect at `workflow-basics` via the central CLAUDE.md.

**Architecture:** The 9 `SKILL.md` files move to `.agents/skills/` (the standard codex/claude/gemini scan path → native discovery for CLI agents, no per-agent config). Because Go excludes dot-dirs and `go:embed` only reaches its own subtree, the Go store + embed move to a **repo-root package** (`//go:embed all:.agents/skills`, verified to work). The MCP serves the same dir to the API aspects. The central CLAUDE.md gains the always-on pointer; `mcp_profile` wiring + shipping the binary are operational steps.

**Tech Stack:** Go, `embed`, `github.com/mark3labs/mcp-go`.

**Spec:** `docs/2026-06-09-agent-skills-system-design.md`. **Branch:** `feat/NEX-543-skills-wiring` (off latest `main`). **Commit trailer:** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

**Depends on:** #1 + #2 merged (the `skills/` package + the 9 skills exist on `main`).

---

## File Structure

- Move: `skills/<name>/SKILL.md` → `.agents/skills/<name>/SKILL.md` (all 9).
- Create: `agentskills.go` (repo root, `package agentskills`) — embed + parse/Load/Search/Get.
- Create: `agentskills_test.go` (repo root) — the moved store + lint tests.
- Delete: `skills/embed.go`, `skills/store.go`, `skills/store_test.go`, `skills/lint_test.go` (the old package).
- Modify: `runtime/cmd/nexus-skills-mcp/tools.go` — import the root package.
- Modify: `Makefile` — add `nexus-skills-mcp` to `BINS` + `CMD_`.
- Modify: `nexus/frame/templates/embed/default/CLAUDE.md` — the `workflow-basics` pointer.

---

## Task 1: Move the SKILL.md files to `.agents/skills/`

**Files:** the 9 `SKILL.md` files.

- [ ] **Step 1: git mv all 9**

```bash
cd /Users/jacinta/Source/nexus
for s in workflow-basics spec planning development review merge release house-style security; do
  mkdir -p ".agents/skills/$s"
  git mv "skills/$s/SKILL.md" ".agents/skills/$s/SKILL.md"
done
```

- [ ] **Step 2: Verify the move**

Run: `ls .agents/skills/*/SKILL.md | wc -l`
Expected: `9`. (The `skills/` dir now holds only the `.go` files, removed in Task 2.)

---

## Task 2: Move the Go store to a repo-root package

**Files:**
- Create: `agentskills.go`
- Create: `agentskills_test.go`
- Delete: `skills/embed.go`, `skills/store.go`, `skills/store_test.go`, `skills/lint_test.go`

- [ ] **Step 1: Write the root package** (`agentskills.go`)

This is `skills/store.go` + `skills/embed.go` merged, repackaged as `agentskills`, embedding the dot-dir. Keep the CRLF normalization (Windows).

```go
// Package agentskills is the canonical store of nexus dev-lifecycle skills.
// The SKILL.md files live under .agents/skills/ — the cross-platform Agent
// Skills location that codex-cli and claude-code discover natively. This
// package (at the repo root, the only place a go:embed can reach the dot-dir)
// embeds them for the nexus-skills-mcp server, which serves the API aspects.
package agentskills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed all:.agents/skills
var files embed.FS

// Skill is one parsed SKILL.md: frontmatter fields + the markdown body.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when_to_use"`
	Body        string `yaml:"-"`
}

func parse(raw []byte) (Skill, error) {
	var s Skill
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return s, fmt.Errorf("missing frontmatter fence")
	}
	rest := raw[len("---\n"):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return s, fmt.Errorf("unterminated frontmatter")
	}
	if err := yaml.Unmarshal(rest[:end], &s); err != nil {
		return s, fmt.Errorf("frontmatter yaml: %w", err)
	}
	if strings.TrimSpace(s.Name) == "" {
		return s, fmt.Errorf("frontmatter: name required")
	}
	s.Body = string(rest[end+len("\n---\n"):])
	return s, nil
}

// Load parses every SKILL.md embedded under .agents/skills.
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
// (case-insensitive). Empty query returns all.
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
		if strings.Contains(strings.ToLower(s.Name+" "+s.Description+" "+s.WhenToUse), q) {
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

> Confirm against `skills/store.go` on `main` and carry any field/behaviour that differs (this mirrors the merged version: CRLF normalization, the `Skill` shape, the three exported funcs).

- [ ] **Step 2: Move the tests** (`agentskills_test.go`)

Copy `skills/store_test.go` + `skills/lint_test.go` content into one `agentskills_test.go`, change the package clause to `package agentskills`. The lint test's `want` set + the parse/CRLF/search tests are unchanged. Then:

```bash
git rm skills/embed.go skills/store.go skills/store_test.go skills/lint_test.go
rmdir skills 2>/dev/null || true
```

- [ ] **Step 3: Point the MCP at the root package**

In `runtime/cmd/nexus-skills-mcp/tools.go`, change the import and call sites:

```go
import (
	// ...
	agentskills "github.com/CarriedWorldUniverse/nexus"
)
// skills.Search(...) → agentskills.Search(...)
// skills.Get(...)    → agentskills.Get(...)
```

- [ ] **Step 4: Build + test**

Run: `go build ./... && go test ./... 2>&1 | grep -E "agentskills|nexus-skills-mcp|FAIL"`
Expected: the root package + the MCP tests PASS; no FAIL.

- [ ] **Step 5: Prove the MCP loads all 9 from the new location**

```bash
go build -o /tmp/nsm ./runtime/cmd/nexus-skills-mcp
for sk in workflow-basics spec planning development review merge release house-style security; do
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"p","version":"0"}}}' '{"jsonrpc":"2.0","method":"notifications/initialized"}' "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"get_skill\",\"arguments\":{\"name\":\"$sk\"}}}" | /tmp/nsm 2>/dev/null | grep -q '"id":2' && echo "$sk ok" || echo "$sk FAIL"
done
```
Expected: all 9 `ok`.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "refactor(skills): move store to .agents/skills (native CLI discovery)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Build the MCP binary in the Makefile

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add to BINS + CMD**

Line 6 is `BINS := nexus agentfunnel aspect nexus-comms-mcp nexus-imap-mcp nexus-jira-mcp nexus-watch outpost`. Append `nexus-skills-mcp`, and add the CMD line alongside the others (after `CMD_nexus-jira-mcp`):

```makefile
BINS := nexus agentfunnel aspect nexus-comms-mcp nexus-imap-mcp nexus-jira-mcp nexus-skills-mcp nexus-watch outpost
# ...
CMD_nexus-skills-mcp := ./runtime/cmd/nexus-skills-mcp
```

- [ ] **Step 2: Verify it builds via make**

Run: `make bin/nexus-skills-mcp 2>&1 | tail -2 && ls bin/nexus-skills-mcp`
Expected: the binary builds and exists.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "build: add nexus-skills-mcp to the Makefile BINS

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: The always-on pointer in the central CLAUDE.md

**Files:**
- Modify: `nexus/frame/templates/embed/default/CLAUDE.md`

- [ ] **Step 1: Add the pointer to the Development rules section**

The section currently (line 31+) is:

```markdown
## Development rules

- All code changes reviewed before deployment. The `feature-dev:code-reviewer` agent is available.
```

Insert the pointer as the first bullet:

```markdown
## Development rules

- **At the start of any task, load the `workflow-basics` skill from `nexus-skills`** (`search_skills`/`get_skill`, or your runtime's native skill loader). It directs you to the right lifecycle skill — spec, planning, development, review, merge, release — and the cross-cutting ones (security, house-style). Don't work from memory when a skill applies.
- All code changes reviewed before deployment. The `feature-dev:code-reviewer` agent is available.
```

- [ ] **Step 2: Commit**

```bash
git add nexus/frame/templates/embed/default/CLAUDE.md
git commit -m "feat(skills): point every aspect at workflow-basics in the central CLAUDE.md

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Operational wiring (documented — NOT a code change)

These run against the live broker + the aspect runtime; they are operator/shadow steps, recorded here so the PR reviewer knows what completes the wiring. Do not put them in the PR.

**A. Ship the binary to the aspect runtime.** The Makefile now builds `nexus-skills-mcp`; ensure the aspect pod image / host install picks it up alongside the other `nexus-*-mcp` binaries (confirm where `nexus-jira-mcp` etc. land and add this one the same way). The CLI builders (codex) need it only if they also use the MCP path; they get skills natively from `.agents/skills/`, so the MCP is for the API aspects (gemma/deepseek).

**B. Add the server to the API aspects' `mcp_profile`.** For each API aspect, `PUT /api/admin/aspects/{name}/mcp_profile` with the `nexus-skills` server added to the existing `{"mcpServers": {...}}`:

```json
{ "mcpServers": { "nexus-skills": { "command": "nexus-skills-mcp" } } }
```
(Merge into the aspect's existing profile, don't overwrite it.)

**C. CLI builders need the repo checked out** (they already are) so codex scans `.agents/skills/` — no action beyond the move in Task 1.

**D. Retire the pasted dev-standards.** Dispatch briefs stop pasting the dev-standards block; they say "load the `development` skill." (Shadow practice + the `reference_dev_standards` note — not a repo change here.)

---

## Task 6: Verify + push

- [ ] **Step 1: Full build + test**

Run: `go build ./... && go test ./... 2>&1 | grep -E "FAIL|ok\s" | grep -v "no test files" | tail -20`
Expected: no FAIL.

- [ ] **Step 2: Confirm the pointer + the location**

Run: `grep -c "workflow-basics" nexus/frame/templates/embed/default/CLAUDE.md && ls .agents/skills/*/SKILL.md | wc -l`
Expected: `1` (or more) and `9`.

- [ ] **Step 3: Push**

```bash
git push -u origin feat/NEX-543-skills-wiring
```

---

## Decomposition

One ticket (NEX-543) covers the repo PR (Tasks 1–4, 6). Task 5 is the operational wiring done after merge + deploy. Sub-project #4 (the CI security scanners) is separate.

## Self-Review notes (for the executor)

- **Spec coverage:** native discovery for CLI agents = the `.agents/skills/` move (T1) + the root embed (T2); the MCP still serves the API aspects (T2 import); the binary builds (T3); the always-on pointer (T4); profile wiring + retire dev-standards (T5, operational). The "delivery: native for CLI, MCP for API" design is satisfied.
- **Type consistency:** `agentskills.Skill{Name,Description,WhenToUse,Body}` + `Load/Search/Get` match the `skills` package they replace; the MCP's call sites change only the package qualifier.
- **Confirm-against-live seams:** the exact `skills/store.go` body to carry (T2 — copy from `main`, incl. the CRLF fix); the `tools.go` call sites; where the `nexus-*-mcp` binaries are installed (T5A).
- **Risk:** the root package import path is the module root (`github.com/CarriedWorldUniverse/nexus`), aliased `agentskills` — unusual but valid; the alias keeps call sites clear. `//go:embed all:.agents/skills` is verified to embed the dot-dir.
