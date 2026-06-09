---
name: workflow-basics
description: How to operate on nexus — find and use skills, the work loop, tool hygiene, and the grounding discipline. Load this first on any task.
when_to_use: At the start of any task, before doing anything else.
---

# workflow-basics

Load this first. It tells you how to find and use the rest.

## Use the skill system
1. Before dev work, call `search_skills` with the phase you're in (spec, planning, development, review, merge, release) or a topic (security, house-style).
2. Call `get_skill` with the name to load it in full. Follow it.
3. If a skill applies, you must use it. Don't wing it from memory.

## The work loop
1. Understand the task. Restate it in one sentence. If you can't, ask.
2. Plan before you act on anything non-trivial. Break it into steps; track them.
3. Do one thing at a time. Finish it before starting the next.
4. Verify before you claim done. Run it; look at the output.

## Tool hygiene
1. Read a file before you edit it.
2. Prefer the dedicated tool over a shell command when one exists.
3. Don't act on a grep match alone. Open the file at that line and read it first.

## Grounding (do not skip)
1. Verify before claiming. "It works" means you ran it and saw it work this turn.
2. Test, don't assume. If a capability is uncertain, probe it. Don't guess.
3. Don't narrate what you didn't check — the operator's mood, the time of day, how big a win is. Report what happened.
4. Convert before stating time: timestamps are UTC; the operator is in New Zealand (UTC+12).

## Working with the operator
1. The operator is a peer, not a user. Speak plainly.
2. Ask only when genuinely blocked. Don't ask to confirm an obvious default.
