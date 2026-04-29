# Vessel — Product Spec v0.1

**Date:** 2026-04-29
**Status:** Draft
**Repo:** `nexus-cw/vessel`

## 1. What Vessel is

Vessel is a desktop application that gives an LLM a face and a voice. The user speaks (or types), the message is routed to the configured chat backend, and the response comes back as audio in a per-recipient voice, lip-syncing to a 3D VRM avatar in a transparent always-on-top window.

It is a **standalone product**, designed as bring-your-own-backend: any OpenAI-compatible endpoint, Anthropic Claude API, a Nexus broker (the reference network this product was originally designed alongside), or community-built adapters built against the Vessel SDK.

The product name reflects the metaphor: the VRM is a vessel for the recipient's identity; the application is a vessel for any LLM. It holds, presents, and speaks for — without claiming to be — the intelligence behind it.

## 2. Why this exists

Two distinct use cases motivate Vessel:

**Ambient interaction.** Chat-based UIs are the right tool for dense, structured work — code review, ticket flow, multi-recipient collaboration. Vessel is for the other mode: ambient interaction. A user says "hey \<recipient-name\>, what's the latest on …" without breaking out of whatever they're doing, hears the response in a configured voice through an avatar with a configured face, and resumes. Recipients-as-individuals reinforced by face + voice.

**Distributable product.** There is a market for "talk to your LLM with a face on it." VTube Studio adjacent tools, AI companion apps, accessibility-driven voice interfaces. Vessel addresses this market by abstracting the chat backend behind an SDK and shipping reference adapters for the common API shapes. Distribution path: desktop binary releases plus the SDK package on npm.

## 3. Architectural decisions (locked)

These are not open questions in this spec:

1. **Native build, not fork.** No Python runtime dependency. No fork-tracking debt against an upstream avatar library.
2. **VRM model format.** Open standard, no licence tax for individual / small-scale use, standardized blendshapes / bones / expressions. Users bring their own; the build bundles CC0 placeholders for development.
3. **Three.js + @pixiv/three-vrm rendering.** Web-tech consistent with the build's frontend. Unity reserved for a possible later swap if animation richness becomes a differentiator.
4. **Tauri v2 shell.** Smaller binaries than Electron (~5MB vs ~80MB), better long-term story, native APIs for transparent always-on-top windows.
5. **whisper.cpp via Rust backend (`whisper-rs`).** No node-gyp surface; native code stays in Rust where it belongs; JS calls `invoke('transcribe', ...)`.
6. **SDK-first with reference adapters.** `ChatSource` interface in `@nexus-cw/vessel-sdk`. Reference adapters bundled at v1: `NexusAdapter`, `OpenAIAdapter` (OpenAI-compatible API shape — covers OpenAI, Ollama, LM Studio, llama.cpp server, vLLM, Together, Groq, OpenRouter, Mistral, DeepSeek, and most local LLM tooling), `AnthropicAdapter` (Anthropic Claude API + emulators).
7. **Single-user at v1.** Vessel-the-binary supports a single user / single config. Multi-tenant deployments are downstream consumer concerns, not v1 scope.

