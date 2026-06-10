<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`vessel`](https://github.com/CarriedWorldUniverse/vessel)'s live `README.md`.
    Edit the README in the repo, not this page.

# Vessel

A desktop application that gives any LLM a face and a voice.

The user speaks (or types), the message is routed to the configured chat backend, and the response comes back as audio in a per-aspect voice, lip-syncing to a 3D VRM avatar.

Vessel is a standalone product. Bring your own backend: Nexus broker, any OpenAI-compatible API (OpenAI, Ollama, LM Studio, llama.cpp server, vLLM, Together, Groq, OpenRouter, Mistral, DeepSeek, ...), Anthropic Claude API, or community-built adapters against the Vessel SDK.

## Status

**Active prototype.** Work has moved from the original desktop-shell plan to a native build path. Two runnable prototypes exist today: an Electron + Three.js macOS reference build (the original stage proof) and a native iOS SwiftUI companion client, which is the current focus. The iOS app already does the full "hey aspect" voice loop against a live Nexus broker.

Spec v0.2 is finalised at [`docs/spec.md`](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/docs/spec.md) (also mirrored at [`CarriedWorldUniverse/nexus/docs/2026-04-29-avatar-interface-spec.md`](https://github.com/CarriedWorldUniverse/nexus/blob/main/docs/2026-04-29-avatar-interface-spec.md) for the spec-history record). The iOS MVP is specced in [`docs/2026-06-09-vessel-ios-mvp-spec.md`](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/docs/2026-06-09-vessel-ios-mvp-spec.md).

## Stack

The product framing is unchanged — an avatar-and-voice shell over any LLM backend. The implementation is now Apple-native first.

**iOS (current prototype)**

- **Shell:** native SwiftUI app (iOS 18+, Xcode project under [`ios/`](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/ios/))
- **STT:** Apple Speech framework (WhisperKit / server-side Whisper a later option)
- **TTS:** `AVSpeechSynthesizer` for the MVP; Vessel/VoxCPM voices preferred when available
- **Transport:** `URLSessionWebSocketTask` to the Nexus broker, Keychain-backed settings
- **Connector:** Nexus reference connector for the "hey aspect" voice flow

**macOS reference build (earlier proof)**

- Electron + Three.js stage with `@pixiv/three-vrm` avatars, VoxCPM TTS with macOS `say` fallback

**Backend SDK**

- `ChatSource` interface for any chat backend: Nexus broker, any OpenAI-compatible API, Anthropic Claude API, or community adapters. OpenAI and Anthropic adapters are part of integration testing rather than the first iOS cut.

The original transparent-overlay desktop shell (Tauri v2 + Preact, `whisper-rs`, `rhubarb-lip-sync`) remains a possible later direction but is not the path under active development.

## macOS reference build

The current repo includes a runnable Electron + Three.js reference prototype for macOS. It is a focused app-window proof of the Vessel stage, not the final Apple-first architecture.

```bash
npm install
npm start
```

To build a local unsigned app bundle:

```bash
npm run build:mac
open dist/mac*/Vessel.app
```

See [`docs/mac-reference-build.md`](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/docs/mac-reference-build.md) for details.

The reference build supports natural aspect targeting for speech. Say `Hey shadow, can you get me the result for today?` and Vessel routes it through Nexus `aspect.say`, focuses Shadow, and speaks Shadow's response when Nexus delivers it. Background messages from other aspects are shown as notices rather than spoken over the active target. The visible stage defaults to `shadow`, `anvil`, and `plumb`; override with `VESSEL_VISIBLE_ASPECTS`. VoxCPM is the configured TTS path for the dmonextreme reference setup, with macOS `say` as the fallback.

## iOS companion

The iPhone path is a native SwiftUI companion client, not an Electron/Tauri port. It uses Apple Speech, `URLSessionWebSocketTask`, Keychain-backed settings, and the Nexus reference connector to provide the same "hey aspect" voice flow on mobile. See [`docs/2026-06-09-vessel-ios-mvp-spec.md`](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/docs/2026-06-09-vessel-ios-mvp-spec.md).

## Family

Sibling projects under `CarriedWorldUniverse`:
- [`casket-go`](https://github.com/CarriedWorldUniverse/casket-go), [`casket-ts`](https://github.com/CarriedWorldUniverse/casket-ts), [`casket-dotnet`](https://github.com/CarriedWorldUniverse/casket-dotnet) — crypto channel libraries.
- [`interchange`](https://github.com/CarriedWorldUniverse/interchange) — Frame-to-Frame relay server.
- [`nexus`](https://github.com/CarriedWorldUniverse/nexus) — multi-aspect coordination layer.

## License

Apache-2.0. See [LICENSE](https://github.com/CarriedWorldUniverse/vessel/blob/HEAD/LICENSE).
