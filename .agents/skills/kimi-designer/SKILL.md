---
name: kimi-designer
description: Kimi-k3 as the DESIGNING subagent for visual/layout problems (operator decree 2026-07-19) — river/terrain looks, settlement layout, shading design, any "how should this read" question. Kimi designs and gives frame verdicts; Claude implements. Trigger on a visual-design problem, on frames needing a design-level SHIP/ITERATE verdict, or when another skill needs a multimodal design review. (Merely SEEING an image → vision-review.)
when_to_use: 'Visual/layout design problems and frame verdicts in Carried World or similar: kimi designs, Claude implements, loop until SHIP.'
---

# Kimi designer loop (kimi-k3 via LiteLLM)

Operator decree: for visual and layout problems, kimi-k3 is the **designer**, not just a
reviewer — Claude is the coding agent. Why + history: memory `feedback-kimi-design-subagent`
and `project-river-barrier` (the full river loop that proved the pattern).

## The loop

1. **Gather** — code excerpts of the systems in question, hard constraints (engine, perf,
   canon), and frames (downscaled, see mechanics). Done when kimi could act on the packet
   without asking anything back.
2. **Design request** — ask for an implementable spec: numbered prescriptions, exact values,
   shader-level where relevant. Done when the reply contains parameter-level prescriptions
   (not vibes); if it cut off mid-answer, run truncation recovery before proceeding.
3. **Implement faithfully — then calibrate against measured reality.** Kimi designs from
   frames; it has never seen the live sim's numbers. When a prescription doesn't land,
   don't re-prescribe by taste: **bisect** (flat-color A/B to prove pixel ownership, value
   heatmaps, disable terms one at a time), fix the calibration, and report the delta to
   kimi as measured fact, not opinion. Done when every prescription is either implemented
   verbatim or replaced by a bisect-proven calibration with the evidence noted.
4. **Re-frame the SAME angles** kimi judged before — verdicts on new angles don't close old
   findings. Done when each previously-failing view has a fresh frame.
5. **Verdict** — send frames + the implemented/deviated list, ask SHIP or ITERATE (one line
   of reasoning per frame max). ITERATE → back to 3. The loop ends only on **SHIP** or the
   operator overruling. Law 4 still applies: kimi's SHIP does not replace the operator's eye.

## Call mechanics (all hard-won — do not rediscover)

- Endpoint `http://litellm.model-stack.svc.cluster.local:4000/v1/chat/completions`, model
  `kimi-k3`, **stream: true always** (non-streaming emits nothing).
- **max_tokens 32000 for any prompt with images** — kimi reasons 30-50k chars before its
  first content token; a 12k cap died with zero content. Text-only follow-ups: 20000.
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
msgs = [{"role": "user", "content": [
    {"type": "text", "text": """<prompt>"""},
    {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64," + b64(f"{SP}/frame1.jpg")}},
]}]
json.dump({"model": "kimi-k3", "stream": True, "max_tokens": 32000, "messages": msgs},
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