## 4. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                  Vessel (Tauri v2 shell + WebView)                │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │              Renderer (TypeScript / Preact)                 │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │  │
│  │  │ Active VRM   │  │ Static       │  │ Speech queue +   │  │  │
│  │  │ (Three.js +  │  │ portrait     │  │ attention model  │  │  │
│  │  │  three-vrm)  │  │ sidebar      │  │                  │  │  │
│  │  └──────────────┘  └──────────────┘  └──────────────────┘  │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │  │
│  │  │ Name         │  │ TTS          │  │ Lip sync         │  │  │
│  │  │ detector     │  │ (Edge TTS    │  │ (rhubarb pre-    │  │  │
│  │  │              │  │  + Piper)    │  │  playback pass)  │  │  │
│  │  └──────────────┘  └──────────────┘  └──────────────────┘  │  │
│  │  ┌────────────────────────────────────────────────────────┐ │  │
│  │  │  Vessel SDK — ChatSource interface                      │ │  │
│  │  │  ┌──────────┐ ┌──────────┐ ┌────────────────────────┐ │ │  │
│  │  │  │ Nexus    │ │ Anthropic│ │ OpenAI-compatible      │ │ │  │
│  │  │  │ Adapter  │ │ Adapter  │ │ Adapter                │ │ │  │
│  │  │  └──────────┘ └──────────┘ └────────────────────────┘ │ │  │
│  │  └────────────────────────────────────────────────────────┘ │  │
│  └────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │               Tauri (Rust) backend                          │  │
│  │  - Transparent always-on-top window management              │  │
│  │  - File system (VRM imports, config, portrait cache)        │  │
│  │  - whisper-rs (STT)                                          │  │
│  │  - rhubarb-lip-sync subprocess                              │  │
│  │  - HTTP client (adapters use this for backend calls)        │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
                              │
                              │ Backend-specific protocols (per adapter)
                              ▼
                    ┌─────────────────────┐
                    │  Configured backend │
                    │  (Nexus broker /    │
                    │   OpenAI / Claude / │
                    │   etc.)             │
                    └─────────────────────┘
```

### 4.1 Process model

- Single Tauri process for app shell + windowing.
- Renderer (WebView) runs the JS/TS: Three.js, name detection, queue manager, TTS coordinator, ChatSource adapters.
- Tauri Rust backend invokes side processes (rhubarb subprocess, whisper-rs in-process via FFI) — keeps native cost off the JS main thread.

### 4.2 Data flow (input)

1. Mic captured by renderer via `getUserMedia`.
2. Audio chunks streamed to Tauri Rust → whisper-rs transcription.
3. Transcribed text returned → name detector scans for recipient mention.
4. Name detector matches `<recipient>:` / `hey <recipient>` / `<recipient>,` / falls back to last-active.
5. Cleaned message + recipient → ChatSource adapter (`adapter.send(recipient, message)`).
6. Adapter returns response (text, possibly streamed).

### 4.3 Data flow (output)

1. ChatSource adapter streams response tokens from backend (`AsyncIterable<string>`).
2. **TTS coordinator buffers to sentence boundaries.** Tokens accumulate in a buffer; on terminal punctuation (`.`, `!`, `?`, `\n\n`) or on stream-close, the buffered sentence is flushed to TTS. Sentence-boundary buffering is the TTS coordinator's job, NOT the adapter's — adapters expose token streams, the coordinator decides when a unit is speakable. Reasons: phoneme timing on a half-sentence produces wrong audio; TTS quality is meaningfully better on coherent sentences; adapters shouldn't need to know that TTS exists.
3. Each buffered sentence → TTS pipeline (Edge TTS primary, Piper fallback). Output: WAV.
4. **Pre-playback lip sync pass.** Rhubarb-lip-sync runs synchronously on the WAV (with the sentence transcript as a hint), produces phoneme-timing JSON. The blendshape timeline is fully precomputed; runtime is just a timeline lookup driven by audio currentTime.
5. Speech queue receives `(recipient, text, voice, audio, blendshape_timeline)` tuple per sentence.
6. Queue dequeues when active speech completes (or interrupt fires); avatar swap happens between entries; consecutive sentences from the same recipient play continuously without re-swap.
7. Renderer plays audio + drives `VRMExpressionManager` blendshapes from precomputed timeline.

**Why pre-playback rather than streaming:** Rhubarb is a batch tool — it needs the full audio to produce accurate phoneme timings. The latency cost (~100-300ms per sentence on modern CPUs) is paid before playback. The "thinking" expression covers this gap.

## 5. Vessel SDK

The SDK is the integration contract between Vessel and any chat backend. Published as `@nexus-cw/vessel-sdk` on npm.

### 5.1 ChatSource interface

```typescript
export interface ChatSource {
  /** Human-readable name shown in UI ("OpenAI", "Anthropic Claude", "Nexus") */
  readonly name: string;

