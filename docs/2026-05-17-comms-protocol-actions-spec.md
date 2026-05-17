# Comms-protocol actions — `!dispatch` (Epic A)

**Date:** 2026-05-17
**Status:** v0.1 draft
**Owner:** shadow
**Companion to:** [`docs/2026-04-30-hand-dispatch-v0_1.md`](2026-04-30-hand-dispatch-v0_1.md) (existing hand-dispatch model)
**Implements:** the operator-facing surface that operationalises hand dispatch via the comms protocol

---

## 1. Summary

The comms protocol gains a class of broker-side **actions**, addressed by chat messages whose content begins with `!<action-name>`. Actions are executed by the nexus broker on ingress; the chat content is consumed (not delivered to thread recipients) and the action handler does its work.

Epic A delivers one action — `!dispatch <context>` — which operationalises the existing hand-dispatch v0.1 spec (`docs/2026-04-30-hand-dispatch-v0_1.md`). When a registered aspect sends `!dispatch <payload>`, the broker spawns a fresh-context hand of that aspect via the existing `handqueue` package, identity-framed with the caller's NEXUS.md / SOUL.md / PRIMER. The hand processes the payload as its first turn input and posts the result back to the originating thread as a chat message.

Epic A also reserves a sibling surface for skill invocations (`/<skill-name>` in chat content) that Epic B will fill in, and reserves a discoverability action (`!skills`) backed by a stub registry today.

The two symbols are deliberately distinct:

- `!action` — **actions**: broker-side built-in operations
- `/skill` — **skill invocations**: load a skill's content into the recipient's boot context (Epic B)

This avoids name-overlap problems and lets the parser route by symbol without consulting registries.

## 2. Goals and non-goals

**Goals:**

- A registered aspect can fire `!dispatch <context>` from any harness (funnel, agora, raw CC + nexus-comms-mcp) and have a fresh-context hand of themselves run with that context.
- The hand boots with the dispatching aspect's identity framing (NEXUS.md / SOUL.md / PRIMER) — finishing the v0.1 deferred work in `runtime/handexec`.
- Results land back on the originating thread as chat messages, distinguishable from regular content via frame metadata.
- Multiple concurrent dispatches are supported per caller, subject to `handqueue`'s existing fairness scheduler.
- Failures (timeout, crash, malformed output) surface symmetrically — same chat-message channel, metadata flags the error.
- Discoverability is built in: `!actions` lists actions; `!skills` lists skills (stubbed in Epic A).
- The protocol works uniformly across aspect-aspect dispatch and operator-aspect dispatch (no separate operator API).

**Non-goals (Epic A):**

- Skill invocation surface (`/<skill-name>`) — Epic B.
- Skill registry implementation — Epic B (interface defined, null implementation shipped).
- `!cancel`, `!status`, `!result <id>` actions — future iterations.
- Hands' ability to fire `!dispatch` recursively — explicitly forbidden by capability removal in v0.1.
- Cross-host hand execution — handqueue v0.1 is single-host.
- Long-context dispatch via file references — defer to NEX-139 (file store) integration.
- Live identity-file edits affecting in-flight dispatches — current contract is "loaded at hand boot, immutable for that dispatch".

## 3. Protocol shape

### 3.1 Symbol disambiguation

When a `chat.send` frame arrives at the broker, the handler peeks the first byte of `content`:

| First byte | Path | Behaviour |
|---|---|---|
| `'!'` | action path | Parse action name, lookup in ActionRegistry, invoke handler with calling aspect's identity. Original content NOT delivered to thread recipients. |
| `'/'` | skill path | (Epic B) Parse skill name, resolve content, attach as metadata, forward to thread. Epic A passes through as plain text. |
| else | normal | Forward to thread as a normal chat message. |

The parser does no name lookup to disambiguate kinds — symbol alone determines the path. This eliminates the "what if a skill is named `dispatch`" class of bug.

### 3.2 `!dispatch` invocation flow

