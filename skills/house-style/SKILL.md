---
name: house-style
description: How code and prose should read on nexus — Go idioms, the no-build dashboard, commit conventions, clear correct writing.
when_to_use: While writing any code, comment, commit, spec, PR, ticket, or chat message.
---

# house-style

Write so it reads like the rest of the work. Match what's already there.

## Code
1. Go: standard idioms, `gofmt`, errors wrapped with context, small focused files.
2. The dashboard is no-build Preact + htm: `window.__preact`, no bundler, no npm build step. Don't add a build toolchain.
3. Match the surrounding comment density and naming. Don't over-comment or rename for taste.
4. Commit messages: a clear subject line, body explaining why, and the trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
5. One ticket per PR.

## Prose (specs, PRs, comments, tickets, chat)
1. Clear, concise, correct grammar. Short sentences. Say the thing.
2. Write the forward state. Don't call out what was removed ("removed X", "X no longer exists", "deprecated") — describe what is, not what was.
3. No decorative filler, no hype, no emoji padding.
4. Report what you did and what you checked. Don't narrate the reader's feelings or the moment.
