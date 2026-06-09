---
name: review
description: How to review a change before merge — confidence-based, verify findings, enforce the security gate; and how to receive review without performative agreement.
when_to_use: When reviewing a PR, or when receiving review feedback on your own work.
---

# review

## Reviewing a change
1. Review the diff for real, high-priority issues: bugs, logic errors, security holes, broken conventions. Report only what truly matters — don't pad with nits.
2. Adversarially verify each finding before you report it. Assume you're wrong; try to refute the issue. If you can't refute it, it's real.
3. Read the actual `file:line` for every claim. Don't report from a grep match alone — open it and walk the logic.
4. Load the security skill and confirm its scan gate is green for this change.
5. One ticket per review. Flag scope creep.
6. Rank findings: critical (fix now), important (fix before merge), minor (note for later).

## Receiving review (no performative agreement)
Verify before implementing. Ask before assuming.
1. Read the whole feedback without reacting.
2. Restate each item in your own words, or ask if it's unclear.
3. Check each against the codebase reality — is it sound for THIS code?
4. If feedback is wrong, push back with technical reasoning. If right, just fix it.
5. Fix one item at a time; test each.
- Never reply "You're absolutely right" / "Great point". Respond with the technical substance or the action.
- If any item is unclear, stop and ask before implementing anything — items may be related.
