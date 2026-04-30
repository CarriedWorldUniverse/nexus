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
CREATE TABLE IF NOT EXISTS chat_messages (
  id           INTEGER PRIMARY KEY,
  thread_id    TEXT,
  from_agent   TEXT NOT NULL,
  content      TEXT NOT NULL,
  reply_to     INTEGER,
  kind         TEXT NOT NULL DEFAULT 'chat',    -- chat | hand | system
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE SET NULL,
  FOREIGN KEY (reply_to)  REFERENCES chat_messages(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_thread_id  ON chat_messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_chat_from_agent ON chat_messages(from_agent);
CREATE INDEX IF NOT EXISTS idx_chat_created_at ON chat_messages(created_at);

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