  /** Adapter implementations declare which recipients are addressable.
      For Nexus, this is the configured roster.
      For OpenAI, it's a single "assistant" identity (or per-config). */
  readonly recipients: Recipient[];

  /**
   * Send a message to a recipient and stream the response text back.
   *
   * Error contract: adapters MUST throw on backend errors (404, rate-limit,
   * timeout, auth failure, network failure). The throw bubbles up to the
   * speech-queue layer which surfaces a user-facing error (spoken offline
   * message template or visual indicator). A clean end-of-stream means
   * "the response finished successfully"; throw means "something went wrong."
   * Never silently terminate the stream on error.
   */
  send(recipient: string, message: string): AsyncIterable<string>;

  /** Optional: subscribe to push events (for backends that support
      server-initiated messages). */
  subscribe?(handler: (event: BackendEvent) => void): () => void;

  /** Lifecycle. */
  connect?(): Promise<void>;
  disconnect?(): Promise<void>;
}

export interface Recipient {
  id: string;          // address ID
  displayName: string; // shown in UI
  voice?: string;      // suggested TTS voice ID
  avatar?: string;     // suggested VRM file path
}

/**
 * Discriminated union of backend-pushed events. Adapters that support push
 * (subscribe()) emit these typed events; consumers (AttentionManager,
 * conversation state) match on `type` and access typed `payload`.
 */
export type BackendEvent =
  | { type: "message"; payload: { recipient: string; from: string; text: string } }
  | { type: "attention"; payload: { from: string; urgency: "normal" | "critical" } }
  | { type: "thread_changed"; payload: { thread_id: string } };
