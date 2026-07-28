---
name: vision-review
description: Use whenever you need to actually SEE an image — review a game screenshot/render, judge how something looks, or read on-screen HUD/UI text. Sends the image to a multimodal vLLM model in the k3s cluster (shadow's "eyes") and returns a concrete description. The default path for visually judging Carried World renders (terrain, water, settlements, HUD/economy panels) and for reading any image shadow can't reliably read itself.
when_to_use: 'When you need to actually see an image — review a game screenshot/render, judge how something looks, or read on-screen HUD/UI text that shadow cannot reliably read itself.'
---

# Vision Review (multimodal vLLM in-cluster)

shadow's own image reading is unreliable. Send the frame to a multimodal model in the cluster and use its description instead of guessing — but **treat findings as leads, not verdicts** (see the accuracy caveat under Endpoints).

## How to use

Run the bundled script (Python stdlib only, no deps):

```bash
# review a local image
python3 ~/.claude/skills/vision-review/vision_review.py /path/to/frame.png

# grab dMon's freshest realm frame (/tmp/cw_shot.png or the latest /tmp/cw_seq_*.png) and review it
python3 ~/.claude/skills/vision-review/vision_review.py --dmon

# a specific burst frame (operator "burst" writes 8 frames: /tmp/cw_seq_0..7.png on dMon)
python3 ~/.claude/skills/vision-review/vision_review.py --dmon-seq 4

# custom prompt (focus the review)
python3 ~/.claude/skills/vision-review/vision_review.py --dmon "Focus only on the water/river — does it read clearly as flowing water?"
```

Relay the model's findings to the operator (don't just dump raw output); pull out the actionable bits.

## Endpoints — verified 2026-07-28

All are vLLM OpenAI-compatible, **no API key**. Override per-run with `OPENAI_BASE_URL` / `OPENAI_MODEL`.

| Endpoint | Model | State |
|---|---|---|
| **`http://100.92.111.3:30800/v1`** — `vllm-qwen36` on robo-dog (tailnet NodePort) | **`qwen3.6`**, **64k** ctx | **LIVE — the script default (flipped 2026-07-28).** Most accurate. Vision verified. |
| `http://10.43.73.157:8000/v1` — `vllm-gemma4-vision-dmon` ClusterIP (NodePort 30804), on **dMon** | `gemma-4-12b`, 32k ctx | LIVE — **fallback**. Vision verified. |
| `http://100.92.111.3:30803/v1` — `vllm-gemma4-vision` on robo-dog | `gemma-4-12b` | scaled to 0 |

- **Fallback when qwen36 is scaled down** (it competes for robo-dog's memory, so it may be — check first if a run fails to connect):
  `OPENAI_BASE_URL=http://10.43.73.157:8000/v1 OPENAI_MODEL=gemma-4-12b python3 ~/.claude/skills/vision-review/vision_review.py …`
  gemma-4-12b is noticeably less accurate — treat its findings as leads and **eyeball the frame yourself** (Read renders images) before reporting to the operator. Gemma runs on dMon, so it stays up regardless of what robo-dog is doing.
- **qwen3.6 is now 64k, not 256k** — it was resized on 2026-07-28 to share robo-dog with Ornith. See [[project-ornith-kv-oversizing]]; robo-dog is at ~89/121 GB with both models up.
- **No readiness probes on these deployments.** The pod reports `1/1 Running` within ~10 s while the engine is still loading; qwen3.6 takes ~7 min to serve (Triton warmup + FlashInfer autotune after KV alloc), Ornith ~4 min. Poll `/v1/models` for HTTP 200 — never trust pod status.
- **Reachability from croft:** dMon-hosted ClusterIPs (like 10.43.73.157) work directly. **robo-dog-hosted ClusterIPs do NOT** — reach robo-dog services via its tailnet NodePort `100.92.111.3:<nodePort>`. The LiteLLM `:4000` and ollama `:11434` are ClusterIP-only on robo-dog, so not reachable from croft.
- **`vision_tour.py` sends up to 16 frames in ONE request** (`CW_TOUR_MAX`). Against gemma's 32k context that can overflow — lower `CW_TOUR_MAX` if you get a context-length error.

## Key facts

- **`<think>` stripping:** qwen3.6 is a reasoning model and emits `<think>…</think>` before its answer; the scripts strip it. gemma doesn't emit it, so stripping is a no-op. If you call the API yourself against a reasoning model, keep text after the last `</think>` and use generous `max_tokens`.
- **"burst"** (operator typing it / the game's burst key) writes 8 frames to dMon `/tmp/cw_seq_*.png`; the realm also writes `/tmp/cw_shot.png`. That's the frame source for `--dmon`.
- **Autonomous loop WORKS (proven 2026-07-11):** relaunch the console via `sudo systemd-run --unit=cw-console` running `~/cw_play_drive.sh` (= cw_play_fs.sh + `--drive`; keep the `--setenv` CW_* vars — the base script does NOT set them). The game then polls `/tmp/cw_cmd` (line 0 = seq, then ops: forward/back/left/right/turn/pitch/up/down/setalt/look/face/goto/quit; turn+pitch are RELATIVE, `setalt V` = ground+V, `goto RIVER|HUB|MINE` teleports framed) and writes `/tmp/cw_nav.png` + `/tmp/cw_nav.txt`, then the seq to `/tmp/cw_done`. Yaw convention: facing.x = −sin(yaw). GOTCHA: the 240 s game day means captures land at NIGHT half the time — poll `/tmp/cw_nav.txt`'s clock and re-shoot until ~09:00–15:00.
- **For plain text, not vision:** use Ornith (`http://100.92.111.3:30801/v1`, model `ornith`, 64k ctx) — that's the general local LLM, not this vision endpoint.
