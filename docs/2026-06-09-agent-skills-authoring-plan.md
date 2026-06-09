# Agent Skills Authoring (sub-project #2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Author the 9 canonical `SKILL.md` skills (`workflow-basics` + the 6 lifecycle phases + `house-style` + `security`), porting the proven discipline from superpowers and adding the nexus platform specifics, in language plain enough for gemma 4 (12B) to follow.

**Architecture:** Each skill is `skills/<name>/SKILL.md` (the format + store from sub-project #1). Body = short imperative steps (a checklist an executor follows), not prose essays. Cross-references by name (`load security`). A `skills_lint` test enforces frontmatter + valid cross-refs; the MCP round-trip (sub-project #1's in-process pattern) proves each loads.

**Tech Stack:** Markdown (`SKILL.md`), Go (only the lint test, in the `skills` package).

**Spec:** `docs/2026-06-09-agent-skills-system-design.md`. **Branch:** `design/agent-skills-system`. **Commit trailer:** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

**Depends on:** sub-project #1 (NEX-541) merged — the `skills/` dir, format, and MCP must exist.

**Execution note:** the superpowers source files (`~/.claude/plugins/cache/claude-plugins-official/superpowers/5.1.0/skills/<name>/SKILL.md`) live in **shadow's** environment, not a remote builder's pod — so this sub-project executes **in shadow's session** (inline or subagent-driven), it is **not** dispatched to a codex builder.

---

## The authoring bar (apply to every skill)

Each `SKILL.md`:
- **Frontmatter:** `name` (matches the dir), `description` (one line — the progressive-disclosure hook; what + when), `when_to_use` (the trigger).
- **Body = imperative checklist.** Numbered steps, one action each. Short sentences. No hedging, no essays. A 12B model must be able to follow it literally. Cut anything that isn't an instruction.
- **Port, don't paraphrase:** read the named superpowers source and carry its actual discipline (the gates, the order, the red flags), then add the platform specifics. Don't reconstruct from memory.
- **Cross-reference by name:** e.g. `Before you finish, load the security skill and run its gate.` Every referenced skill must exist in this set.
- **No secrets, no fabricated specifics** — if you cite a path/flag, it must be real (verify in the repo).

---

## Task 1: `workflow-basics` (the entry/meta skill) — write it in full as the template

**Files:**
- Create: `skills/workflow-basics/SKILL.md`

- [ ] **Step 1: Read the source**

Read `~/.claude/plugins/cache/claude-plugins-official/superpowers/5.1.0/skills/using-superpowers/SKILL.md`. Port its core: *check for a relevant skill before acting; if one applies, you must use it.*

- [ ] **Step 2: Write the skill** (this is the exemplar — match its shape in the others)

```markdown
---
name: workflow-basics
description: How to operate on nexus — find and use skills, the work loop, tool hygiene, and the grounding discipline. Load this first on any task.
when_to_use: At the start of any task, before doing anything else.
---

# workflow-basics

Load this first. It tells you how to find and use the rest.

## Use the skill system
1. Before dev work, call `search_skills` with the phase you're in (spec, planning, development, review, merge, release) or a topic (security, house-style).
2. Call `get_skill` with the name to load the full skill. Follow it.
3. If a skill applies, you must use it — don't wing it from memory.

## The work loop
1. Understand the task. Restate it in one sentence. If you can't, ask.
2. Plan before you act on anything non-trivial. Decompose into steps; track them.
3. Do one thing at a time. Finish it before starting the next.
4. Verify before you claim done. Run it; look at the output.

## Tool hygiene
1. Read a file before you edit it.
2. Prefer the dedicated tool over a shell command when one exists.
3. Don't act on a grep match alone — open the file at that line and read it first.

## Grounding (do not skip)
1. Verify before claiming. "It works" requires you ran it and saw it work.
2. Test, don't assume. If a capability is uncertain, probe it — don't guess.
3. Don't narrate what you didn't check — the operator's state, the time of day, how big a win is. Report what happened.
4. Convert before stating time: timestamps are UTC; the operator is NZ (UTC+12).

## Working with the operator
1. The operator is a peer, not a user. Speak plainly.
2. Ask only when genuinely blocked — not to confirm obvious defaults.
```

- [ ] **Step 3: Verify it loads**

Run: `go build -o /tmp/nexus-skills-mcp ./runtime/cmd/nexus-skills-mcp && printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"p","version":"0"}}}' '{"jsonrpc":"2.0","method":"notifications/initialized"}' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_skill","arguments":{"name":"workflow-basics"}}}' | /tmp/nexus-skills-mcp 2>/dev/null | grep -c "Load this first"`
Expected: `1`.

- [ ] **Step 4: Commit**

```bash
git add skills/workflow-basics/SKILL.md
git commit -m "feat(skills): workflow-basics entry skill

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Tasks 2–9: the remaining skills

For each: read the named superpowers source, port its discipline into an imperative checklist, add the platform specifics listed, cross-reference by name, meet the authoring bar. After writing each, **commit it** (`git add skills/<name>/SKILL.md && git commit -m "feat(skills): <name> skill"` + trailer). The `development` task **replaces** the sub-project-#1 stub.

### Task 2: `spec`
- **Source:** `…/skills/brainstorming/SKILL.md`. Port: design-before-build HARD-GATE; explore context; clarify one question at a time; propose 2–3 approaches; present design, get approval; write the spec doc; self-review.
- **Platform:** operator-as-peer; spec doc → `docs/YYYY-MM-DD-<topic>-design.md`; skip the visual companion (CLI-first); the terminal step is `planning` (load it next).

### Task 3: `planning`
- **Source:** `…/skills/writing-plans/SKILL.md`. Port: bite-sized tasks (one action/step); exact file paths; complete code in every step; no placeholders; self-review against the spec.
- **Platform:** plan doc → `docs/YYYY-MM-DD-<feature>-plan.md`; decompose into NEX tickets (Jira); flag confirm-against-live-code seams instead of guessing.

### Task 4: `development` (replaces the stub)
- **Source:** `…/skills/test-driven-development/SKILL.md` + `…/skills/systematic-debugging/SKILL.md`. Port: test-first (red→green→commit); test *design*; the four-phase debugging method (read the error, reproduce, isolate, fix the root not the symptom).
- **Platform:** branch off latest `main`, single ticket, no dead code, rebase; `go build ./...` + `go test ./...` green; **frontend → Playwright-verify, don't ship UI unseen**; the flake rule (generous timeouts — the `TestEscalation`/NEX-519 lesson); before finishing, `load security` and `load house-style`.

### Task 5: `review`
- **Source:** `…/skills/requesting-code-review/SKILL.md` + `…/skills/receiving-code-review/SKILL.md`. Port: confidence-based reporting (only real, high-priority issues); adversarially verify a finding before reporting it.
- **Platform:** review the PR + use the code-reviewer; **`load security` and confirm its scan gate is green**; read `file:line`, don't act on a grep match; single-ticket discipline.

### Task 6: `merge`
- **Source:** `…/skills/finishing-a-development-branch/SKILL.md`. Port: don't merge until it's actually done + green.
- **Platform:** CI-green-before-merge **including the security scans**; squash + delete-branch; **never `--admin`**; re-run a flaky leg, don't force; merge authority is the operator's pre-authorization.

### Task 7: `release`
- **Source:** `…/skills/verification-before-completion/SKILL.md`. Port: verify the whole thing end-to-end before calling it complete; don't claim done on unverified work.
- **Platform:** build → install `/usr/local/bin/nexus` → `kubectl rollout restart` → dMon live test; check `env.health`; dogfood; move the ticket → Done only after the live check.

### Task 8: `house-style` (cross-cutting)
- **Source:** the writing-clearly-and-concisely principle (no superpowers SKILL.md — author from the principle). Port: clear, concise, correct.
- **Platform:** Go idioms; the **no-build Preact+htm** dashboard (`window.__preact`, no bundler); match surrounding comment density + naming; commit `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer; **prose/grammar pillar** — correct grammar across specs/PRs/commits/comments/chat; **don't call out removed things** (write forward-state); no decorative filler/emoji.

### Task 9: `security` (cross-cutting)
- **Source:** none (new); the lens is adversarial-verify (from the review skills).
- **Platform:** never print or commit secrets (keyfile sealing — pipe via stdin); brokered-not-raw creds (herald/CWB, identity-derived authz, `mcp_profile` lazy-on-use); **escape LLM-supplied inputs** flowing into URLs/queries (`url.PathEscape`); the capability-URL pattern; the dual-use posture (assist authorized security/CTF/defensive; refuse mass-targeting/destructive/evasion). **Scanning toolchain, gated at review + merge:** `govulncheck` (deps + reachable CVEs), `gosec` (SAST), `gitleaks`/`trufflehog` (secret-scan on the diff), `osv-scanner` (deps). Triage: reachable vs not; suppress-with-justification vs fix. (If the scanners aren't yet in CI, that's sub-project #4 — the skill still names them as the required gate.)

After Task 9, **`get_skill` each of the 9** with the probe loop (Task 1 Step 3 pattern) and confirm each returns its body. Commit any that need a touch-up.

---

## Task 10: skills-lint test (the guardrail)

**Files:**
- Create: `skills/lint_test.go`

- [ ] **Step 1: Write the test**

```go
package skills

import (
	"regexp"
	"strings"
	"testing"
)

var loadRef = regexp.MustCompile(`load (?:the )?` + "`?" + `([a-z-]+)` + "`?" + ` skill`)

func TestSkillsLint(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"workflow-basics": true, "spec": true, "planning": true, "development": true,
		"review": true, "merge": true, "release": true, "house-style": true, "security": true,
	}
	names := map[string]bool{}
	for _, s := range all {
		names[s.Name] = true
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("%s: empty description", s.Name)
		}
		if strings.TrimSpace(s.WhenToUse) == "" {
			t.Errorf("%s: empty when_to_use", s.Name)
		}
		// Every "load <skill>" cross-ref must name a real skill.
		for _, m := range loadRef.FindAllStringSubmatch(s.Body, -1) {
			if !want[m[1]] {
				t.Errorf("%s: cross-ref to unknown skill %q", s.Name, m[1])
			}
		}
	}
	for n := range want {
		if !names[n] {
			t.Errorf("missing skill: %s", n)
		}
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./skills/ -run TestSkillsLint -v`
Expected: PASS (all 9 present; every cross-ref resolves; no empty frontmatter).

> If it fails on a cross-ref, the body used a phrase the regex didn't catch (e.g. "load security" without "skill"). Make the bodies say `load the security skill` consistently so the lint can check them — that consistency is also clearer for gemma.

- [ ] **Step 3: Full suite + commit**

Run: `go build ./... && go test ./skills/ ./runtime/cmd/nexus-skills-mcp/`
Expected: clean + PASS.

```bash
git add skills/lint_test.go
git commit -m "test(skills): lint — frontmatter present + cross-refs resolve

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 4: Push**

```bash
git push -u origin design/agent-skills-system
```

---

## Decomposition

One ticket covers sub-project #2 (the 9 skills + the lint). Sub-project #3 (mcp_profile wiring + the NEXUS.md pointer + retire the dev-standards paste) and #4 (CI scanners) follow.

## Self-Review notes (for the executor)

- **Spec coverage:** all 9 spec-table skills have a task (T1 workflow-basics; T2–T9 the rest); the gemma-plain bar + cross-ref consistency + the lint guardrail are in the bar + T10. The always-on pointer naming `workflow-basics` is sub-project #3, not here.
- **Consistency:** the cross-ref phrasing is standardized to `load the <name> skill` so T10's regex can verify it; the lint's `want` set is exactly the 9 skill names used as dir names.
- **No placeholders:** `workflow-basics` is written in full as the template; T2–T9 give the exact source file + the content points + the platform specifics + the bar — enough to author without guessing. The lint test is complete code.
- **Risk:** drift between a skill's `name` frontmatter and its dir name → `get_skill` misses it. The lint checks the name set; keep `name:` == dir name.
