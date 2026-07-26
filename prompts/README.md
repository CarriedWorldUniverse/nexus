# prompts — the network's system prompts, as source

Every identity the network runs boots with a system prompt assembled from two
DB tables:

| what | where | scope |
|---|---|---|
| central policy | `nexus_settings.nexus_md` (single row, int version) | every identity |
| per-aspect delta | `aspect_personalities` (`nexus_md`, `soul_md`, `primer_md`) | one aspect |

Until this directory existed, those were editable **only as live rows**. No
diff, no review, no history beyond a version counter — you could not see what
v3 said, or why v4 replaced it. This directory makes the repo the source of
truth and the DB a deployment target.

## Layout

    central/nexus-md.live.md    verbatim capture of the LIVE central prompt
    central/parts/common.md     draft: policy shared by every identity
    central/parts/aspect.md     draft: policy for long-lived interactive aspects
    central/parts/worker.md     draft: policy for headless one-shot workers
    central/composed-*.draft.md generated from parts — the strings that would be applied
    aspects/<name>/{nexus,soul,primer}.md   verbatim capture, per aspect
    MANIFEST.json               live versions at time of capture
    promptsync.sh               capture / diff / apply

## Workflow

    ./promptsync.sh diff      # is live still what the repo says?
    ./promptsync.sh capture   # pull live in (then `git diff` shows what changed underneath)

Edit here, review as a normal change, then apply:

    NEXUS_ADMIN_TOKEN_FILE=~/.nexus/admin-token \
      ./promptsync.sh apply-central central/composed-aspect.draft.md --yes

**If you edit a prompt live, capture it back the same day.** A live edit that
never lands here restores the old problem, silently.

## Why the split (drafts)

The captured `nexus-md.live.md` was last written **2026-06-09** and describes
the pre-restructure world. It is given verbatim to *every* identity — including
dispatched runs, for whom several of its instructions are actively wrong:

- *"You are an aspect of the Nexus — one of several AI identities…"* — the
  named fleet retired into pool personalities on a shared brain (2026-07-05).
- *"see your own NEXUS.md / SOUL.md / PRIMER.md"* — a dispatched run has no
  home and no such files.
- *"Surface to the operator rather than guess at intent."* — there is no
  operator in a headless run and no channel to one. A worker that stops to ask
  produces nothing and its run ends; from the outside that is indistinguishable
  from the silent run deaths of 2026-07-23.
- *"load the workflow-basics skill"* — the lifecycle skills it points at assume
  a human in the loop.

So the same string cannot serve both audiences. The drafts split it into
`common` + one of `aspect` / `worker`, keeping the shared half in one file so
it cannot drift between the two.

## How the split is delivered (NEX-827, built)

`nexus_settings` carries a second column, `nexus_md_worker`, and both resolve
paths choose between the two:

- `nexus/aspects/resolve.go` — `ResolveByName`, the path a JWT-booted worker
  takes
- `nexus/aspects/validate.go` — the keyfile handshake

Both call `NexusSettings.CentralFor(name)`, which serves the worker variant to
headless identities and the interactive text to everyone else. The
discriminator is `IsDerivedName`, the package's own predicate — it already
recognises **both** dispatched shapes, derived run identities
(`shadow.umbra`) and pool workers (`<personality>-<role>`), so nothing new
sniffs names here.

Note the asymmetry with persona: personality lookups deliberately key on
`BaseName`, because a run *inherits its parent's persona* while needing its
own *policy*. Those two pull in opposite directions on purpose, and there are
tests asserting both at once so a future simplification can't collapse them.

**The fallback is one-directional.** An empty `nexus_md_worker` means every
identity gets `nexus_md`, exactly as before — so deploying the migration
changes nothing until content is written. There is no fallback the other way:
an aspect never receives worker policy.

Write it with:

    PUT /api/admin/nexus-md-worker   {"nexus_md_worker": "..."}

Posting `""` clears the variant, which is the rollback: every identity returns
to the single-prompt shape.

## Caveats — what has and has not been exercised

- `capture` and `diff` were **run against the live broker** and round-trip
  cleanly (central v4 + 9 aspects).
- `apply-central` / `apply-aspect` are **untested**: they need an admin token,
  and running them would bump the live version before anyone reviewed the
  drafts. Treat the first apply as the test, with `git` as the rollback.
- Reads go through the DB, not the API, because **the admin surface has no GET
  for prompt content** — only `PUT`. Two small GET endpoints would let
  `capture` drop its cluster dependency; worth doing.
- `composed` in `aspect_personalities` is 0 bytes for every row. That column
  is assembled at resolve time rather than stored, so it is not captured here.