```

### 5.2 Reference adapters bundled at v1

**`NexusAdapter`** — talks to a Nexus broker via WebSocket. Auth via Bearer token. Subscribes to message events filtered for the user's active thread (per §8). Emits `attention` events when a Nexus aspect uses the attention message kind. Recipients are the configured roster.

**`OpenAIAdapter`** — talks to any OpenAI-compatible Chat Completions endpoint. Config: `base_url`, `api_key`, `model`. Single recipient (`assistant`) by default, or multiple if config defines per-recipient model overrides. Streams via SSE. Covers OpenAI, Ollama, LM Studio, llama.cpp server, vLLM, Together, Groq, OpenRouter, Mistral, DeepSeek, and most local-LLM tooling.

**`AnthropicAdapter`** — talks to Anthropic Messages API. Config: `api_key`, `model`. Streams via SSE. Single recipient by default. Covers Anthropic Claude API and emulators.

### 5.3 Community adapters

Adapters not in the v1 bundle (Cohere, Replicate, custom backends) are community-contributed. Two paths:
- **Loose:** published as `@<author>/vessel-adapter-<backend>` on npm, users install per-need.
- **Curated:** a `nexus-cw/vessel-adapters` mono-repo accepting community contributions to a quality bar. Optional, depending on contributor signal.

The SDK contract is the only thing core ships forever. Adapters in core (the three above) carry maintenance commitment; everything else is community.

## 6. Speech queue and interrupts

When multiple recipients have responses pending (broadcast reply, threaded back-and-forth, attention calls), they speak in order — not simultaneously. The queue is the orchestration layer.

### 6.1 Queue model (v1)

- FIFO queue of `(recipient, text, voice, audio, blendshape_timeline)` tuples.
- Active speech plays to completion, then queue advances.
- Avatar swap happens between queue entries — visual + voice change together.
- Single voice at any moment. Multi-avatar visual presence (§7) does not mean multi-voice.

### 6.2 Interrupt rules (v1)

**User-driven only.** A new utterance from the user (typed or spoken, addressing any recipient) interrupts the current speaker. Active speech stops at the next phoneme boundary, queue is preserved (queued items remain queued), and the user's addressed recipient's response (when it arrives) jumps to head-of-queue.

Recipients responding to each other never interrupt each other. They queue. Cleanest mental model: the user is always immediately responsive, recipients wait their turn.

Priority-tagged interrupts and recipient-aware interrupts are deferred to Phase 6+. v1 has the simple rule.

### 6.3 Edge cases

- **Mic active = queue pauses.** When user is speaking (push-to-talk held, or VAD active), avatar speech pauses at the next phoneme boundary and resumes when mic releases.
- **Stale messages drop.** Queue entries older than 30s (configurable) are dropped at dequeue time. Avoids stale ambush after user has moved on.
- **Queue cap.** Maximum 5 entries; older entries dropped if cap hit.
- **Stop hotkey.** Single global hotkey clears the queue + cuts current speaker.

## 7. Visual model

### 7.1 v1: active VRM + static portrait sidebar

- **Active slot** — full Three.js + VRM render. Skeletal animation, lip-sync, idle, expressions. Front-center, full-size.
- **Portrait sidebar** — static portrait images (PNG) for queued recipients and recipients in attention state. Plain HTML `<img>` with CSS, on-screen in a corner or sidebar.
- **Attention indicator** — CSS animation on the relevant portrait (subtle pulse for "calling," brighter glow for emergency).
- **Speech queue still single-voice** — only the active VRM speaks. Static portraits are visual presence, not voice.

### 7.2 Portrait sourcing

Each recipient needs a static portrait. Two ways:
1. **Auto-generate from VRM** — render the VRM in a neutral pose to a PNG at first run, cache locally.
2. **Per-recipient override** — if a hand-crafted portrait PNG is at the configured path, use it instead.

Default: auto-generate. Users can drop in better art later. Auto-generation uses fixed lighting setup so portraits are visually consistent across recipients.

### 7.3 Future visual upgrades (post-v1)

- **v2: two-VRM slots** — second avatar live-rendered in peripheral position. Wave-for-attention with full skeletal animation.
- **v3: full ensemble** — all configured recipients on screen, panel view, ambient presence.

Deferred until v1 ships and the product earns the investment.

## 8. Active-thread filtering

The avatar isn't a passive observer of all chat. It's tied to user attention — only messages in the user's currently-active thread drive speech.

### 8.1 v1 mechanism: client-side inference

Vessel tracks the user's last-addressed recipient / last-active thread. Outgoing messages from user establish the active thread; incoming messages on that thread drive avatar speech; messages on other threads are silently received (still visible in any backend UI for the user).

This requires no backend change. Cost: less precise (doesn't reflect "user clicked another thread elsewhere" until they speak again).

### 8.2 Future: backend-side state

For backends that support it (e.g. Nexus broker), a future enhancement: backend tracks `user.active_thread` as session state, multiple UIs subscribe via push events. Single source of truth. Defer to a later Vessel version + corresponding backend change. Not v1 scope.

### 8.3 Non-Nexus adapters

For OpenAI / Anthropic / others: there is no concept of "thread" beyond what the application maintains. Vessel's internal conversation state is the active thread; no ambiguity.

## 9. Attention mechanism

Recipients outside the user's active thread need a way to break in.

### 9.1 v1 patterns

- **Soft notification (default).** Recipient's portrait appears in sidebar with attention CSS animation (pulse). Optional soft chime. User engages by addressing the recipient (voice or text), queue then plays the recipient's pending message.
- **Direct break-in (declared emergency only).** Recipient speech interrupts current activity. Reserved for narrow emergency taxonomy; user-defined per deployment.

### 9.2 Protocol

**Nexus adapter.** Messages with the attention kind and an optional `urgency` field (`"normal"` or `"critical"`) carry the attention call. Backend rate-limits and allowlists are enforced server-side; the adapter consumes them as `BackendEvent { type: "attention", ... }` events.

**Routing decision: backend emits a distinct WS event type.** The Nexus broker filters attention messages and pushes them as a separate event, which the NexusAdapter forwards directly. Reasons: keeps NexusAdapter narrowly responsible for transport, not message-schema interpretation; avoids forcing every Vessel client to parse attention semantics; backend is the right place to enforce rate-limits and allowlists.

**Other adapters.** OpenAI/Anthropic don't natively support server-initiated messages; attention mechanism is N/A for those backends unless a server-side wrapper emits `BackendEvent` of type `attention` via the SDK's optional `subscribe` method.

### 9.3 Gating

For backends that support attention (Nexus today, others potentially via wrappers), gating is configured per-deployment:

```yaml
attention:
  allowlist:
    normal: ["*"]              # any recipient can call for normal attention (rate-limited)
    critical: ["<recipient>"]  # only configured recipients can declare critical
  rate_limit:
    normal_seconds: 300        # 1 per 5 min per recipient
    critical_seconds: 0        # unlimited; trust gating instead