```
T+0
  Caller (any registered aspect, or operator via shadow's MCP)
  sends chat.send to broker:
    content = "!dispatch <payload>"
    thread  = <originating thread or operator-private channel>

  Broker recognises action path:
    a. Resolves calling aspect from authenticated wsConn.
    b. Action name = "dispatch", args = "<payload>".
    c. Looks up Action in registry, invokes Handle.
    d. DOES NOT forward the original chat content to thread recipients.

  DispatchAction.Handle:
    a. Validates calling aspect identity (already done by broker; no user-supplied identity trusted).
    b. Validates payload non-empty.
    c. Builds DispatchPayload {
         CallingAspect: <wsConn registered name>,
         Thread:        <originating thread root>,
         Payload:       <args>,
         SubmittedAt:   now(),
         DispatchID:    uuid_v4(),
       }
    d. handqueue.Submit(payload) — concurrent multi-dispatch supported.
    e. Returns response frame to caller: KindDispatchAccepted {
         DispatchID,
         QueuePosition,   // 0 if spawned immediately
       }

  Broker emits audit chat.send frame to thread:
    from     = <caller>,
    content  = "!dispatch <truncated args>",
    metadata = { kind: "dispatch_audit", dispatch_id, status: "submitted" }

T+~1s
  handqueue picks a slot (or queues per spec §2-3).
  When spawn fires, broker emits audit chat.send to thread:
    metadata = { kind: "dispatch_audit", dispatch_id, status: "running", worker_pid }

T+~30s
  handexec subprocess completes, captures stdout.

  Success path:
    Broker emits chat.send to thread:
      from     = "<caller>-hand-<short-id>",
      content  = <hand's stdout>,
      metadata = { kind: "dispatch_result", dispatch_id, status: "success", duration_ms }

  Failure path:
    Broker emits chat.send to thread:
      from     = "<caller>-hand-<short-id>",
      content  = "<reason / error summary>",
      metadata = { kind: "dispatch_error", dispatch_id, status: "failure",
                   exit_code, error_class }

  Caller's harness receives via chat.deliver, sees it as a normal inbox item,
  correlates via dispatch_id metadata, and proceeds with whatever logic the
  caller's turn-engine wants.
```

### 3.3 Concurrent multi-dispatch

A caller may fire any number of `!dispatch` calls. Each receives a unique `DispatchID` in the `KindDispatchAccepted` response. handqueue's fairness scheduler ensures no caller starves others, with soft cap N and hard ceiling H per existing spec.

The caller's harness keeps an in-process map `dispatch_id → {submitted_at, payload_summary, thread}` and matches each arriving result by metadata. Order of arrival is not guaranteed — dispatchers fan in.

The protocol does not include any "wait for all" primitive. The caller decides their own synthesis logic.

### 3.4 Audit chat frame metadata

All audit / result frames use the existing `chat.send` envelope with a `metadata` JSON sidecar (already supported by the broker spec). Schema:

```json
{
  "kind": "dispatch_audit" | "dispatch_result" | "dispatch_error",
  "dispatch_id": "<uuid>",
  "status": "submitted" | "running" | "success" | "failure",
  "duration_ms": <int>,
  "exit_code": <int>,
  "error_class": "timeout" | "crashed" | "malformed" | "identity_load_failed" | "spawn_failed",
  "worker_pid": <int>,
  "queue_position": <int>,
  "truncated": <bool>
}
```

Receivers that ignore metadata see a normal chat message and degrade gracefully. Activity-stream renderers (per `2026-05-17-agora-source-aware-render` / NEX-187) use the `kind` field to style audit lines as dim activity events vs full content.

## 4. handexec identity framing

`runtime/handexec/handexec.go` finishes the v0.1 deferred work from spec §2.1 — workers boot loaded with the dispatching aspect's identity framing.

### 4.1 Identity bundle loading

When the dispatcher invokes handexec, the worker:

1. **Resolves aspect home** from `<data-dir>/aspects/<calling-aspect>/` (canonical). Falls back to a broker-served path if the local copy is unavailable.

2. **Reads identity files in order:**
   - `<home>/NEXUS.md` — aspect role + working agreements (required)
   - `<home>/SOUL.md` — personality / disposition (optional)
   - `<home>/PRIMER.md` — always-loaded context (optional)

