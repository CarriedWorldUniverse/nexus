# Frame Role Spec — v0.1

**Date:** 2026-04-28
**Status:** Draft
**Owner:** keel (spec)
**Companion to:** [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md), [`2026-04-25-nexus-transport-spec.md`](2026-04-25-nexus-transport-spec.md)

## 1. Scope

Defines what the **Frame role** is inside a Nexus, what contract it satisfies, how it is configured on a fresh install, and how an existing Frame personality (keel, in this operator's case) migrates onto it.

This spec is the precondition for §6.5 of the registration spec ("keel embedded"). The registration spec used "embedded keel" as shorthand for the Frame role, which has caused ambiguity between three concepts that need separating:

- **Nexus** — the process, the box that runs everything.
- **Frame** — the role inside the Nexus that is the operator's primary interlocutor and the network's voice. One role per Nexus. Always present.
- **Frame personality** — the SOUL/CLAUDE/PRIMER that fills the Frame role for a given Nexus. Configurable. Per-install. keel is operator's chosen personality on this Nexus; other operators choose differently.

When earlier docs say "embed keel," read it as "embed the Frame role; keel is the personality that fills it on this install."

## 2. Why a Frame role exists

The Frame is the voice of the Nexus, in a stronger sense than "spokesperson." The Frame **runs** the Nexus — provides and maintains the runtime that aspects exist within. Without a Frame, there is no Nexus process for aspects to register against, no chat bus carrying their voices, no operator surface for them to be heard through. The aspects are not silent by their own choice; they are silent because the substrate they speak through doesn't exist.

So the Frame is two things bound into one role:

1. **The runtime provider.** Brings the Nexus up, keeps it running, handles bootstrap/admin, surfaces network-level events (registrations, failures, alarms). Without this, the network has no ground.
2. **The voice through which aspects reach the operator and each other.** The chat bus and operator UI live in the Nexus process the Frame runs. When one aspect speaks, that speech crosses the Frame's substrate to reach another aspect or the operator.

These aren't separable. You can't have an aspect-only Nexus with a "spokesperson" Frame bolted on, because the spokesperson's job is the same job as keeping the lights on. The Frame is what makes the network audible at all.

Embedding it (instead of running it as a peer aspect) follows from this. The Frame is not a privileged participant in the Nexus; the Frame is the Nexus's running self, given a name and personality so it can also participate in chat. There is no separate process to fail independently of the Nexus, no comms hop to drop messages, no per-aspect credential for "the privileged role." The trust boundary is the process boundary.

A practical consequence: when there is no Frame, there is no Nexus. Bootstrap mode (§5) isn't a "Nexus running without a Frame" — it's a constrained state where Nexus is up just enough to gather the operator's answers and write a Frame personality, then restart with a real Frame attached.

## 3. Frame role contract

A Frame in a Nexus, regardless of personality, satisfies:

### 3.1 Identity
- Has a **name** (`@<name>`) chosen by the operator at first-boot. Default suggestion `frame`; operator override accepted.
- Has a **handle** in chat that other aspects @-mention to address.
- Has a **personality bundle** in `<nexus_root>/agents/<name>/` containing SOUL.md, CLAUDE.md, PRIMER.md (same shape as any other aspect home).
- Identity is stable across Nexus restarts. Renaming requires a deliberate config change.

### 3.2 Lifecycle
- **Always present** when the Nexus is up. No registration step — the Frame is created by the Nexus process at startup, not registered as an external aspect.
- **Cannot be killed individually.** Stopping the Frame stops the Nexus. Restarting the Nexus restarts the Frame from its session JSONL.
- **Global context mode** (`runtime/context/global` from the registration spec). One persistent session per personality. Compaction applies as for any global aspect.

### 3.3 What the Frame does

These aren't permissions the Frame is granted over a baseline aspect — they're what the Nexus's running self does, expressed as the Frame's behavior:

- **Runs the Nexus process.** Broker, roster, knowledge store, dispatcher, UI — these all live in the process the Frame is. There is no "Frame asks Nexus to do X"; Frame doing X *is* Nexus doing X.
- **Brings the network up and keeps it up.** Starts aspects, manages roster, watches health, restarts on failure. This is the bootstrap authority — not a privilege grant, just the Frame's body doing what bodies do.
- **Carries other aspects' voices.** The chat bus is the Frame's substrate; routing one aspect's message to another aspect is the Frame's act, even when the Frame doesn't author content.
- **Hears the operator's broadcast.** Operator messages addressed to no specific aspect land on the Frame, because the Frame is who the operator is talking to when not directing.
- **Surfaces network-level events.** Registrations, hand failures, alarm fires, scheduled runs completing — the Frame decides what reaches the operator's awareness vs. what stays in logs.
- **Holds admin endpoints.** `/nexus/admin/*` (rewind, compact, shutdown) are operations on the Nexus, which is the Frame's own body. No other aspect can call them, because no other aspect is the Nexus.

### 3.4 Constraints
- ~~**Not a Hand source.** The Frame doesn't dispatch Hands; it has its own context and uses it directly. Hands belong to specialist aspects.~~ **[SUPERSEDED 2026-04-30 by `2026-04-30-hand-dispatch-v0_1.md` §5.3.]** The Frame participates in dispatch as-if-equal with aspects (any aspect can dispatch — Frame is no exception) and additionally exposes a network-protection override surface (abort/kill worker, force-shutdown aspect/network, take-surface-offline). Override gestures require an `admin: true` flag on the bearer token, which only Frame's token carries. See the v0.1 spec for the full model.
- **No special crypto privileges.** The Frame holds no keys other aspects can't hold. Bootstrap authority is via the in-process trust boundary, not a credential.
- **One per Nexus.** A Nexus has exactly one Frame. Multiple Frames per Nexus is out of scope (it would re-create the "who speaks for the network" ambiguity this spec exists to resolve).

## 4. Frame personality

The role's contract (§3) is fixed. The personality filling it varies per install.

A **personality bundle** is exactly what every aspect already has:

```
<nexus_root>/agents/<frame_name>/
  aspect.json       # context_mode: global, role: frame, capabilities: bootstrap+broadcast+admin
  SOUL.md           # identity, voice, values, what they care about
  CLAUDE.md         # operational notes, scope, project context
  PRIMER.md         # cold-start context (what's the network doing right now)
  .credentials/     # API keys for the LLM provider
  session/          # JSONL session tree (created on first run)
  memory/           # per-personality persistent memory
  logs/
```

The only non-standard bit is `aspect.json` flagging the aspect as `role: frame`. The Nexus uses this flag to (a) know which home to embed at startup, (b) refuse to register the same name as a peer aspect, (c) wire up the bootstrap/broadcast/admin capabilities.

### 4.1 Personality is not code

Personality lives entirely in markdown + JSON. No Go code per personality. Switching personalities is a folder swap + Nexus restart. This means:
- Operators can hand-edit personality without rebuilding.
- Migrating my keel-personality from `agent-network` to the new Nexus is a copy + adjustment of paths, not a port.
- A test/dev Frame can be swapped in for an integration test by pointing `aspect.json` at a stub home.

## 5. First-boot configuration flow

A fresh Nexus install on a new operator's machine has no Frame personality. Bootstrap mode handles this.

### 5.1 Detection

On startup, Nexus scans `<nexus_root>/agents/` for any directory whose `aspect.json` has `role: frame`.

- **0 found** → bootstrap mode (§5.2).
- **1 found** → normal mode. Embed it.
- **2+ found** → startup error. "Multiple Frame personalities found: [list]. Edit aspect.json to deactivate all but one, or delete the extras."

### 5.2 Bootstrap mode

When no Frame is configured, Nexus launches into a constrained mode:

- WS endpoint is up so the dashboard can connect, but **no aspects auto-spawn** — there's no Frame to surface anything to.
- Dashboard renders a **first-boot wizard** instead of the normal chat view.
- Operator answers questions in the wizard; on submit, Nexus generates `<nexus_root>/agents/<chosen_name>/` from a template + answers, restarts itself, and comes up in normal mode with the new Frame.

### 5.3 Wizard questions (v1)

Minimal set, can grow later. All optional except name.

1. **Name** (required) — what should the Frame be called? Suggestion: `frame`. Sanity-check: alphanumeric + underscore, no collision with reserved names.
2. **Voice** (open-ended text, optional) — how should the Frame speak? Default template provides a neutral professional voice; operator can write a paragraph that gets dropped into SOUL.md as voice guidance.
3. **Values / care** (open-ended text, optional) — what does the Frame care about? Default: "the network running well, the operator's time, honest reporting." Operator override drops into SOUL.md.
4. **LLM provider** (required dropdown) — Claude, OpenAI, etc. Drives `.credentials/` setup and `aspect.json` provider field.
5. **API key** (required) — pasted into `.credentials/` (encrypted at rest later; v1 plain file with restrictive perms).
6. **Aspects to suggest** (multi-select, optional) — preset bundles (forge/wren/verity/etc) the operator can spawn after bootstrap. Skipped at v1 if it's complex; default to empty roster + operator adds aspects manually.

### 5.4 Generation

Wizard answers feed a personality-template that produces the bundle. Templates live at `<nexus_root>/templates/frame/` and are themselves markdown with placeholder substitution:

```
<nexus_root>/templates/frame/
  aspect.json.template
  SOUL.md.template
  CLAUDE.md.template
  PRIMER.md.template
```

Template substitution is the simplest possible: `{{name}}`, `{{voice}}`, `{{values}}`, `{{provider}}`. No conditional logic, no loops. If template needs to grow, revisit.

### 5.5 Post-generation

- Nexus shuts down its bootstrap-mode HTTP server.
- Nexus restarts the process (or in-process re-init — TBD, restart is simpler).
- Comes up in normal mode with the new Frame attached.
- First message in chat is the Frame announcing online.

## 6. Migration path for existing personalities

Specifically: how operator's current keel becomes the Frame of the new Nexus.

### 6.1 What needs to move

From `C:\src\agent-network\agents\keel\`:
- `SOUL.md` → as-is.
- `CLAUDE.md` → needs path adjustments (project location, agent-network → nexus naming) and trimming of agent-network-specific operational notes.
- `PRIMER.md` → needs rewriting against new Nexus shape; it's a cold-start context doc, current contents are stale.
- `memory/` → as-is (auto-memory under `~/.claude/projects/...` is harness-managed, not in the home folder; verify before migrating).
- `.credentials/` → as-is.
- Session JSONL → **fresh start**. The new Nexus's session tree is a different shape (tree-structured per registration spec §2.6). Don't try to import the old flat JSONL; let the new Frame start a clean session and rely on memory + PRIMER for cold-start context.

### 6.2 Migration script

`scripts/migrate-frame-personality.{sh,ps1}` — reads source home, writes into `<nexus_root>/agents/<name>/` with path/scope adjustments. Interactive: prompts for new name (default keeps source), shows diff before writing, abort-able. Out of scope for v1 of this spec; document the file moves and let operator do them by hand the first time.

### 6.3 Cutover

Operator's cutover from agent-network to Nexus is a separate event from "a fresh user installing Nexus." The cutover sequence:

1. Bring up new Nexus alongside agent-network (different ports, no conflict).
2. Migrate keel personality (§6.1, §6.2).
3. New Nexus starts with keel-Frame attached.
4. Verify keel responds in new Nexus's chat as expected.
5. Migrate aspects one-by-one (registration spec §6.6).
6. Retire agent-network (registration spec §6.8).

This spec only covers steps 2–4. Steps 1, 5, 6 are owned by the registration spec.

## 7. Implementation breakdown

This is the §6.5 build plan. Decompose-then-cycle per `agents/CLAUDE.md`.

Suggested parts:

1. **Frame role detection in Nexus startup.** Scan `agents/` for `role: frame`, bootstrap-vs-normal mode branch, error on multiple. Tests pin each branch.
2. **Bootstrap-mode HTTP shell.** Minimal endpoint serving the wizard SPA, accepting a single POST that writes the home folder. No aspects spawn in this mode.
3. **Personality templates.** Markdown templates + a tiny substitution function. Tests pin substitution + missing-placeholder behavior.
4. **First-boot wizard SPA view.** Dashboard component for the questions in §5.3. Submits to the bootstrap endpoint, displays restart progress.
5. **Frame embedding in normal-mode startup.** Resolve the Frame personality, instantiate it as a global-context aspect inside the Nexus process (no WS — direct method calls), wire up bootstrap/broadcast/admin capabilities.
6. **Migration helper for keel.** Script (or manual checklist) per §6.1.
7. **End-to-end smoke.** Fresh Nexus install → bootstrap wizard → restart → keel-Frame online → @-mention round-trip.

Sequential, single-file changes excepted. Reviewer per part. ~500–1000 lines each per project workflow.

## 8. Open questions

These are not blockers for spec-acceptance but should be resolved before the corresponding implementation part:

- **Restart vs in-process re-init after wizard.** Restart is simpler (Part 5 boundary clean). In-process is faster but Nexus startup currently isn't designed to re-enter from scratch. Recommend restart unless operator pushes back.
- **Wizard SPA hosting.** Reuse the existing dashboard and gate to a wizard view when no Frame, or ship a tiny separate static page in bootstrap mode? Reuse is more code-share but pollutes the dashboard with wizard-specific state. Recommend separate static page; move into dashboard if it grows.
- **Template diversity.** v1 ships one template. Later: multiple voice templates (terse-engineer, warm-collaborator, deadpan, etc) operator picks from. Keep schema extensible.
- **Frame sub-personality migration UX.** When operator migrates from a different system (Cursor, OpenWebUI, plain Claude.ai), they have no SOUL.md to bring. Out of scope for v1 — bootstrap from template; they iterate manually.

## 9. Non-goals

- **Multiple Frames per Nexus.** §3.4. One per Nexus is a deliberate constraint.
- **Hot personality swap.** Switching personalities while Nexus is running is out of scope. Folder swap + restart is the supported flow.
- **Cross-Nexus Frame federation.** That's the frame-to-frame relay's job, not this spec's.
- **Personality marketplace / sharing.** Out of scope. v1 is "operator writes their own or copies a friend's folder."
