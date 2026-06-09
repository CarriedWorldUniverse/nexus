---
name: planning
description: How to turn an approved spec into a bite-sized, no-placeholders implementation plan.
when_to_use: After a spec is approved, before implementation.
---

# planning

Write the plan so someone with zero context for this codebase can execute it. Document the files, the code, the tests, and how to verify.

## Steps
1. Map the file structure first: which files are created or modified, and what each is responsible for. One clear responsibility per file; prefer small focused files.
2. Break the work into tasks. Each task is self-contained. Each step inside a task is one action (write the failing test, run it, implement, run it, commit).
3. Write complete content in every step. If a step changes code, show the code. Exact file paths. Exact commands with expected output.
4. Follow TDD in the steps: failing test first, then the minimal code.
5. Self-review against the spec: every spec requirement maps to a task; method/property names are consistent across tasks; no placeholders.

## No placeholders
Never write "TBD", "implement later", "add error handling", "write tests for the above", or "similar to Task N". Repeat the real content. These are plan failures.

## Platform
- Save the plan to `docs/YYYY-MM-DD-<feature>-plan.md` and commit it.
- Decompose the work into single-ticket units (one NEX ticket per PR).
- Where the plan depends on existing code you haven't confirmed, flag it as a "confirm against live code" seam — don't invent APIs.
- To execute the plan, load the development skill.
