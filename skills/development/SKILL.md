---
name: development
description: How to implement a dispatched ticket on nexus — test-first, root-cause debugging, single-ticket discipline, verify before the PR.
when_to_use: When writing or changing code to implement a ticket, before opening a PR.
---

# development

One ticket per branch, branched off the latest `main`. No dead code, no unrelated changes.

## Test-first (the iron law)
NO PRODUCTION CODE WITHOUT A FAILING TEST FIRST.
1. Write one small failing test for the behaviour you want.
2. Run it. Watch it fail for the right reason. (If you didn't watch it fail, you don't know it tests the right thing.)
3. Write the minimal code to pass.
4. Run it. Green.
5. Commit. Repeat for the next behaviour.
If you wrote code before the test, delete it and start from the test.
Exceptions (ask the operator first): throwaway prototypes, generated code, config.

## When something breaks (root cause first)
NO FIXES WITHOUT FINDING THE ROOT CAUSE.
1. Read the error message fully — it usually says what's wrong.
2. Reproduce it reliably.
3. Isolate the smallest failing case.
4. Fix the cause, not the symptom. A patch that hides the symptom is a failure.

## Test design
- Test behaviour, not implementation detail.
- Timing/wait deadlines must be generous (a max, returned-on-success). Tight deadlines flake in CI.
- For provider/integration code, env-gate live tests so they're opt-in.

## Platform
- Branch `builder/<TICKET>` off latest `main`. Rebase, don't merge `main` in.
- `go build ./...` and `go test ./...` must be green before the PR.
- Frontend: browser-verify it (Playwright) before shipping. Don't ship UI you haven't seen rendered.
- Before you finish, load the security skill and load the house-style skill, and apply them.

## Done means
Branch pushed, single-ticket PR open, CI green (including security scans), PR description says what changed and how you verified it. Then load the review skill.