3. **Composes system prompt:**

   ```
   [NEXUS.md content]
   ---
   [SOUL.md content]              # if present
   ---
   [PRIMER.md content]            # if present
   ---
   You are running as a hand — a fresh-context instance of yourself
   dispatched to do a focused side task. Your reply lands back on
   thread <thread-id> as a chat message. You have one turn.
   ```

4. **Launches claude subprocess:**

   ```
   claude -p \
       --system-prompt-file=<temp-file> \
       (no --resume — fresh context per spec)
   stdin: <dispatch payload as first user message>
   ```

5. **Collects output:**

   - stdout → `DispatchResultPayload.Content`
   - stderr → log; on non-zero exit, surface as `dispatch_error` with `error_class=crashed`

### 4.2 Boundaries

The hand has no MCP tools, no WS connection, no aspect credentials beyond what the system prompt files already expose. Its only output channel is stdout, which the broker translates to a single chat.send frame on the originating thread.

This is enforced by capability removal — the handexec subprocess has no broker WS or MCP server to call. Recursion (hand fires `!dispatch`) is therefore impossible in v0.1, not by policy but by what's wired.

### 4.3 Missing-file behaviour

- Missing aspect home → dispatch rejected with `error_class=identity_load_failed`, audit emits failure to thread immediately, no spawn.
- Missing NEXUS.md → fail closed: same as missing home (role definition is mandatory).
- Missing SOUL.md or PRIMER.md → log warning, compose system prompt without them, proceed.
- I/O error reading any file → dispatch rejected with `error_class=identity_load_failed`.

### 4.4 Drive freshness caveat

Identity files live canonically on the operator's Drive folder (`Drive/nexus/<aspect>/`) with local copies in aspect homes. v0.1 trusts the local copy as-is — no freshness check, no pull-from-broker. If Drive sync is lagging, hands may boot with stale identity. Future enhancement: timestamp comparison or broker-mediated refresh.

## 5. Action registry

### 5.1 Interface

```go
// Action handles a comms-protocol action invocation.
type Action interface {
    // Name is the registry key, without the leading "!".
    Name() string

    // Description is a one-line summary for the !actions list output.
    Description() string

    // Help is multi-line detail for !actions <name> output.
    Help() string

    // Handle processes the action. envelope carries the calling
    // aspect's connection context (identity, thread). args is the
    // raw rest-of-content after "!<name> ". Returns the response
    // frame the broker sends back to the caller.
    Handle(ctx context.Context, envelope frames.Envelope, args string) (frames.Envelope, error)
}
```

### 5.2 Registry

```go
type ActionRegistry struct {
    mu      sync.RWMutex
    actions map[string]Action
}

// Register adds an action. Panics on duplicate name (programmer error,
// not a runtime concern — registrations happen at startup only).
func (r *ActionRegistry) Register(a Action)

// Lookup returns the Action for a name, plus bool indicating presence.
func (r *ActionRegistry) Lookup(name string) (Action, bool)

// List returns descriptors for all registered actions, sorted by name,
// for !actions discovery.
func (r *ActionRegistry) List() []ActionDescriptor

type ActionDescriptor struct {
    Name        string
    Description string
}
```

### 5.3 Wiring

Registry is constructed and populated at broker startup. v0.1 registrations:

```go
broker.actions = NewActionRegistry()
broker.actions.Register(NewDispatchAction(handqueue, identityLoader))
broker.actions.Register(NewListActionsAction(broker.actions))
broker.actions.Register(NewListSkillsAction(skillRegistry))   // skillRegistry = NullSkillRegistry in Epic A
```

All handlers are compiled into the broker binary. No external registration mechanism in v0.1.

### 5.4 Parser entrypoint

```go
// parseLeadingAction inspects content for a leading !action-name.
// Returns name, rest-of-content, and true if an action was detected.
func parseLeadingAction(content string) (name, args string, isAction bool) {
    if len(content) == 0 || content[0] != '!' {
        return "", "", false
    }
    rest := content[1:]
    idx := strings.IndexByte(rest, ' ')
    if idx < 0 {
        return rest, "", true   // !action with no args
    }
    return rest[:idx], rest[idx+1:], true
}
```

Invoked from the broker's existing `chat.send` handler before the normal forward-to-thread path.

### 5.5 Unknown action

