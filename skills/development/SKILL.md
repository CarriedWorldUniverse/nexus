---
name: development
description: How to implement a dispatched ticket on nexus - test-first, single-ticket discipline, verify before opening a PR.
when_to_use: When you are writing or changing code to implement a ticket, before opening a PR.
---

# development

Implement one ticket per branch, branched off the latest `main`.

## Loop
1. Write a failing test first; run it; see it fail for the right reason.
2. Write the minimal code to pass; run the tests green.
3. No dead code, no unrelated changes - single ticket only.
4. Verify before you open the PR: `go build ./...` and `go test ./...` green. For frontend, browser-verify (Playwright) - do not ship UI unseen.
5. Also load `security` (scan + secret hygiene) and `house-style` (conventions) before you finish.

## Definition of done
Branch pushed, single-ticket PR open, CI green (including security scans), PR description states what changed and how it was verified.
