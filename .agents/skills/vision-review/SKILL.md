---
name: vision-review
description: Use whenever you need to actually SEE an image — review a game screenshot/render, judge how something looks, or read on-screen HUD/UI text. Sends the image to the qwen3.6 multimodal model on robo-dog (shadow's "eyes") and returns a concrete description. The default path for visually judging Carried World renders (terrain, water, settlements, HUD/economy panels) and for reading any image shadow can't reliably read itself.
when_to_use: 'When you need to actually see an image — review a game screenshot/render, judge how something looks, or read on-screen HUD/UI text that shadow cannot reliably read itself.'
---

# Vision Review (qwen3.6 on robo-dog)

shadow's own image reading is unreliable. **`qwen3.6` on the ATOM (robo-dog) is multimodal, reachable from shadow, and reads both scenes and on-screen HUD text accurately** — use it as the visual reviewer instead of guessing from a frame yourself. (Operator confirmed: more accurate + faster than the old gemma4.)

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

## Key facts

- **Endpoint:** vLLM OpenAI-compatible, `http://robo-dog:30800/v1`, model `qwen3.6`, **no API key**. (NodePort 30800 → `vllm-qwen36` in the `model-stack` k3s ns. The LiteLLM `:4000` and ollama `:11434` are ClusterIP-only — NOT reachable from shadow.) Config also in `~/qwen/` (`.env`, `test-connection.sh`).
- **Reasoning model:** emits `<think>…</think>` before the answer — the script strips it; if you call the API yourself, keep text after the last `</think>` and use generous `max_tokens`.
- **"burst"** (operator typing it / the game's burst key) writes 8 frames to dMon `/tmp/cw_seq_*.png`; the realm also writes `/tmp/cw_shot.png`. That's the frame source for `--dmon`.
- **Limitation:** this reviews frames that already exist (operator's play/bursts land them in dMon `/tmp`). shadow still can't launch a *fresh* render itself — `voxelgodot` over SSH dies (needs dMon's render seat un-wedged via a real reboot/session-cycle). Once that's clear, shadow render + this skill = a fully autonomous visual-iteration loop.
- It also works as a general local LLM for plain text (256k ctx) if needed.

See memory: `reference_qwen_vision_reviewer`.