If the parser detects `!<name>` but the registry has no matching handler, broker responds to caller with `KindDispatchError`:

```json
{
  "kind": "dispatch.error",
  "error_class": "unknown_action",
  "name": "<offending-name>",
  "available_actions": ["dispatch", "actions", "skills"]
}
```

The original chat content is not forwarded to the thread. Caller sees the error inline.

## 6. Discoverability — `!actions` and `!skills`

### 6.1 `!actions`

Lists all registered actions. Optional argument shows detail on one.

```
$ !actions

Available actions:
  !dispatch <context>       Spawn a fresh-context hand of yourself, run with context
  !actions [name]           List actions, or detail on one
  !skills [name]            List skills, or detail on one (Epic B — placeholder)

Use `!actions <name>` for detail. Use `!skills` for skill discovery.

$ !actions dispatch

!dispatch <context>

Spawn a fresh-context hand of yourself with the given context.
The hand runs as you (same identity, same NEXUS.md/SOUL.md/PRIMER),
processes the context as its first turn input, returns the result
on this thread as a chat message.

Concurrent multi-dispatch supported — fire several !dispatch calls,
each returns when complete with a unique dispatch_id. Reply correlates
back via the dispatch_id in the chat frame's metadata.

Identity is self-only — you cannot dispatch as another aspect.

See: docs/2026-04-30-hand-dispatch-v0_1.md
```

### 6.2 `!skills`

Epic A ships a stub:

```
$ !skills

Skill registry not yet implemented (Epic B).
This action will list shared network skills once the registry ships.
For now, skills exist only in the local CC session via claude-code's
/<skill> mechanism.
```

When Epic B fills in `SkillRegistry`, the same action produces the real listing without code change.

### 6.3 Skill on-disk layout (locked here, used by Epic B)

Skills follow the claude-code convention:

```
<aspect-home>/.nexus/skills/<skill-name>/
    SKILL.md                          # frontmatter + body
    <supporting-files>.md             # referenced by SKILL.md by relative path
```

Frontmatter carries `name`, `description`, optional metadata:

```yaml
---
name: subagent-driven-development
description: Execute plans by dispatching fresh hands per task, with two-stage review
metadata:
  type: workflow
---
```

Drive-canonical copies live at `Drive/nexus/<aspect>/.nexus/skills/` and sync to local aspect homes. Existing claude skills port verbatim — no conversion.

### 6.4 Skill registry interface

```go
type SkillRegistry interface {
    List() []SkillDescriptor
    Get(name string) (SkillDescriptor, bool)
}

type SkillDescriptor struct {
    Name        string
    Description string
    Content     string   // populated lazily or eagerly per Epic B design
}
```

Epic A ships:

```go
type NullSkillRegistry struct{}
func (NullSkillRegistry) List() []SkillDescriptor { return nil }
func (NullSkillRegistry) Get(string) (SkillDescriptor, bool) { return SkillDescriptor{}, false }
```

The `!skills` action consumes the interface. Epic B replaces the implementation; the action's code does not change.

### 6.5 Response delivery

`!actions` and `!skills` responses are addressed only to the caller, not the thread. Discovery is metadata, not conversation. Broker uses existing single-recipient `chat.send` routing to deliver the listing to the calling aspect.

### 6.6 NEXUS.md addition

The canonical NEXUS.md template (on Drive) gets a "Comms protocol actions" section listing registered actions and the `!` / `/` symbol convention. Aspects load NEXUS.md as part of their boot, so they know `!dispatch` and `!actions` exist.

Template addition:

```markdown
## Comms protocol actions

Chat content beginning with `!<name>` triggers a broker-side action.
Currently registered:

- `!dispatch <context>` — spawn a fresh-context hand of yourself, run with context
- `!actions [name]` — discover available actions
- `!skills [name]` — discover available skills

Use `!actions` to list, `!actions <name>` for detail.

Chat content beginning with `/<name>` invokes a skill (recipient loads
the skill's content into their context for that turn). Skill registry
ships in Epic B.
```

## 7. Security, limits, and edge cases

### 7.1 Identity binding

Calling aspect identity is read from the authenticated `wsConn` registered name. The action handler does not trust any user-supplied identity in the payload. Hands always boot as the caller — no `-as` parameter, no impersonation.

