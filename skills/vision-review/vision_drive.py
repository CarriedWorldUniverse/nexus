#!/usr/bin/env python3
"""qwen AUTOPILOT — qwen3.6 drives the Carried World to validate/tune it without a human at the display.

Perception->action loop: grab the current frame + HUD -> qwen calls the `step` nav tool (observe + move)
-> write the command to the running game's /tmp/cw_cmd -> wait for /tmp/cw_done -> pull the new frame ->
repeat. qwen is the vision engine (it identifies what it sees); this orchestrates and logs.

Requires the game running on dMon in --drive mode (persistent, polls /tmp/cw_cmd). Render seat must be
live (logged-in graphical session). Each step ~1.4k image tokens; a sliding window of frames keeps the
256k context bounded over long runs.

Usage:
  vision_drive.py [--steps N] [--goal "..."] [--start "goto HUB"]
Env: OPENAI_BASE_URL, OPENAI_MODEL, CW_DMON_HOST
Writes a markdown report to /tmp/cw_drive_report.md.
"""
import argparse, base64, json, os, subprocess, sys, time, urllib.request

BASE = os.environ.get("OPENAI_BASE_URL", "http://robo-dog:30803/v1")
MODEL = os.environ.get("OPENAI_MODEL", "gemma-4-12b")
DMON = os.environ.get("CW_DMON_HOST", "jacinta@100.91.185.71")
KEEP_FRAMES = 8                       # sliding window: keep only the last N frames as images in context

TOOLS = [{
    "type": "function",
    "function": {
        "name": "step",
        "description": "Record what you see in the CURRENT frame, then choose ONE navigation action to take next.",
        "parameters": {
            "type": "object",
            "properties": {
                "observation": {"type": "string", "description":
                    "What is in the current frame: terrain/relief, settlements & buildings (and their state), "
                    "the native Bush (manuka scrub, tree-ferns, podocarps), water, roads. Call out anything that "
                    "looks WRONG or worth tuning (gaps, floating geometry, missing features, bad colour/scale)."},
                "action": {"type": "string", "enum":
                    ["forward", "back", "left", "right", "up", "down", "face", "goto", "look", "stay", "done"],
                    "description": "Next move. Reposition with forward/back/left/right (STRAFE relative to your facing) "
                    "and up/down. `face` aims the camera at the nearest settlement's INN — your main way to look at a "
                    "settlement; use it whenever buildings drift out of frame. goto=jump to an overview above a named "
                    "settlement; look=down/level/up tilt; stay=hold and re-observe; done=finished."},
                "value": {"type": "number", "description":
                    "Distance in metres for forward/back/left/right/up/down. Ignored for face/goto/look."},
                "target": {"type": "string", "enum": ["RIVER", "HUB", "MINE", "down", "level", "up"],
                    "description": "For goto: which settlement. For look: down/level/up."}
            },
            "required": ["observation", "action"]
        }
    }
}]

DEFAULT_VAL = {"forward": 50, "back": 50, "left": 40, "right": 40, "up": 30, "down": 30, "setalt": 70}

def sh(cmd, timeout=30):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)

def translate(action, value, target):
    """qwen action -> a /tmp/cw_cmd line (or [] for a pure re-observe)."""
    if action in ("stay", "done"):
        return []
    if action == "face":
        return ["face"]
    if action == "look":
        t = target
        if t not in ("down", "level", "up"):          # qwen often passes an angle in `value` instead
            v = value or 0
            t = "down" if v < 0 else ("up" if v > 0 else "level")
        return ["look %s" % t]
    if action == "goto":
        return ["goto %s" % (target or "HUB")]
    v = value if (value is not None and value != 0) else DEFAULT_VAL.get(action, 40)
    return ["%s %g" % (action, v)]

def capture(seq, cmds, timeout=40):
    """Send a command batch to the running game; wait for cw_done==seq; pull the frame + HUD meta."""
    body = "%d\n%s" % (seq, "\n".join(cmds))
    open("/tmp/_cw_cmd", "w").write(body)
    sh(f"scp -q /tmp/_cw_cmd {DMON}:/tmp/cw_cmd")
    deadline = time.time() + timeout
    while time.time() < deadline:
        r = sh(f"ssh {DMON} 'cat /tmp/cw_done 2>/dev/null'")
        if r.stdout.strip() == str(seq):
            break
        time.sleep(0.6)
    else:
        sys.exit(f"[drive] timeout waiting for the game (seq {seq}). Is it running with --drive and the "
                 f"render seat live?")
    sh(f"scp -q {DMON}:/tmp/cw_nav.png /tmp/cw_nav.png 2>/dev/null")
    meta = sh(f"ssh {DMON} 'cat /tmp/cw_nav.txt 2>/dev/null'").stdout.strip()
    return "/tmp/cw_nav.png", meta

