---
name: merge
description: How to land a reviewed PR on nexus — CI green including security scans, squash and delete, never force.
when_to_use: When a PR is reviewed and ready to merge to main.
---

# merge

Don't merge until it's actually done and green. A PR that "should pass" is not ready.

## Steps
1. Confirm the PR is reviewed and the work is complete.
2. Wait for CI to be green on all legs — including the security scans (load the security skill if unsure what must pass).
3. If a leg fails on a known flake (e.g. a timing test), re-run that leg. Read the failure first to be sure it's a flake, not a real break — don't re-run blind.
4. If a leg fails for a real reason, it's not ready. Fix it; don't merge around it.
5. Merge with squash and delete the branch.
6. Summarise back what landed.

## Rules
- NEVER use `--admin` to bypass required checks. Green means green.
- Merge authority is the operator's standing pre-authorization to merge after review. Pause for destructive or cross-cutting changes.
- After merge, the work isn't live yet — load the release skill to deploy and verify.
