#!/usr/bin/env python3
"""qwen NPC-journey TOUR — feed the captured walk frames to qwen3.6 (multimodal) in one request and
get a flowing running commentary + a world overview. qwen is the vision engine (more accurate at
identifying things); this just orchestrates. ~1.4k tokens/frame (qwen caps per-image internally), so
a ~16-frame journey is ~23k tokens — well inside the 256k context.

Usage: vision_tour.py                # pull /tmp/cw_seq_*.png from dMon, narrate
       vision_tour.py <dir-or-glob>  # narrate local frames
Env: OPENAI_BASE_URL, OPENAI_MODEL, CW_DMON_HOST, CW_TOUR_MAX (default 16)
"""
import base64, glob, json, os, subprocess, sys, urllib.request

BASE = os.environ.get("OPENAI_BASE_URL", "http://robo-dog:30803/v1")
MODEL = os.environ.get("OPENAI_MODEL", "gemma-4-12b")
DMON = os.environ.get("CW_DMON_HOST", "jacinta@100.91.185.71")
MAX = int(os.environ.get("CW_TOUR_MAX", "16"))

PROMPT = (
    "These are SEQUENTIAL frames from a traveler journeying across the game world 'Carried World' "
    "(Aotearoa-inspired): from the RIVER settlement, through grassland and native Bush, toward the "
    "HUB and the MINE, along dirt roads. Give a flowing RUNNING COMMENTARY of the journey, like "
    "narrating a flythrough — call out what the traveler passes: terrain and relief, settlements / "
    "buildings (and their state), the native Bush (mānuka scrub, tree-ferns, rimu/tōtara podocarps), "
    "water, roads. Use the on-screen HUD position text to ground where things are. Then finish with a "
    "short OVERVIEW: what is this world like to move through?"
)

def main():
    args = sys.argv[1:]
    if args:
        frames = sorted(glob.glob(args[0] + "/*.png") if os.path.isdir(args[0]) else glob.glob(args[0]))
    else:
        subprocess.run("rm -f /tmp/cw_seq_*.png", shell=True)
        subprocess.run(f"scp -q '{DMON}:/tmp/cw_seq_*.png' /tmp/ 2>/dev/null", shell=True)
        frames = sorted(glob.glob("/tmp/cw_seq_*.png"))
    if not frames:
        sys.exit("no frames (run a --walk first to capture /tmp/cw_seq_*.png)")
    if len(frames) > MAX:                      # sample evenly down to MAX
        step = len(frames) / MAX
        frames = [frames[int(i * step)] for i in range(MAX)]
    print(f"[tour] {len(frames)} frames -> {BASE} {MODEL}")
    content = [{"type": "text", "text": PROMPT}]
    for f in frames:
        b64 = base64.b64encode(open(f, "rb").read()).decode()
        content.append({"type": "image_url", "image_url": {"url": f"data:image/png;base64,{b64}"}})
    body = {"model": MODEL, "max_tokens": 6000, "temperature": 0.4,
            "messages": [{"role": "user", "content": content}]}
    req = urllib.request.Request(BASE + "/chat/completions", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer EMPTY"})
    try:
        d = json.load(urllib.request.urlopen(req, timeout=400))
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode()[:600]}")
    u = d.get("usage", {})
    print(f"[tour] prompt_tokens={u.get('prompt_tokens')} completion_tokens={u.get('completion_tokens')}\n")
    m = d["choices"][0]["message"]
    msg = (m.get("content") or "")
    if "</think>" in msg:
        msg = msg.rsplit("</think>", 1)[1]
    msg = msg.strip()
    print(msg if msg else "[reasoning-only]\n" + (m.get("reasoning_content") or "")[-2500:])

if __name__ == "__main__":
    main()