def img_part(path):
    b64 = base64.b64encode(open(path, "rb").read()).decode()
    return {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{b64}"}}

def qwen(messages):
    body = {"model": MODEL, "max_tokens": 2200, "temperature": 0.3,
            "messages": messages, "tools": TOOLS,
            "tool_choice": {"type": "function", "function": {"name": "step"}}}
    req = urllib.request.Request(BASE + "/chat/completions", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer EMPTY"})
    try:
        d = json.load(urllib.request.urlopen(req, timeout=180))
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode()[:500]}")
    return d["choices"][0]["message"]

def prune(messages):
    """Keep the system + last KEEP_FRAMES user-image messages; collapse older images to their text."""
    imgs = [i for i, m in enumerate(messages) if m["role"] == "user" and isinstance(m["content"], list)]
    for i in imgs[:-KEEP_FRAMES]:
        messages[i]["content"] = [c for c in messages[i]["content"] if c.get("type") == "text"]

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--steps", type=int, default=24)
    ap.add_argument("--goal", default="Survey the world end to end: visit each settlement, the Bush, the "
                    "water and the roads between them. Confirm it looks right and flag anything off.")
    ap.add_argument("--start", default="goto HUB")    # begin from a clean overview vantage
    ap.add_argument("--tour", default="", help="comma-list of settlements to force-visit in order, e.g. RIVER,HUB,MINE")
    ap.add_argument("--dwell", type=int, default=2, help="qwen inspection steps at each settlement before auto-advancing")
    args = ap.parse_args()
    tour = [t.strip().upper() for t in args.tour.split(",") if t.strip()]

    sys_prompt = (
        "You are an autonomous tester driving a camera through the voxel game world 'Carried World' "
        "(Aotearoa-inspired: river/hub/mine settlements, native Bush, water, dirt roads) to VALIDATE and "
        "help TUNE it — there is no human watching the screen. Each turn you receive the current frame and "
        "an HUD line with your position, ground height, river flag, and distance to each settlement (RIVER, "
        "HUB, MINE). Move deliberately toward things worth inspecting, look around, and on every turn record "
        "a precise observation — especially anything that looks WRONG or needs tuning. Use `goto` to jump to "
        "a settlement, small `forward`/`turn` steps to inspect up close, `look down` for an overview. "
        "CONTROLS — keep it simple. MOVE with `forward`/`back`/`left`/`right` (strafe relative to your facing) and "
        "`up`/`down`. To LOOK at a settlement, use `face` — it aims straight at the nearest inn; use it whenever the "
        "buildings drift out of frame (the HUD line 'nearest inn … facing it: no' is your cue to `face`). "
        "`goto <SETTLEMENT>` jumps to a fresh overview above a named settlement. `look down`/`level`/`up` tilts. "
        "Do NOT rotate by degrees. Typical loop: `goto` a settlement, `face` to centre the inn, then `forward`/`left`/"
        "`right` to move around and inspect, `face` again if it slips out of view. `forward` follows your gaze, so "
        "after `face` (looking slightly down at the inn) a `forward` brings you closer and lower. Begin: overview of HUB. "
        "Call `done` when you have surveyed enough. GOAL: " + args.goal)
    messages = [{"role": "system", "content": sys_prompt}]

    print(f"[drive] goal: {args.goal}\n[drive] connecting to the running game on {DMON} ...")
    seq = 1
    init = ["goto " + tour[0]] if tour else ([args.start] if args.start else [])
    frame, meta = capture(seq, init)
    log = []
    tour_idx = 1        # next settlement in the itinerary to force-visit
    since_goto = 0      # qwen inspection steps since the last (re)position
    for step in range(1, args.steps + 1):
        messages.append({"role": "user", "content": [
            {"type": "text", "text": f"Step {step}. HUD:\n{meta}\nWhat do you see, and what is your next action?"},
            img_part(frame)]})
        prune(messages)
        m = qwen(messages)
        tcs = m.get("tool_calls") or []
        if not tcs:
            print("[drive] no tool_call returned; stopping.")
            break
        try:
            a = json.loads(tcs[0]["function"]["arguments"])
        except Exception:
            a = {"observation": "(unparseable)", "action": "done"}
        obs, act = a.get("observation", ""), a.get("action", "done")
        val, tgt = a.get("value"), a.get("target")
        print(f"\n── step {step}: {act} {val if val is not None else ''} {tgt or ''}".rstrip())
        print(f"   {obs}")
        log.append({"step": step, "hud": meta, "action": act, "value": val, "target": tgt, "obs": obs})
        messages.append({"role": "assistant", "content": None, "tool_calls": [
            {"id": tcs[0].get("id", "c%d" % step), "type": "function",
             "function": {"name": "step", "arguments": json.dumps(a)}}]})
        messages.append({"role": "tool", "tool_call_id": tcs[0].get("id", "c%d" % step), "content": "ok"})
        # --- tour scheduling: force progression through all settlements (qwen still observed each) ---
        cmds = None
        if tour:
            if since_goto >= args.dwell and tour_idx < len(tour):
                nxt = tour[tour_idx]; tour_idx += 1; since_goto = 0
                cmds = ["goto " + nxt]; print(f"   [tour] advancing → goto {nxt}")
            elif tour_idx >= len(tour) and since_goto >= args.dwell:
                print("\n[drive] toured all settlements."); break
            elif act == "done" and tour_idx < len(tour):
                nxt = tour[tour_idx]; tour_idx += 1; since_goto = 0
                cmds = ["goto " + nxt]; print(f"   [tour] (qwen said done early) → goto {nxt}")
        if cmds is None:
            if act == "done":
                print("\n[drive] qwen reports survey complete."); break
            cmds = translate(act, val, tgt); since_goto += 1
        seq += 1
        frame, meta = capture(seq, cmds)

    # final report
    lines = [f"# Carried World — qwen autopilot validation\n", f"**Goal:** {args.goal}\n",
             f"**Steps:** {len(log)}\n", "## Journey\n"]
    for e in log:
        lines.append(f"- **step {e['step']}** `{e['action']} {e['value'] or ''} {e['target'] or ''}`".rstrip("` ") + "`")
        lines.append(f"  - {e['obs']}")
    open("/tmp/cw_drive_report.md", "w").write("\n".join(lines))
    print(f"\n[drive] report -> /tmp/cw_drive_report.md  ({len(log)} steps)")

if __name__ == "__main__":
    main()