```

Defaults conservative.

## 10. Per-recipient config

Vessel config at `${app_config_dir}/vessel.config.yaml`:

```yaml
backend:
  adapter: "nexus"  # or "openai", "anthropic", or community adapter name
  config:
    # adapter-specific
    url: "wss://<your-broker>/ws/chat"
    token: "${NEXUS_TOKEN}"

stt:
  engine: "whisper-cpp"
  model: "base.en"
  push_to_talk_hotkey: "Ctrl+Space"

tts:
  engine: "edge-tts"
  fallback_engine: "piper"

queue:
  stale_seconds: 30
  max_entries: 5
  stop_hotkey: "Ctrl+Shift+Esc"

attention:
  allowlist:
    normal: ["*"]
    critical: []
  rate_limit:
    normal_seconds: 300
    critical_seconds: 0

recipients:
  - id: "recipient-a"
    avatar: "models/recipient-a.vrm"
    portrait: "portraits/recipient-a.png"  # optional override
    voice: "en-GB-RyanNeural"
  - id: "recipient-b"
    avatar: "models/recipient-b.vrm"
    voice: "en-US-DavisNeural"

defaults:
  fallback_recipient: "recipient-a"
  thinking_expression: "happy"  # VRM expression key
  offline_message: "{recipient} isn't online right now."
