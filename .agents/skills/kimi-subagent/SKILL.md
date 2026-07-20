---
name: kimi-subagent
description: Kimi-k3 as a subagent via LiteLLM, two modes (operator decrees 2026-07-19). BUILD (default): Claude specs and plans, kimi implements — bounded code units, GDScript/shader work, mechanical multi-file changes. DESIGN: kimi designs visual/layout systems and gives frame SHIP/ITERATE verdicts, Claude implements. Trigger when delegating a spec'd implementation unit, on a visual-design problem, or when another skill needs a multimodal design review. (Merely SEEING an image → vision-review.)
when_to_use: 'Delegate work to kimi-k3: Claude specs and kimi implements (BUILD, default), or kimi designs visual/layout systems and judges frames (DESIGN).'
---

# Kimi subagent (kimi-k3 via LiteLLM)

Kimi has **no tools and no filesystem — Claude is its hands.** Everything kimi needs goes in
the prompt; everything kimi produces, Claude applies, gates, and verifies. Exploration,
architecture, and final judgment stay with Claude (same split rule as the orchestrator skill).
Why kimi: operator decree + sovereignty — the work runs on the owned gateway. History:
memory `feedback-kimi-design-subagent`, `project-river-barrier`.

## BUILD mode — Claude specs, kimi implements (default)

**Harness path (preferred): headless Claude Code running ON kimi.** LiteLLM exposes the
Anthropic `/v1/messages` route, so the Claude Code harness itself can run with kimi-k3 as
the model — kimi gets Read/Edit/Write (and the agent loop) and edits the worktree itself:

```bash
cd <worktree> && env ANTHROPIC_BASE_URL=http://litellm.model-stack.svc.cluster.local:4000 \
  ANTHROPIC_API_KEY=litellm-local \
  claude -p "<orchestrator-style spec: goal, files, conventions, acceptance criteria>" \
  --model kimi-k3 --max-turns 16 --permission-mode acceptEdits \
  --strict-mcp-config --mcp-config '{"mcpServers":{}}'
```

- `--strict-mcp-config --mcp-config '{"mcpServers":{}}'` is **required** — the Meshy MCP
  tool schemas use `$ref`, which Moonshot's validator rejects (400 on every request).
- `--permission-mode bypassPermissions` is classifier-blocked from a nested session; stay
  on `acceptEdits`. Kimi therefore edits but does not run commands — Claude runs the gate.
