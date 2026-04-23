# shared/

Cross-cutting modules used by both the Nexus process and the runtime.

- `paths/` — path resolution for home folders, session files, logs, credentials.
- `auth/` — v1: shared `NEXUS_TOKEN`. v2: per-aspect tokens. Long-term: mTLS.
- `schemas/` — JSON schemas for aspect.json, registration payload, hand envelopes.