```

## 11. Build plan

Five phases, each independently shippable.

### Phase 1 — Tauri shell + VRM render proof

**Goal:** Validate transparent always-on-top window + VRM model loads + renders.

**Deliverables:**
- Repo scaffold (Tauri v2: `tauri.conf.json`, `src-tauri/`, `src/`).
- `src/main.tsx` — Preact entry, mounts Three.js canvas.
- `src/avatar/Avatar.tsx` — VRM loader using `@pixiv/three-vrm`.
- Hardcoded test VRM (CC0 sample model).
- Transparent always-on-top window enabled via Tauri v2 plugins.

**Acceptance:** App launches, renders model in transparent always-on-top window. Model visible, oriented correctly.

### Phase 2 — ChatSource SDK + reference Nexus adapter

**Goal:** SDK shape + first adapter end-to-end. Vessel can talk to a backend, see responses come back.

**Deliverables:**
- `@nexus-cw/vessel-sdk` package — `ChatSource` interface, types (§5).
- `src/adapters/NexusAdapter.ts` — implements `ChatSource`, connects via WebSocket, handles auth handshake, subscribes to messages, sends via REST.
- Test page with text input + response display. Validates the Vessel-meets-backend seam.

**Phase order rationale:** Doing comms before TTS surfaces the highest-risk integration first. Phase 3 TTS gets driven by real backend reply text, not hardcoded strings.

**Acceptance:** Type a message in Vessel test page → backend delivers to recipient → recipient's response renders as text in Vessel.

### Phase 3 — TTS + lip sync

**Goal:** Avatar speaks responses with mouth movement.

**Deliverables:**
- `src/tts/EdgeTTS.ts` — Node-side websocket client to MS Cognitive Services Edge TTS. Auth-token-rotation behaviour validated against the Python `edge-tts` reference at spike time.
- `src/tts/Piper.ts` — local Piper subprocess fallback.
- `src/lipsync/Rhubarb.ts` — rhubarb-lip-sync subprocess invocation, parses phoneme timing.
- Phoneme → VRM blendshape mapping (starting table, refine at spike):

| Rhubarb | VRM shape |
|---------|-----------|
| A | `aa` |
| B | `aa` at 0.1 |
| C | `ih` |
| D | `aa` |
| E | `oh` at 0.5 |
| F | `ou` |
| G | `ih` at 0.3 |
| H | `aa` at 0.7 |
| X | all at 0 |

- Timeline driver — synchronizes audio playback with blendshape interpolation.

**Acceptance:** Pipe text to TTS, audio plays in selected voice, avatar mouth syncs. Switch between 2-3 Edge TTS voices works.

### Phase 4 — Per-recipient config + avatar swap + idle animations + portrait sidebar

**Goal:** Multiple recipients, each with VRM + voice. Active VRM in main slot, static portraits in sidebar. Idle animations make active avatar feel alive.

**Deliverables:**
- `vessel.config.yaml` loader + schema validation.
- `src/avatar/RecipientAvatars.ts` — preload VRMs, swap active model on recipient change with crossfade.
- Voice config — per-recipient TTS voice ID.
- `src/avatar/PortraitSidebar.tsx` — static portrait row/sidebar UI, CSS animation hooks for attention indicator.
- `src/avatar/PortraitGen.ts` — auto-generate portrait PNG from VRM at first run (fixed lighting). Cache.
- Idle animations: subtle breathing, blink loop on active VRM.
- Avatar swap with crossfade between recipients (~200-300ms).

**Portrait lighting (fixed values):** Three-point setup. Key light at 45° upper-left, intensity 1.0, warm white `#FFF5E0`. Fill at 135° upper-right, intensity 0.4, neutral `#FFFFFF`. Back at 180° lower-back, intensity 0.6, cool `#E0F0FF`. Standard portrait photography lighting; works across VRM skin tones.

**Acceptance:** Address different recipients, different avatars appear with different voices. Active avatar idles (breath, blink). Queued recipients show as portraits in sidebar. Avatar swap is smooth.

### Phase 5 — Speech queue + attention model + STT mic input

**Goal:** Multi-message orchestration + voice input.

**Deliverables:**
- `src/queue/SpeechQueue.ts` — FIFO queue, FIFO with user-interrupt, stale-drop, cap. Stop hotkey integration.
- `src/attention/AttentionManager.ts` — handles `BackendEvent` of type `attention`, drives portrait sidebar CSS state, distinguishes normal vs critical.
- `src/stt/Whisper.ts` — Tauri command bridge to `whisper-rs` in Rust backend. Push-to-talk hotkey.
- Mic-active queue pause (per §6.3).
- "Thinking" expression while waiting for backend response.
- Offline-recipient fallback: configured `offline_message` template spoken if recipient doesn't reply within 30s (configurable).

**Acceptance:** Hold push-to-talk, speak, release → transcribed → routed → response queued → speaks. Multiple recipients respond → queue plays in order. User interruption works. Recipient attention call shows portrait pulse; addressing the calling recipient engages their queued message.

### Phase 6+ (post-v1)

- **Two-VRM slots.** Live-render second avatar for richer attention/queue visual.
- **Full ensemble (N VRMs).** Panel view, all recipients on screen.
- **Spatial audio.** Voice pans with avatar position.
- **Voice activity detection.** Hands-free always-listening mode.
- **OpenAIAdapter, AnthropicAdapter integration testing rounds.**
- **Per-recipient commissioned VRMs.** Replace placeholders with custom art.
- **Public commercial release.** SDK release licence, distribution, install runbook, cross-platform binary builds.

