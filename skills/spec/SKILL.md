---
name: spec
description: How to turn an idea into an agreed design before any code — clarify, explore approaches, write the spec, get approval.
when_to_use: When the operator brings a new feature or change and there is no agreed design yet.
---

# spec

Design before you build. Do not write code or scaffold anything until a design is written and the operator has approved it. This holds even when the task looks simple — simple tasks are where unexamined assumptions waste the most work.

## Steps
1. Explore the context first. Read the relevant files, docs, recent commits.
2. Ask clarifying questions one at a time. Prefer multiple-choice. Cover purpose, constraints, success criteria.
3. If the request is really several subsystems, say so and decompose before designing.
4. Propose 2–3 approaches with trade-offs. Lead with your recommendation and why.
5. Present the design in sections. Get the operator's approval before moving on.
6. Write the spec to `docs/YYYY-MM-DD-<topic>-design.md` and commit it.
7. Self-review the spec: no TBD/placeholders, no contradictions, no ambiguity, single focused scope. Fix inline.
8. Ask the operator to review the written spec. Only proceed when they approve.

## Rules
- The operator is a peer. Don't survey endlessly — recommend.
- Skip the visual companion; this team is CLI-first.
- After the spec is approved, load the planning skill.
