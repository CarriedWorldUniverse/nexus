#!/usr/bin/env python3
"""Vision review via qwen3.6 (multimodal) on robo-dog — shadow's EYES.

Sends an image to the qwen3.6 vision model (OpenAI-compatible vLLM on the ATOM) and prints
its description. qwen3.6 reads scenes AND on-screen HUD text accurately, so it doubles as a
way to read game HUD/economy panels. It is a REASONING model (emits <think>…</think>); this
script strips that and prints only the final answer.

Usage:
  vision_review.py IMAGE.png ["custom prompt"]
  vision_review.py --dmon ["custom prompt"]        # grab dMon's freshest /tmp/cw_shot|cw_seq frame first
  vision_review.py --dmon-seq N ["custom prompt"]  # grab /tmp/cw_seq_N.png from dMon

Env overrides: OPENAI_BASE_URL (default http://robo-dog:30803/v1), OPENAI_MODEL (gemma-4-12b),
CW_DMON_HOST (default jacinta@100.91.185.71).
"""
import base64, json, os, subprocess, sys, urllib.request, urllib.error

BASE = os.environ.get("OPENAI_BASE_URL", "http://robo-dog:30803/v1")
MODEL = os.environ.get("OPENAI_MODEL", "gemma-4-12b")
DMON = os.environ.get("CW_DMON_HOST", "jacinta@100.91.185.71")

DEFAULT_PROMPT = (
    "This is a screenshot from the voxel game Carried World. Describe concretely: (1) terrain + any "
    "WATER/river and how the water reads, (2) settlements/buildings/roads and whether things sit on the "
    "ground correctly, (3) read out as much HUD text as you can VERBATIM (especially any ECONOMY/stock/"
    "trade numbers), (4) lighting/sky, (5) any visual glitches/artifacts. Be specific."
)

def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True)

def grab_dmon(which):
    if which == "latest":
        r = sh(f"ssh {DMON} 'ls -t /tmp/cw_shot.png /tmp/cw_seq_*.png 2>/dev/null | head -1'")
        remote = r.stdout.strip()
    else:  # a seq index
        remote = f"/tmp/cw_seq_{which}.png"
    if not remote:
        sys.exit("no frame found on dMon /tmp (is the game running / has it bursted?)")
    local = "/tmp/cw_vision_frame.png"
    sh(f"scp -q {DMON}:{remote} {local}")
    if not os.path.exists(local):
        sys.exit(f"failed to pull {remote} from dMon")
    print(f"[grab] {DMON}:{remote} -> {local}")
    return local

def review(path, prompt):
    with open(path, "rb") as f:
        b64 = base64.b64encode(f.read()).decode()
    print(f"[vision] {path} ({len(b64)} b64) -> {BASE} model={MODEL}")
    body = {
        "model": MODEL, "max_tokens": 16000, "temperature": 0.2,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": prompt},
            {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{b64}"}},
        ]}],
    }
    req = urllib.request.Request(
        BASE + "/chat/completions", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer EMPTY"})
    try:
        d = json.load(urllib.request.urlopen(req, timeout=240))
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode()[:600]}")
    except Exception as e:
        sys.exit(f"request failed: {e!r}")
    choice = d["choices"][0]
    m = choice["message"]
    msg = (m.get("content") or "")
    if "</think>" in msg:
        msg = msg.rsplit("</think>", 1)[1]
    msg = msg.strip()
    if not msg:
        # ran out of budget before emitting a final answer, or reasoning-parser split it out
        rc = (m.get("reasoning_content") or "").strip()
        fr = choice.get("finish_reason")
        msg = (rc + f"\n\n[no final answer — finish_reason={fr}; raised max_tokens may be needed]") if rc \
              else f"[empty response — finish_reason={fr}]"
    print(msg)

def main():
    args = sys.argv[1:]
    if not args:
        sys.exit(__doc__)
    if args[0] == "--dmon":
        path = grab_dmon("latest"); prompt = args[1] if len(args) > 1 else DEFAULT_PROMPT
    elif args[0] == "--dmon-seq":
        path = grab_dmon(args[1]); prompt = args[2] if len(args) > 2 else DEFAULT_PROMPT
    else:
        path = args[0]; prompt = args[1] if len(args) > 1 else DEFAULT_PROMPT
    review(path, prompt)

if __name__ == "__main__":
    main()