## 12. STT pipeline

**`whisper-rs` via Tauri Rust backend.** Bypasses node-gyp, keeps native code in Rust where it belongs. JS calls `invoke('transcribe', { audio: <wav-bytes> })` via the Tauri command bridge.

**Push-to-talk batches a complete WAV.** Record from key-press to key-release into a single buffer, transcribe in one call on release. No streaming adapter needed at v1.

VAD-driven hands-free mode (always-listening) does need streaming chunking — deferred to Phase 6+.

Model: `base.en` at v1 (~140MB, real-time-capable on modern CPUs). Configurable to larger models for accuracy tradeoff.

## 13. TTS pipeline

**Primary: Edge TTS** via Node websocket reimplementation. MS Cognitive Services Edge TTS is free, fast, ~30+ English voices with personality variation. Reimplemented in Node from the Python `edge-tts` library reference (~200 LOC). Token-rotation behaviour (Python `edge-tts` refreshes tokens per-session) is an assumption to validate at spike time.

**Fallback: Piper.** Local TTS, no internet dependency. Slightly worse quality than Edge TTS but fully offline. Runs as Tauri subprocess.

Per-recipient voice = config field. Voice IDs (e.g. `en-GB-RyanNeural`, `en-US-DavisNeural`) drawn from the Edge TTS catalogue; users map per recipient to fit personality.

## 14. Open questions

These need resolution before the corresponding phase starts.

1. **Tauri v2 transparent always-on-top edge cases.** Verified working on Windows; need to confirm macOS and Linux at Phase 1 spike.
2. **Edge TTS token rotation (assumption vs confirmed).** Validate at Phase 3 spike by reading `edge-tts` Python source or testing live.
3. ~~`whisper-rs` audio chunking.~~ **Resolved:** Push-to-talk batches a complete WAV on key-release; no streaming adapter needed at v1.
4. **Voice perceptual confirmation.** The configured voices for each recipient should be heard during Phase 3 and confirmed before being baked into config defaults.
5. ~~Default VRM portrait lighting.~~ **Resolved:** Three-point setup specified in §11 Phase 4.

## 15. Non-goals

- **Multi-tenant deployment.** Vessel binary is single-user. Multi-user is a downstream consumer concern.
- **VR / 3D world.** Avatar lives in desktop overlay, not immersive environment.
- **Video input / facial tracking.** No webcam, no user-face-mirroring.
- **Cross-recipient simultaneous voice.** Speech queue is strictly single-voice. Multi-voice harmony not a goal.
- **Recipient-initiated UI control.** Recipients can request attention; they cannot move windows, resize, change config. The user owns app state.
- **Custom animation rigs.** Standard VRM blendshapes + bones only at v1.
- **Modifying recipient behavior or prompt context.** Vessel is a pure presentation client. It never injects system prompts, persona instructions, or context into what the recipient receives. The recipient sees the user's message verbatim — Vessel adds nothing. Trust model stays clean: Vessel can be swapped for any other client and recipient behavior is unchanged.

## 16. Glossary

- **Active speaker** — the recipient currently speaking; rendered as the active VRM, voice playing.
- **Attention call** — message with `kind: "attention"` (Nexus context) or `BackendEvent` of type `attention` (any adapter context). Drives portrait sidebar CSS state.
- **Backend** — the LLM source: Nexus broker, OpenAI, Anthropic, or community-adapted source.
- **ChatSource** — the SDK interface adapters implement.
- **Recipient** — an addressable identity in the backend.
- **Speech queue** — the FIFO of pending speech-tuples. User-interrupts, stale-drops, capped.
- **Vessel SDK** — `@nexus-cw/vessel-sdk` npm package, `ChatSource` interface + types.