- Kimi reasons heavily per turn: run real units with `run_in_background` and a generous
  timeout. Spec discipline is the same as the API path below (kimi still can't ask back).
- After the run: Claude gates and adversarially reviews the diff exactly as steps 4-5 below.
- Honesty note: kimi-k3 routes gateway → OpenRouter → Moonshot — owned *gateway*, remote
  *model*. Smoke-proven 2026-07-20 (file task, DONE, verified on disk).

**API path (fallback — harness unavailable, or a one-shot authoring call is enough):**

1. **Spec** the unit like an orchestrator builder ticket: goal, constraints and conventions
   (for Carried World GDScript always include: tabs; never `:=` on a Variant right-hand
   side; match surrounding idiom), acceptance criteria, and the **full current text of every
   region kimi may touch** (whole file if small, generous excerpts if large — kimi cannot
   look anything up). Done when kimi could implement without asking anything back.
2. **Request edits in strict SEARCH/REPLACE blocks** — require this exact shape, one block
   per change, file path line before each block, SEARCH text copied **verbatim** from the
   provided source:

   ```
   FILE: client/foo.gd
   <<<<<<< SEARCH
   <exact existing lines>
   =======
   <replacement lines>
   >>>>>>> REPLACE
   ```
3. **Apply mechanically** — parse the blocks and apply via the blessed python-heredoc
   pattern: `assert s.count(old) == 1` per block, build the full string, write once. A
   SEARCH that doesn't match exactly is **rejected back to kimi** with the real text — never
   hand-fix kimi's block and never fuzzy-match. Done when every block applied or bounced.
4. **Gate** — headless parse/run on dMon (or the unit's own acceptance check). Failures go
   back to kimi verbatim (error + current excerpt), max **3 fix rounds**, then Claude takes
   over the remainder. Done when the gate is green or Claude has taken over.
5. **Verify adversarially** — Claude reads the final diff as reviewer (never trust-merge),
   then commit/deploy per the normal flow. Visual work still ends look-first (Law 4).

## DESIGN mode — kimi designs, Claude implements

1. **Gather** — code excerpts, hard constraints (engine, perf, canon), downscaled frames.
   Done when kimi could act on the packet without asking anything back.
2. **Design request** — ask for an implementable spec: numbered prescriptions, exact values,
   shader-level where relevant. Done when the reply is parameter-level (not vibes).
3. **Implement faithfully — then calibrate against measured reality.** Kimi designs from
   frames, never from the live sim's numbers. When a prescription doesn't land, don't
   re-prescribe by taste: **bisect** (flat-color A/B to prove pixel ownership, value
   heatmaps, disable terms one at a time) and report the delta to kimi as measured fact.
   Done when every prescription is implemented verbatim or replaced by a bisect-proven
   calibration with evidence noted.
4. **Re-frame the SAME angles** kimi judged before — new angles don't close old findings.
5. **Verdict** — frames + implemented/deviated list, ask SHIP or ITERATE (one line per
   frame). ITERATE → step 3. Ends only on SHIP or operator overrule; kimi's SHIP never
   replaces the operator's eye.

## Call mechanics (all hard-won — do not rediscover)

- Endpoint `http://litellm.model-stack.svc.cluster.local:4000/v1/chat/completions`, model
  `kimi-k3`, **stream: true always** (non-streaming emits nothing).
- **max_tokens 32000 for any prompt with images or code generation** — kimi reasons 30-50k
  chars before its first content token; a 12k cap died with zero content. Short text-only
  follow-ups: 20000.
- Parse `delta.reasoning_content` and `delta.content` **separately**; content-only parsing
  of a reasoning-heavy reply looks like an empty response. `finish_reason: "length"` with
  no content = raise the cap and resend.
- Images: JPEG ~1400px wide as base64 data-URIs. On croft only ffmpeg exists:
  `ffmpeg -y -i in.png -vf scale=1400:-1 -q:v 4 out.jpg`.
- Multi-turn: replay prior kimi output as an `assistant` message — kimi holds no state.
- Truncation recovery (curl `--max-time` cut the stream): resend with the partial as the
  assistant turn + user: `Continue EXACTLY from '<last words>'` — it resumes cleanly.

## Canonical call

```bash
SP=<scratchpad>; python3 - <<'PYEOF'
import base64, json
SP = "<scratchpad>"
def b64(p): return base64.b64encode(open(p, "rb").read()).decode()
content = [{"type": "text", "text": """<prompt>"""}]
# DESIGN mode frames (omit in BUILD mode):
# content.append({"type": "image_url", "image_url": {"url": "data:image/jpeg;base64," + b64(f"{SP}/frame1.jpg")}})
json.dump({"model": "kimi-k3", "stream": True, "max_tokens": 32000,
           "messages": [{"role": "user", "content": content}]},
          open(f"{SP}/kimi_req.json", "w"))
PYEOF
curl -sN --max-time 590 -X POST http://litellm.model-stack.svc.cluster.local:4000/v1/chat/completions \
  -H "Content-Type: application/json" -d @$SP/kimi_req.json > $SP/kimi_raw.txt; echo "exit=$?"
python3 - <<'PYEOF'
import json
SP = "<scratchpad>"
content, reasoning, fin = [], [], None
for line in open(f"{SP}/kimi_raw.txt"):
    line = line.strip()
    if not line.startswith("data: ") or line == "data: [DONE]": continue
    try: d = json.loads(line[6:])
    except Exception: continue
    ch = d.get("choices", [{}])[0]
    delta = ch.get("delta", {})
    if delta.get("content"): content.append(delta["content"])
    if delta.get("reasoning_content"): reasoning.append(delta["reasoning_content"])
    if ch.get("finish_reason"): fin = ch["finish_reason"]
c = "".join(content)
open(f"{SP}/kimi_out.md", "w").write(c)
print(f"content={len(c)} reasoning={len(''.join(reasoning))} finish={fin}")
PYEOF
```

## Applying BUILD-mode blocks

```bash
python3 - <<'PYEOF'
import re
SP = "<scratchpad>"
out = open(f"{SP}/kimi_out.md").read()
blocks = re.findall(r"FILE:\s*(\S+)\s*\n<{7} SEARCH\n(.*?)\n={7}\n(.*?)\n>{7} REPLACE", out, re.S)
assert blocks, "no SEARCH/REPLACE blocks found"
by_file = {}
for path, old, new in blocks:
    by_file.setdefault(path, []).append((old, new))
for path, edits in by_file.items():
    s = open(f"<worktree>/{path}", encoding="utf-8").read()
    for old, new in edits:
        assert s.count(old) == 1, f"REJECT {path}: SEARCH not unique/found: {old[:60]!r}"
        s = s.replace(old, new, 1)
    assert isinstance(s, str)
    open(f"<worktree>/{path}", "w", encoding="utf-8").write(s)
    print(f"applied {len(edits)} edit(s) -> {path}")
PYEOF
```