### 7.2 Payload size

Inherits chat-message size limits already enforced by the broker. `!dispatch` payload is the chat content minus the leading `!dispatch ` token; oversized content is rejected at chat-frame ingress with the existing `chat_too_large` error before the action handler runs.

Long-context dispatch via `nexus://` file references (NEX-139 file store) is deferred to a future enhancement.

### 7.3 Concurrent dispatch

handqueue already enforces soft cap `N` and hard ceiling `H` per hand-dispatch v0.1 §2.1. Callers firing many `!dispatch` calls have some queued (FIFO with fairness invariant). Caller's harness sees `KindDispatchAccepted` with `queue_position > 0` for queued ones. No new limit added by Epic A.

### 7.4 Recursion prevention

Hands cannot fire `!dispatch` recursively in v0.1. The handexec subprocess has no MCP, no WS connection, no access to the broker's action surface. The hand's only output channel is stdout, which becomes a single chat message on thread. Recursion is impossible by capability, not policy.

Future recursion (Epic B+) requires explicit design: depth limits, identity tracking, billing attribution.

### 7.5 Edge cases

| Case | Behaviour |
|---|---|
| `!dispatch` with empty args | Reject with `error_class=empty_payload`. No spawn. |
| `!dispatch` from unregistered connection | Reject with `error_class=caller_not_registered`. |
| Aspect home missing | Reject with `error_class=identity_load_failed`. |
| handqueue at hard ceiling | Reject with `error_class=hard_ceiling_reached`. |
| Hand subprocess fails to start | Audit emits running→failure with `error_class=spawn_failed`. |
| Hand exits with no stdout | Result chat frame with empty content, `status=success`, `duration_ms` set. Caller may treat empty as soft failure. |
| Hand stdout exceeds chat size limit | Truncate to limit; result metadata includes `truncated=true`. Full output retrieval is future work. |
| Caller WS disconnects mid-dispatch | Hand continues. Result still emitted to thread. Mailbox-on-broker or outpost replays to caller on reconnect. |
| Identity files modified mid-dispatch | Hand uses snapshot loaded at boot; in-flight dispatches unaffected. Future enhancement: configurable refresh policy. |

### 7.6 Out of scope (explicit)

- `!cancel <dispatch_id>` action — deferred.
- `!status <dispatch_id>` action — deferred.
- `!result <dispatch_id>` for full-output retrieval after truncation — deferred.
- Drive-synced identity-file freshness check — deferred.
- Cross-host hand execution — handqueue v0.1 single-host.
- Hand → broker recursive comms — explicitly disabled in v0.1.
- Live identity-bundle edits affecting in-flight dispatches — current contract is "loaded at hand boot, immutable for that dispatch".

## 8. Build sequence

Phase ordering (decomposed by writing-plans):

1. **Action registry primitive** — `broker/action.go`: `Action` interface, `ActionRegistry`, parser entry. Tests cover parser edge cases (no `!`, empty action name, args with spaces, unknown action).

2. **`!actions` action** — `broker/action_listactions.go`. Trivial; consumes `ActionRegistry.List()`. Tests cover empty registry, single action, multi-action, single-recipient response delivery.

3. **Skill registry interface + null implementation** — `broker/skillregistry.go`: `SkillRegistry`, `SkillDescriptor`, `NullSkillRegistry`. No actual scanning. Lays foundation for Epic B.

4. **`!skills` action** — `broker/action_listskills.go`. Consumes `SkillRegistry`. Tests cover null-registry (Epic A stub message) and a fake non-null registry.

5. **handexec identity framing** — finish v0.1 deferred work in `runtime/handexec/handexec.go`: load NEXUS.md / SOUL.md / PRIMER, compose system prompt, pass payload as first user message. Tests cover missing files, success path, malformed identity.

6. **`!dispatch` action** — `broker/action_dispatch.go`. Validates caller, builds payload, calls `handqueue.Submit`, returns `KindDispatchAccepted`. Tests cover happy path, empty payload, unregistered caller, missing aspect home, ceiling rejection.

