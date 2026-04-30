# §6.5 P9 — Manual e2e checklist

Operator-driven smoke for the live Frame setup → embedding → admin →
deliberation → shutdown path. The Go test in `frame_smoke_test.go`
covers bootstrap → setup → folder render automatically; this checklist
covers everything that needs a live LLM, real process restart, or
human-in-the-loop verification.

Run on a fresh checkout. ~10 minutes start to finish.

## Prerequisites

- `ANTHROPIC_API_KEY` in env (or set up claude-code-headless and pick
  `claudecode` as the provider in step 4).
- A clean working directory: `mkdir /tmp/nexus-smoke && cd /tmp/nexus-smoke`.
- Build the nexus binary: `go build -o nexus ./nexus/cmd/nexus`.

## 1. Bootstrap mode reachable

```bash
mkdir agents
NEXUS_TOKEN=test-token NEXUS_ASPECT_DIR=$PWD/agents \
  ./nexus -addr :7888
```

Expected:
- Log line: `frame: bootstrap mode — no Frame personality found`.
- Log line: `frame: bootstrap mode listening addr=:7888 agents_dir=...`.

In a browser: `http://localhost:7888/`. Should render the wizard SPA
(dark monospace UI, "Nexus first boot" header, form fields).

## 2. Wizard form submits

In the browser:
- Name: `keel`
- Voice: leave empty (default applies)
- Values: leave empty (default applies)
- Provider: claude-api
- Model: leave empty (default applies)
- Click "Create Frame".

Expected:
- Success panel appears with the home path.
- The Nexus process exits with code 64.

Check the result:
```bash
ls agents/keel/
# expect: aspect.json  CLAUDE.md  PRIMER.md  SOUL.md
cat agents/keel/aspect.json | jq .role  # "frame"
cat agents/keel/SOUL.md | head -20  # voice + values defaults applied
```

## 3. Restart in normal mode → frame embedded

```bash
NEXUS_TOKEN=test-token NEXUS_ASPECT_DIR=$PWD/agents \
  ./nexus -addr :7888
```

Expected:
- Log line: `frame: detected name=keel path=...`.
- Log line: `frame: embedded as in-process aspect name=keel ...`.
- Log line: `broker listening addr=:7888`.

Verify the roster:
```bash
curl -s -H "Authorization: Bearer test-token" \
  http://localhost:7888/api/aspects | jq '.aspects[] | select(.name=="keel")'
# Frame should appear with capabilities including "admin".
```

## 4. Admin REST surface gated correctly

Non-admin token (this test rig only has the master token; simulate
non-admin by using a clearly-invalid bearer):

```bash
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer invalid-token" \
  http://localhost:7888/api/admin/dispatch-status
# expect 401
```

Admin token:
```bash
curl -s -H "Authorization: Bearer test-token" \
  http://localhost:7888/api/admin/dispatch-status | jq
# expect: { active_workers, soft_cap, hard_ceiling, queue_depth, busy_aspects }
```

## 5. Admin shutdown gracefully terminates

```bash
curl -s -X POST -H "Authorization: Bearer test-token" \
  http://localhost:7888/api/admin/shutdown | jq
# expect: 202 with op_id
```

The Nexus process should log `frame: admin shutdown requested` and
exit cleanly. Run again to confirm a fresh boot picks up the existing
frame from §3 without re-bootstrapping.

## 6. Deliberation loop (live, costs tokens)

This is the funnel + bridle integration smoke. Runs one real Claude
turn against the API. **Skip this if you're tight on credits — the
funnel package's unit tests cover the same paths against a fake
provider.**

Wire keel up to receive a comm and produce a response — TBD by the
operator since chat-bus integration into the Nexus broker is post-§6.5.
For now this checklist marker exists so the next person knows to
exercise the funnel against a live model when chat is wired in.

## 7. Compaction trigger (long-running, optional)

To exercise the compaction path, set a low threshold and run enough
turns to cross it:

```bash
NEXUS_TOKEN=... \
  FRAME_COMPACT_THRESHOLD=2000 \
  ./nexus -addr :7888
```

(The threshold is currently a Go constant; this env override lands
when context_threshold makes it onto aspect.json — see #102 follow-up.)

## Pass/fail criteria

✅ All of §1–§5 pass.
⚠️ §6 / §7 are optional credits-permitting checks — flag if either fails
   but block ship only on §1–§5.

## After the manual run

- Tear down: `pkill nexus; rm -rf /tmp/nexus-smoke`.
- File any failures as numbered tasks pointing at the failing step.
