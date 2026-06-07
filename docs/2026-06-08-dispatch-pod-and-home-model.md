# Dispatch pod & home model

**Date:** 2026-06-08
**Status:** design (worked out with the operator)
**Builds on:** `2026-06-08-named-agent-dispatch-model.md` (the dispatch *identity/threading* spine — NEX-492/494, merged + deployed). This covers the *runtime* layer: pod images, per-agent persistent home, and a funnel hook system.
**Relates:** NEX-491 (named-agent dispatch epic), NEX-493 (`pod_image`), NEX-498 (concurrent same-agent), porter (`project porter`).

## 1. Role-generic pod images

Pod images are **role-generic**, not per-aspect: a small set of base images (e.g. dev, art, training) shared by all agents of that role. The agent's *label* maps to a **role → image**, not a bespoke image per agent. Default = the dev-builder image; specialist roles (art, training) override. (The per-agent *distinctness* comes from identity + persona + home, not from a unique image.)

## 2. Per-agent persistent home — a bare git repo, worktree-expressed per run

This is the **memory/continuity layer** — what makes a dispatched agent a real team member that *remembers* across runs, rather than a fresh worker each time. Identity-scoped, versioned, failure-safe.

**Shape:** one PVC per agent tag, holding a **bare git repo** (`main` = the canonical home: memory, config, accumulated/learned state). No checked-out files sit idle between runs — the PVC is pure history.

**Lifecycle (the pod's own spawn/despawn job — entrypoint bookends, broker untouched):**
- **Spawn:** `git worktree prune` (sweep any stale admin entry from a prior crash) → `git worktree add` a `run-<id>` branch off `main`, expressing the files into the pod's **ephemeral** storage (`emptyDir`). That worktree *is* the agent's home for the run.
- **Run:** the agent reads/writes its home normally.
- **Clean despawn:** commit the tracked changes → merge `run-<id>` into `main` (in the bare repo) → `git worktree remove`. The PVC gains one merge of committed history; the working files evaporate with the pod.

**Working repos live on a separate volume, not in the home.** The agent's repo clones / working space mount at `~/repos` (or `~/src`) on a **separate volume** from the home — physically decoupling the durable, versioned home (memory/config) from large, recreatable clones. That volume is tunable on its own:
- **ephemeral** (`emptyDir`) — fresh clone per run; simplest, self-cleaning; or
- **persistent cache** (PVC) — clones persist so spawns `git pull` instead of full-cloning (fast), at the cost of occasional GC and per-run isolation when concurrent (a clone worktree per run, mirroring the home model).

The home repo additionally **`.gitignore`s** the repos path + caches/build-artifacts as defense-in-depth, so nothing recreatable ever lands in the versioned home even if something writes there. Net: **home = memory/config (small, durable, versioned); repos = working space (separate, recreatable, fast).** Start ephemeral; add the persistent git-cache as an optimization.

**Why this model:**
- **Versioned, auditable memory** — every run's home changes are commits; you can diff how an agent's memory evolved and revert a bad memory.
- **Failure-safe** — a crashed pod loses only its *uncommitted, ephemeral* deltas; `main` is always clean committed-history. The only residue is a stale worktree admin entry, swept by `prune` on the next spawn. No heavy reconcile needed.
- **Concurrency-ready** — concurrent same-agent runs are just multiple worktrees off the one bare repo; git ref-locking serialises the merges. One-at-a-time wants RWO on the PVC; concurrent (NEX-498) wants RWX + git locking. Extends without redesign. (This resolves the *home-state* facet of NEX-498; the *WS one-session-per-name* facet is separate and still wants derived sub-identities.)
- **No idle clutter** — bare repo between runs; nothing to corrupt or clean.

Relation to **porter**: the home store is a focused instance of porter's versioned-checkin / type-aware-merge idea, scoped to agent homes.

## 3. Funnel hook system

A generic hook system on the headless CLI the funnel drives (claude-code / codex), giving the platform clean lifecycle seams to observe/intervene **without** modifying each agent:
- **Memory** — inject relevant memory at session-start; capture/commit memory at stop (ties into §2's home).
- **Tool use** — observe / gate / audit tool calls (ties into the custodian credential layer + observability).
- **Lifecycle** — compaction, notifications, session start/end.

The funnel already does *ad-hoc* versions (the judge, the rewriter, observability hooks); a real hook system **generalises** these into one extensible mechanism instead of bolted-on special cases.

**Action — hooks survey:** survey Claude Code's hooks (`PreToolUse`/`PostToolUse`/`UserPromptSubmit`/`SessionStart`/`SessionEnd`/`Stop`/`SubagentStop`/`PreCompact`/`Notification` + the memory/skills systems) and Gemini CLI's hooks/extensions side-by-side → recommend which to wire into the funnel and how. Feeds the funnel-hooks implementation.

## Implementation deltas (→ tickets, under NEX-491)

1. **NEX-493 → role-generic `pod_image`** (label → role → image; default dev-builder; specialists override). *(refine)*
2. **Per-agent home = bare-repo worktree-merge store** — PVC bare repo per agent; pod entrypoint does prune+worktree-add (spawn) and commit+merge+remove (despawn); `EnsureHomeRepo(agent)` alongside the keyfile-secret ensure. Plus a **separate repos volume** at `~/repos`/`~/src` (ephemeral, or persistent git-cache) for working clones; home `.gitignore`s recreatable paths as defense. *(new)*
3. **Funnel hook system** — generic hooks on the headless CLI (memory / tool-use / lifecycle), generalising the judge+rewriter+observability. *(new)*
4. **Hooks survey** — CC + Gemini CLI hooks → funnel recommendation. *(research, feeds #3)*