7. **Audit chat frame emission** — broker emits `dispatch_audit`, `dispatch_result`, `dispatch_error` frames to thread at submitted / running / completed transitions. Tests cover metadata correctness, single-recipient vs thread-wide addressing.

8. **`chat.send` handler integration** — wire `parseLeadingAction` into the existing `chat.send` ingress path. Tests cover plain chat (forwarded), `!`-prefixed (action dispatched, not forwarded), `/`-prefixed (Epic A: forwarded as-is, treated as plain text).

9. **NEXUS.md template update on Drive** — add "Comms protocol actions" section. Deployed by syncing the template file; no code change.

10. **Skill rewrite: `subagent-driven-development`** — replace claude-code Task subagent calls with `!dispatch /role-skill ...` composition pattern. Skill file changes only. Document the new flow.

Phases 1-4 are independent and can parallelise. Phase 5 (identity framing) blocks Phase 6 (`!dispatch`). Phase 7 ties to 6. Phase 8 integrates the whole pipeline. Phases 9-10 are documentation/skill rewrites with no code dependency on 1-8 but should land after the broker is functional so the documented flow is actually exercisable.

## 9. v0.1 acceptance criteria

- Registered aspect sends `!dispatch <payload>` and receives `KindDispatchAccepted` with a unique `dispatch_id`.
- Broker does not forward the original `!dispatch ...` chat content to thread recipients.
- Broker emits `dispatch_audit` chat frame to thread with `kind=dispatch_audit, status=submitted`.
- handqueue spawns a worker subprocess (handexec) that boots loaded with the calling aspect's NEXUS.md / SOUL.md / PRIMER as system prompt.
- Worker processes the payload as first user message and writes result to stdout.
- Broker captures stdout, emits `dispatch_result` chat frame to thread with `kind=dispatch_result, status=success`.
- Worker crash / non-zero exit produces `dispatch_error` chat frame with `error_class=crashed` and exit code in metadata.
- Multiple concurrent `!dispatch` calls from one caller each receive distinct `dispatch_id`s; results return independently; caller's harness can correlate by metadata.
- `!actions` (no args) lists registered actions with one-line descriptions, addressed only to the caller (not thread).
- `!actions dispatch` returns multi-line detail on the dispatch action.
- `!skills` returns the Epic A stub message (null registry).
- Unknown action (`!nonexistent`) produces `KindDispatchError` with `error_class=unknown_action` and a list of available actions.
- Empty-payload dispatch (`!dispatch`) rejected with `error_class=empty_payload`.
- Hand cannot fire `!dispatch` recursively (verified by capability removal — no MCP, no WS).
- All tests in `broker/action_*_test.go` and `runtime/handexec/handexec_test.go` green on all three platforms (ubuntu, macos, windows).

## 10. Open questions

- **Drive sync freshness for identity files.** v0.1 trusts the local copy. Should there be a TTL or operator-pull mechanism before the first Epic B dispatch tests? Probably not — manual `git pull` or Drive sync suffices for now. Revisit if stale-identity bugs surface.
- **Truncated result retrieval.** v0.1 truncates oversized hand stdout with a `truncated=true` flag. Future `!result <dispatch_id>` action would let caller pull full output. Defer until a real use case demands it (Epic A's dispatches are intended to be short).
- **Audit-frame noise.** Every dispatch produces at least 3 chat frames (submitted, running, completed). For a thread with heavy dispatch activity, the audit stream may dominate the chat log. Mitigated by activity-stream renderers (NEX-187) styling them dim and collapsible. If still noisy, future enhancement: audit-frame batching or per-thread audit-channel separation.

## 11. Dependencies

- **`nexus/handqueue` package** — exists, fully implemented per hand-dispatch v0.1 §2-3. No changes.
- **`runtime/handexec` package** — exists; identity-framing finish lands in Phase 5.
- **`nexus/frames` package** — extend with `KindDispatchAccepted`, refine `KindDispatchError` if needed. Existing `KindDispatch` / `KindDispatchResult` repurposed or kept for internal broker→handqueue flow.
- **`nexus/broker/chat.go` (chat.send handler)** — Phase 8 integration point.
- **No new external dependencies.**

## 12. Status

v0.1 draft. Brainstormed 2026-05-17 with operator. Pending implementation plan via writing-plans.
