-- Nexus schema. Idempotent — safe to run on every startup.
-- Canonical source lives here; the database is never committed.
-- See registration spec v0.5 §2.8 for the design.
--
-- PRAGMAs (journal_mode=WAL, foreign_keys=ON, busy_timeout=5000) are
-- set via the DSN in storage.Open — the DSN is the authority for
-- connection-level settings. Do not duplicate them here; a drift
-- between DSN and SQL could create hard-to-diagnose behaviour.

-- -------------------------------------------------------------------
-- Knowledge store
-- -------------------------------------------------------------------
-- Technical / operational knowledge entries. Narrative canon is out of
-- scope for this table (per operator #7676). `shared=1` = operator-
-- curated entry visible to Frame aspects that pass `Shared: true` in
-- KnowledgeScope.
--
-- Embedding columns (`embedding`, `embed_model`, `embed_dim`) are
-- RESERVED day-one but unused in v1: the sqlite-vec extension is not
-- loaded (DEFERRED — see schema.go header and registration spec §2.8).
-- When vector retrieval is turned on, these columns are populated by a
-- one-time backfill with no schema migration.
CREATE TABLE IF NOT EXISTS knowledge (
  id           INTEGER PRIMARY KEY,
  from_agent   TEXT NOT NULL,
  topic        TEXT NOT NULL,
  content      TEXT NOT NULL,
  shared       INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
  embedding    BLOB,
  embed_model  TEXT,
  embed_dim    INTEGER,
  UNIQUE(from_agent, topic)
);

CREATE INDEX IF NOT EXISTS idx_knowledge_from_agent ON knowledge(from_agent);
CREATE INDEX IF NOT EXISTS idx_knowledge_shared     ON knowledge(shared) WHERE shared = 1;
CREATE INDEX IF NOT EXISTS idx_knowledge_updated_at ON knowledge(updated_at);

-- FTS5 index mirrors `topic` and `content` for keyword retrieval.
CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_fts USING fts5(
  topic, content,
  content=knowledge,
  content_rowid=id,
  tokenize='porter unicode61'
);

-- Triggers keep FTS in sync with the base table.
-- Invariant: knowledge.id is an auto-increment INTEGER PRIMARY KEY and
-- is never updated. If that ever changes, these triggers would leave
-- the FTS index orphaned (content=external mode uses rowid as the
-- binding between tables). Callers must not UPDATE knowledge.id.
CREATE TRIGGER IF NOT EXISTS knowledge_ai AFTER INSERT ON knowledge BEGIN
  INSERT INTO knowledge_fts(rowid, topic, content) VALUES (new.id, new.topic, new.content);
END;
CREATE TRIGGER IF NOT EXISTS knowledge_ad AFTER DELETE ON knowledge BEGIN
  INSERT INTO knowledge_fts(knowledge_fts, rowid, topic, content) VALUES('delete', old.id, old.topic, old.content);
END;
CREATE TRIGGER IF NOT EXISTS knowledge_au AFTER UPDATE ON knowledge BEGIN
  INSERT INTO knowledge_fts(knowledge_fts, rowid, topic, content) VALUES('delete', old.id, old.topic, old.content);
  INSERT INTO knowledge_fts(rowid, topic, content) VALUES (new.id, new.topic, new.content);
END;

-- -------------------------------------------------------------------
-- Threads (conversation containers for thread-context aspects)
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS threads (
  id           TEXT PRIMARY KEY,
  topic        TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_threads_updated_at ON threads(updated_at);

-- -------------------------------------------------------------------
-- Chat messages (comms traffic — the network's shared chat feed)
-- -------------------------------------------------------------------
-- parent_msg_id + thread_root_msg_id implement the linked-list thread
-- model (task #226): every msg has at most one parent and exactly one
-- thread_root. `reply_to` stays as the caller-provided hint of which
-- thread to continue; the broker resolves the actual parent at INSERT
-- time as "latest msg in this thread when this row was created" so
-- concurrent replies-to-the-same-root naturally chain via SQLite's
-- serialized INSERT order (no DAG branching). thread_root_msg_id is
-- the canonical thread identity used by aspects to compute per-thread
-- session IDs (deterministic uuid_v5 of aspect_name + ":" + thread_root).
-- Top-level msg: parent_msg_id=NULL, thread_root_msg_id=own id.
-- Reply: parent_msg_id=last-in-thread, thread_root_msg_id=that thread's root.
CREATE TABLE IF NOT EXISTS chat_messages (
  id                  INTEGER PRIMARY KEY,
  thread_id           TEXT,
  from_agent          TEXT NOT NULL,
  content             TEXT NOT NULL,
  reply_to            INTEGER,
  parent_msg_id       INTEGER,           -- linked-list parent; NULL = thread root
  thread_root_msg_id  INTEGER,           -- canonical thread identity; NULL for legacy rows pre-#226 migration
  kind                TEXT NOT NULL DEFAULT 'chat',    -- chat | hand | system
  created_at          TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (thread_id)          REFERENCES threads(id)          ON DELETE SET NULL,
  FOREIGN KEY (reply_to)           REFERENCES chat_messages(id)   ON DELETE SET NULL,
  FOREIGN KEY (parent_msg_id)      REFERENCES chat_messages(id)   ON DELETE SET NULL,
  FOREIGN KEY (thread_root_msg_id) REFERENCES chat_messages(id)   ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_thread_id          ON chat_messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_chat_from_agent         ON chat_messages(from_agent);
CREATE INDEX IF NOT EXISTS idx_chat_created_at         ON chat_messages(created_at);
-- Indexes for parent_msg_id + thread_root_msg_id are created in
-- schema.go's Bootstrap step AFTER addMissingColumns runs (the
-- columns are added by ALTER TABLE migration on pre-#226 databases,
-- so a CREATE INDEX here would fail on legacy DBs because schema.sql
-- runs before the column migrations).

-- -------------------------------------------------------------------
-- Chat reactions (toggle-emoji on a chat message — Lock 3 react_to)
-- -------------------------------------------------------------------
-- One row per (msg, reactor, emoji). Toggling means delete-or-insert
-- on this triple — the gateway handles the toggle semantics; the
-- table stores only the current state.
CREATE TABLE IF NOT EXISTS chat_reactions (
  id           INTEGER PRIMARY KEY,
  msg_id       INTEGER NOT NULL,
  reactor      TEXT NOT NULL,
  emoji        TEXT NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (msg_id) REFERENCES chat_messages(id) ON DELETE CASCADE,
  UNIQUE (msg_id, reactor, emoji)
);

CREATE INDEX IF NOT EXISTS idx_chat_reactions_msg ON chat_reactions(msg_id);

-- -------------------------------------------------------------------
-- Shared files (announce_file / share_file — Lock 3 file surface)
-- -------------------------------------------------------------------
-- Files surfaced to chat (announce_file) get a paired chat_messages
-- row; the announce_msg_id links them. Direct shares (share_file)
-- have NULL announce_msg_id and a recipients JSON array column.
CREATE TABLE IF NOT EXISTS shared_files (
  id              INTEGER PRIMARY KEY,
  path            TEXT NOT NULL,
  description     TEXT,
  shared_by       TEXT NOT NULL,
  announce_msg_id INTEGER,
  recipients_json TEXT,                              -- JSON array of aspect ids; NULL for announces
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (announce_msg_id) REFERENCES chat_messages(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_shared_files_shared_by ON shared_files(shared_by);
CREATE INDEX IF NOT EXISTS idx_shared_files_announce  ON shared_files(announce_msg_id);

-- -------------------------------------------------------------------
-- Token usage attribution per Lock 4 (operator #9254/#9258)
-- -------------------------------------------------------------------
-- Forensics, NOT chat-visible. Operator's framing: "I don't want to
-- know while I'm building something — I just want to be able to find
-- where it all went if we run out, so we can look at why and adjust."
--
-- Each row records (msg_id triggering the turn, turn_id internal
-- handle, input/output tokens, model, recorded_at). Joinable from
-- the chat history view: "click chat msg #N → see what that turn
-- cost." Server-stamped recorded_at; aspects don't supply timestamps.
CREATE TABLE IF NOT EXISTS chat_usage (
  id            INTEGER PRIMARY KEY,
  msg_id        INTEGER,                          -- triggering chat msg; NULL for non-comms turns
  turn_id       TEXT NOT NULL,
  aspect        TEXT NOT NULL,
  model         TEXT NOT NULL,
  input_tokens  INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  recorded_at   TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (msg_id) REFERENCES chat_messages(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_usage_msg     ON chat_usage(msg_id);
CREATE INDEX IF NOT EXISTS idx_chat_usage_aspect  ON chat_usage(aspect);
CREATE INDEX IF NOT EXISTS idx_chat_usage_recorded ON chat_usage(recorded_at);

-- -------------------------------------------------------------------
-- Tickets (durable tasks tracked across aspects)
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tickets (
  id           INTEGER PRIMARY KEY,
  title        TEXT NOT NULL,
  body         TEXT,
  status       TEXT NOT NULL DEFAULT 'open',    -- open | in_progress | closed
  owner        TEXT,
  created_by   TEXT NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
  closed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets(status);
CREATE INDEX IF NOT EXISTS idx_tickets_owner  ON tickets(owner);

-- -------------------------------------------------------------------
-- Activity log (observability — what tools aspects are calling)
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS activity (
  id           INTEGER PRIMARY KEY,
  agent        TEXT NOT NULL,
  type         TEXT NOT NULL,                   -- tool | output | input | registration | heartbeat | error
  content      TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_activity_agent      ON activity(agent);
CREATE INDEX IF NOT EXISTS idx_activity_type       ON activity(type);
CREATE INDEX IF NOT EXISTS idx_activity_created_at ON activity(created_at);

-- -------------------------------------------------------------------
-- Session projection — read-only mirror of aspect session trees.
-- Source of truth is the aspect's local JSONL (per transport spec
-- §8); this table exists so the dashboard can render live session
-- history without querying individual aspects. Populated by
-- session.entry.appended frames forwarded from the aspect to Nexus.
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS session_projection (
  id           INTEGER PRIMARY KEY,
  aspect       TEXT NOT NULL,
  session_id   TEXT NOT NULL,
  entry_id     TEXT NOT NULL,
  parent_id    TEXT,
  entry_kind   TEXT NOT NULL,
  entry_ts     TEXT NOT NULL,
  payload      TEXT,                         -- JSON blob, best-effort
  received_at  TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(aspect, session_id, entry_id)       -- idempotent on replay
);

CREATE INDEX IF NOT EXISTS idx_sp_aspect     ON session_projection(aspect);
CREATE INDEX IF NOT EXISTS idx_sp_session    ON session_projection(aspect, session_id);
CREATE INDEX IF NOT EXISTS idx_sp_received   ON session_projection(received_at);

-- -------------------------------------------------------------------
-- Per-aspect bearer tokens (hand-dispatch v0.1 §5.3, §5.4)
-- -------------------------------------------------------------------
-- Each aspect (and the special `frame` identity) holds its own bearer
-- token. The dispatcher resolves a presented token to its aspect_id and
-- admin flag; identity-mismatch and admin-required checks cover the
-- spec's authentication and override-authorization invariants.
--
-- `agent_id` is the aspect's name (matches roster.Name) for normal
-- aspects. The reserved id `frame` carries admin=1 and is the only
-- identity allowed to call override gestures.
--
-- Tokens are minted on first encounter by ReconcileAgentTokens and
-- persisted; subsequent broker startups load them back. Operator can
-- reset by deleting the row (next reconcile mints a fresh token).
CREATE TABLE IF NOT EXISTS agent_tokens (
  agent_id     TEXT PRIMARY KEY,
  token        TEXT NOT NULL UNIQUE,
  admin        INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_agent_tokens_token ON agent_tokens(token);

-- -------------------------------------------------------------------
-- Aspects — keyfile-auth spec §3.1
-- -------------------------------------------------------------------
-- Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md.
-- Replaces the on-disk `aspect.json` model: aspects + their personalities
-- are nexus.db-resident, runtime-pushed to agentfunnel hosts at startup.
--
-- `name` is the unique aspect identity within this Nexus instance. There
-- is no global registry across Nexuses; (nexus_id, name) is the cross-
-- Nexus key, but within a single Nexus the name alone is the PK.
--
-- `status`:
--   'active'  — aspect can be minted, validated, and connected
--   'retired' — keyfiles permanently dead; mint refused; resurrect to revive
--
-- `current_keyfile_version` — incremented on every mint. Validation
-- rejects keyfile blobs whose embedded version is less than this. This
-- is the auto-revoke mechanism: re-mint = bump version = old keyfile
-- dead.
--
-- `aspect_pubkey` — 32-byte Ed25519 public key matching the privkey
-- inside the encrypted keyfile blob. Validation derives the pubkey
-- from the blob's privkey and compares to this column as a sanity
-- check.
--
-- `provider` / `model` / `capabilities` / `metadata` — runtime config
-- pushed to agentfunnel alongside the personality. Replaces aspect.json.
CREATE TABLE IF NOT EXISTS aspects (
  name                    TEXT PRIMARY KEY,
  status                  TEXT NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'retired')),
  current_keyfile_version INTEGER NOT NULL DEFAULT 1,
  aspect_pubkey           BLOB NOT NULL,
  provider                TEXT NOT NULL,
  model                   TEXT NOT NULL,
  capabilities            TEXT,
  metadata                TEXT,
  created_at              TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at              TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_aspects_status ON aspects(status);

-- -------------------------------------------------------------------
-- Aspect personalities — keyfile-auth spec §3.2
-- -------------------------------------------------------------------
-- One row per aspect; CASCADE on delete with the parent aspects row.
-- The three Markdown columns mirror the historical file layout
-- (NEXUS.md / SOUL.md / PRIMER.md). Per task #68 (NEXUS.md naming
-- replaces CLAUDE.md), the column is named `nexus_md` — vendor-neutral.
--
-- `composed` is a cache of the assembled SystemPrompt. Writers MUST
-- invalidate (set to '' or recompute) on any change to nexus_md/soul_md/
-- primer_md so reads always see fresh state. `version` increments on
-- any column edit so connected aspects can detect drift via the
-- personality.refresh push protocol (spec §6).
CREATE TABLE IF NOT EXISTS aspect_personalities (
  aspect_name TEXT PRIMARY KEY
                REFERENCES aspects(name) ON DELETE CASCADE,
  nexus_md    TEXT NOT NULL DEFAULT '',
  soul_md     TEXT NOT NULL DEFAULT '',
  primer_md   TEXT NOT NULL DEFAULT '',
  composed    TEXT NOT NULL DEFAULT '',
  version     INTEGER NOT NULL DEFAULT 1,
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- -------------------------------------------------------------------
-- Nexus identity (single row) — keyfile-auth spec §3.3
-- -------------------------------------------------------------------
-- Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md.
-- Each Nexus instance has its own application-layer identity (separate
-- from TLS cert): a stable nexus_id (UUID), an Ed25519 server keypair
-- used for keyfile decryption (NaCl crypto_box_seal targets the X25519
-- equivalent of server_pubkey), and an HMAC secret for signing
-- session JWTs.
--
-- Single-row constraint: id MUST be 1. The `nexus identity init`
-- subcommand populates this row at first run; subsequent boots fail
-- loudly if the row is absent (don't silently regenerate — the
-- nexus_id must be stable across restarts so keyfiles minted by this
-- Nexus continue to validate).
--
-- This is application-layer identity. TLS cert (PR-A2 nexus cert init)
-- is transport-layer and separate.
CREATE TABLE IF NOT EXISTS nexus_identity (
  id                     INTEGER PRIMARY KEY CHECK (id = 1),
  nexus_id               TEXT NOT NULL,
  server_pubkey          BLOB NOT NULL,
  server_privkey         BLOB NOT NULL,
  session_signing_secret BLOB NOT NULL,
  created_at             TEXT NOT NULL DEFAULT (datetime('now'))
);

-- -------------------------------------------------------------------
-- Nexus settings (single row) — personality decomposition Part 9a
-- -------------------------------------------------------------------
-- Per agent-network/docs/2026-05-08-personality-decomposition-spec.md.
-- Holds the central, network-wide nexus_md content — operational scope
-- shared by every aspect on this Nexus. Per-aspect aspect_personalities
-- .nexus_md remains as a short delta (≤ ~1 paragraph) layered on top.
--
-- Composed prompt = nexus_settings.nexus_md ⊕ aspect.nexus_md ⊕
-- aspect.soul_md ⊕ aspect.primer_md.
--
-- Single-row constraint mirrors nexus_identity. Admin-edited only;
-- aspects have no agent-side write path to this row.
CREATE TABLE IF NOT EXISTS nexus_settings (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  nexus_md   TEXT NOT NULL DEFAULT '',
  -- version starts at 0 so the first SetNexusMD always lands at >=1.
  -- Lets refresh-callback subscribers (Part 9d) reliably detect the
  -- first write — without this, a fresh-table SetNexusMD would land
  -- at version=1 (same as default), and version-equality readers
  -- couldn't distinguish "uninitialised" from "first content."
  version    INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- -------------------------------------------------------------------
-- Operator passkeys — WebAuthn credentials registered for the
-- operator identity (dashboard-ws-port spec §6.1). The operator's
-- passkey unlocks an in-memory keyfile at login; this table holds
-- the public side. Multiple rows = multiple registered devices for
-- the same operator (<operator-host>, dMon, etc).
--
-- credential_id is the WebAuthn credential id (raw bytes, base64url-
-- encoded on the wire); UNIQUE so the same passkey can't double-
-- register. public_key is the COSE-encoded public key returned by
-- the authenticator at registration. sign_count is the
-- authenticator's monotonic replay counter — every successful login
-- must observe a strictly greater value than the stored one, or the
-- assertion is rejected as a replay/clone signal.
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS operator_passkeys (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  credential_id   BLOB NOT NULL UNIQUE,
  -- COSE-encoded public key, kept as a top-level column so callers
  -- that don't decode the credential JSON (e.g. tooling/audit) can
  -- still observe the key material without parsing.
  public_key      BLOB NOT NULL,
  -- Sign count is replicated outside credential_json for the same
  -- reason: the SaveSignCount UPDATE must be atomic against this
  -- column without round-tripping the JSON. credential_json is
  -- read for FinishLogin verification; sign_count is the source of
  -- truth for replay detection.
  sign_count      INTEGER NOT NULL DEFAULT 0,
  label           TEXT NOT NULL,
  -- Full webauthn.Credential record as JSON (transport, flags,
  -- authenticator config, attestation, etc.). Persisted so
  -- FinishLogin can hand the lib back the full record; the inner
  -- sign_count IS the source the lib mutates and we re-marshal on
  -- every successful login.
  credential_json TEXT NOT NULL DEFAULT '',
  registered_at   TEXT NOT NULL DEFAULT (datetime('now')),
  last_used_at    TEXT
);

-- -------------------------------------------------------------------
-- Inbox triage — every chat.send the funnel ingests must produce a
-- structured outcome (reply or skip-with-reason) before the turn ends.
-- This table is the audit trail. Per `2026-05-10-funnel-triage-contract.md`.
--
-- Skip events are NOT broadcast as chat (would create noise); they
-- live here for the operator's 1:1 view to render. Replies live in
-- chat_messages and link back via reply_msg_id.
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inbox_triage (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  aspect_name  TEXT NOT NULL,
  msg_id       INTEGER NOT NULL,
  turn_id      TEXT NOT NULL,
  decision     TEXT NOT NULL CHECK (decision IN ('reply','skip')),
  reason       TEXT NOT NULL DEFAULT '',
  reply_msg_id INTEGER,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (msg_id)       REFERENCES chat_messages(id) ON DELETE CASCADE,
  FOREIGN KEY (reply_msg_id) REFERENCES chat_messages(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_inbox_triage_aspect_time
  ON inbox_triage(aspect_name, created_at);
CREATE INDEX IF NOT EXISTS idx_inbox_triage_msg
  ON inbox_triage(msg_id);
CREATE INDEX IF NOT EXISTS idx_inbox_triage_turn
  ON inbox_triage(turn_id);

-- -------------------------------------------------------------------
-- Broker-mediated credentials — encrypted credential storage for
-- third-party services aspects need to call.
--
-- Originally: provider credentials only (#218; Anthropic/OpenAI API
-- keys). Extended (#NEX-74/75): kind-typed storage covers provider
-- creds + jira/imap/gmail/future. The bundle is a per-kind JSON object
-- encrypted at rest:
--
--   kind='provider'  bundle = {api_shape, base_url, key, default_model?}
--   kind='jira'      bundle = {atlassian_email, atlassian_token, atlassian_subdomain}
--   kind='imap'      bundle = {host, port, user, password, ssl}
--   future kinds add their own shape; the schema stays opaque to it.
--
-- Aspects on remote/unsafe hosts (forge in WSL, plumb on <operator-host>,
-- future external aspects) fetch via broker.credential.fetch (#NEX-77).
-- For provider kinds, the funnel's ProviderEnvResolver auto-injects
-- API_KEY + BASE_URL into TurnRequest.ProviderEnv per turn — no
-- per-call configuration needed.
--
-- Encryption: encrypted_bundle is AES-256-GCM ciphertext of the bundle
-- JSON. The data key is derived from nexus_identity.session_signing_secret
-- (already exists, already OS-protected). Per-row nonce. Doesn't
-- protect against full-DB-plus-nexus_identity compromise — that's an
-- OS-level concern — but stops disk snapshots from leaking creds in
-- cleartext.
--
-- Table name: pre-#NEX-75 was `provider_credentials`. Renamed to
-- `credentials` once non-provider kinds landed (jira/imap don't fit
-- the "provider" framing). Existing rows migrate kind='provider' with
-- their api_shape/base_url/key/default_model packed into bundle JSON.
CREATE TABLE IF NOT EXISTS credentials (
  name              TEXT PRIMARY KEY,
  description       TEXT NOT NULL DEFAULT '',
  -- kind is the credential class. Values: 'provider' | 'jira' | 'imap' |
  -- 'gmail' | ... Future kinds added without schema change. Handlers
  -- validate the bundle shape per kind.
  kind              TEXT NOT NULL,
  -- encrypted_bundle: AES-256-GCM ciphertext of the per-kind JSON bundle.
  -- Plaintext shape is kind-specific (see header comment). Never logged,
  -- never serialised in admin responses.
  encrypted_bundle  BLOB NOT NULL,
  encryption_nonce  BLOB NOT NULL,
  -- allowed_aspects: JSON array of aspect names allowed to use this
  -- credential. Special value '*' means all aspects. Empty array
  -- means no aspect — the credential exists but is parked.
  allowed_aspects   TEXT NOT NULL DEFAULT '["*"]',
  -- mode: 'proxy' (only via broker proxy tools, key never leaves) |
  -- 'fetch' (plaintext fetch allowed, aspect handles the cred directly)
  -- | 'both'. Operator opts in to plaintext fetch per credential.
  -- Defaults to 'proxy' — the safer default. Note: jira/imap kinds
  -- are inherently 'fetch' (no broker-side proxy exists for them);
  -- handler enforces.
  mode              TEXT NOT NULL DEFAULT 'proxy'
                       CHECK (mode IN ('proxy','fetch','both')),
  created_at        TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
  last_used_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_credentials_kind
  ON credentials(kind);

-- credential_audit — one row per use. Operator can answer "who used my
-- DeepSeek key last week" without spelunking logs. Cheap once the
-- dispatch path is in place; lives broker-side so remote aspects can't
-- forge or omit entries.
CREATE TABLE IF NOT EXISTS credential_audit (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  credential_name TEXT NOT NULL,
  aspect          TEXT NOT NULL,
  action          TEXT NOT NULL
                     CHECK (action IN ('proxy_call','plaintext_fetch','fetch','denied')),
  ts              TEXT NOT NULL DEFAULT (datetime('now')),
  -- details: free-form JSON. For proxy_call: {model, endpoint, input_tokens, output_tokens}.
  -- For fetch / plaintext_fetch: {kind}. For denied: {reason, kind}.
  details         TEXT NOT NULL DEFAULT '{}',
  FOREIGN KEY (credential_name) REFERENCES credentials(name) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_credential_audit_credential_ts
  ON credential_audit(credential_name, ts);
CREATE INDEX IF NOT EXISTS idx_credential_audit_aspect_ts
  ON credential_audit(aspect, ts);

-- Note: aspects table needs columns for per-aspect default credentials
-- (default_anthropic_credential, default_openai_credential,
-- default_jira_credential, default_imap_credential). These are added
-- via a Go-side conditional migration in storage/schema.go because
-- ALTER TABLE ADD COLUMN isn't idempotent and schema.sql runs every
-- boot. The provider-shape defaults (anthropic/openai) match the api_shape
-- enum from #218; the non-provider defaults (jira/imap) match kind.

-- -------------------------------------------------------------------
-- MCP profiles — per-aspect MCP server configuration (NEX-168)
-- -------------------------------------------------------------------
-- One row per aspect. `profile` is the operator-authored MCP-server JSON
-- (whatever shape the agent funnel consumes) with credential references
-- left as placeholders. At connect time the broker calls
-- credentials.Store.Substitute(aspect, profile) to resolve those
-- references against the credentials table, writing one credential_audit
-- row per resolved reference (action='fetch', details.via=
-- 'mcp_profile_substitute', details.profile_aspect=<aspect>).
--
-- Storing the unsubstituted profile (not the rendered output) keeps
-- secrets out of this table — only the credentials table holds key
-- material. Re-renders are cheap and always reflect the current
-- credential bundle.
--
-- FK CASCADE on aspects(name): when an aspect is dropped from the
-- network, its MCP profile goes with it. There's no scenario where
-- a profile makes sense without its aspect.
CREATE TABLE IF NOT EXISTS mcp_profiles (
  aspect_name TEXT PRIMARY KEY
                REFERENCES aspects(name) ON DELETE CASCADE,
  profile     TEXT NOT NULL DEFAULT '{}',
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- -------------------------------------------------------------------
-- Network-wide model defaults — NEX-294
-- -------------------------------------------------------------------
-- Single-row table holding fallback model + credential values that
-- apply when an aspect doesn't have a per-aspect override (NEX-263
-- columns on aspects). Resolution order at runtime:
--
--   1. Per-aspect override (aspects.judge_model etc.) — wins if set
--   2. Network default (this table)
--   3. Legacy hardcoded fallback ("haiku" model, ambient env cred)
--
-- The `singleton` CHECK enforces exactly-one-row semantics — no
-- conflicting defaults rows possible. INSERT OR IGNORE on bootstrap
-- so a re-run doesn't duplicate-key error.
--
-- Primary model + primary credential are intentionally NOT defaultable
-- at the network level: the whole point of primary config is per-aspect
-- differentiation (different aspects pick different models). Only judge
-- + compact have the "should be uniform across the network" property
-- that motivates a default.
CREATE TABLE IF NOT EXISTS network_defaults (
  singleton            INTEGER PRIMARY KEY CHECK (singleton = 1),
  judge_model          TEXT,
  judge_credential     TEXT,
  judge_provider       TEXT,
  compact_model        TEXT,
  compact_credential   TEXT,
  updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT OR IGNORE INTO network_defaults (singleton) VALUES (1);

-- -------------------------------------------------------------------
-- Schema metadata — marker only. Real migrations defer until first
-- backwards-incompatible change (per §10 of registration spec).
-- -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_meta (
  key    TEXT PRIMARY KEY,
  value  TEXT NOT NULL
);
INSERT OR IGNORE INTO schema_meta(key, value) VALUES
  ('version',         '1'),
  ('bootstrapped_at', datetime('now'));
