# docs/

Design reference.

- [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) — v0.5 primary design. Read this first. v0.5 adds §2.8 knowledge storage + retrieval.
- [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md) — v0.2 provider adapter detail. Companion to §2.3 of the registration spec; defines the interface (chat + embeddings), tool translation, triage, and provider-selection precedence for running aspects against Claude / Gemini / OpenAI / ollama-local.
- [`2026-04-25-nexus-transport-spec.md`](2026-04-25-nexus-transport-spec.md) — v0.1 transport & dispatch. Defines the WS-first protocol between Nexus, Outposts, aspects, and hand harnesses. Supersedes the HTTP registration endpoints from §6.3; introduces Outposts, auto-spawn, and the data-ownership split (per-aspect local, cross-aspect on Nexus).

Future decision records land here as `YYYY-MM-DD-<topic>.md`.
