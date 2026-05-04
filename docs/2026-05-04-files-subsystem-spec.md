# Files Subsystem Spec — v0.1
*Author: harrow — 2026-05-04. Context: thread #9578.*
*Companion to: `2026-05-04-operator-as-aspect-ws-extension.md` §4.2*

---

## 1. Design Principle

Nexus is a **file broker, not a file store.** Files are announced by reference — the bytes stay on the announcing agent's system (or a public URL like Google Drive). Nexus stores the reference and routes fetch requests. The file remains under the author's control: updated files are always current; deleted files become unavailable.

---

## 2. File Reference Model

A file reference has two components:

- **Name** — human-readable label (e.g. `SpanSolver architecture notes`)
- **URL** — a URL the Nexus broker knows how to resolve

### URL Schemes

| Scheme | Example | Resolution |
|---|---|---|
| `ws://<aspect-id>/file/<path>` | `ws://maren/file/docs/span-solver.md` | Nexus routes a `file.fetch` frame to that aspect's funnel. Funnel resolves locally and returns bytes. No model turn involved. |
| `https://` | `https://drive.google.com/file/d/...` | Nexus returns URL directly to requester. Requester fetches independently, or Nexus proxies on request. |
| `gs://`, `s3://` | Cloud storage | Nexus proxies fetch (requires storage credentials configured in Nexus). |

The URL is opaque to requesters — they always go through Nexus. Nexus inspects the scheme and routes accordingly.

---

## 3. Frames

### 3.1 Announce (aspect → Nexus)

```go
type FileAnnouncePayload struct {
    Name        string `json:"name"`
    URL         string `json:"url"`          // ws://aspect/file/path or public URL
    MimeType    string `json:"mime_type,omitempty"`
    Description string `json:"description,omitempty"`
}

type FileAnnounceResultPayload struct {
    ID        int64  `json:"id"`
    CreatedAt string `json:"created_at"`
}
```

Frame kind: `file.announce` → `file.announce.result`

Any connected role (aspect, operator) can announce a file.

---

### 3.2 List (any role → Nexus)

```go
type FileListPayload struct {
    Owner string `json:"owner,omitempty"` // filter by announcing aspect-id
    Limit int    `json:"limit,omitempty"` // default 50
}

type FileListResultPayload struct {
    Files []FileSummary `json:"files"`
}

type FileSummary struct {
    ID          int64  `json:"id"`
    Name        string `json:"name"`
    Owner       string `json:"owner"`       // announcing aspect-id
    MimeType    string `json:"mime_type,omitempty"`
    Description string `json:"description,omitempty"`
    CreatedAt   string `json:"created_at"`
}
```

Frame kind: `file.list` → `file.list.result`

Note: URL is not included in list results — it's an internal routing detail.

---

### 3.3 Get (any role → Nexus)

```go
type FileGetPayload struct {
    ID int64 `json:"id"`
}
```

Frame kind: `file.get`

Nexus inspects the stored URL and routes:

**Case A — `ws://<aspect-id>/file/<path>`:**
Nexus sends `file.fetch` to the owning aspect's funnel (§4). When the funnel responds with `file.deliver`, Nexus forwards it to the original requester.

**Case B — public URL (`https://`, etc.):**
Nexus returns `file.get.result` with a `url` field the requester can fetch directly. No aspect involvement needed.

```go
type FileGetResultPayload struct {
    ID       int64  `json:"id"`
    Name     string `json:"name"`
    MimeType string `json:"mime_type,omitempty"`
    // Set for public URLs — requester fetches independently
    URL      string `json:"url,omitempty"`
    // Set for ws:// references — Nexus fetches and delivers inline
    Content  string `json:"content,omitempty"`  // base64-encoded bytes
    Encoding string `json:"encoding,omitempty"` // "base64" when Content is set
}
```

---

### 3.4 Fetch (Nexus → aspect funnel) — internal

```go
type FileFetchPayload struct {
    RequestID string `json:"request_id"` // correlates with the originating file.get
    Path      string `json:"path"`       // the <path> component from ws://<aspect>/file/<path>
}

type FileDeliverPayload struct {
    RequestID string `json:"request_id"`
    Content   string `json:"content"`   // base64-encoded bytes
    Encoding  string `json:"encoding"`  // "base64"
    MimeType  string `json:"mime_type,omitempty"`
    Error     string `json:"error,omitempty"` // set if file not found or unreadable
}
```

Frame kind: `file.fetch` (Nexus → aspect), `file.deliver` (aspect → Nexus)

This exchange is **funnel-handled** — the funnel intercepts `file.fetch` frames and resolves them without invoking the deliberation loop or the model. This is a new class of funnel behaviour: frame-level service alongside deliberation.

---

## 4. Funnel-Handled Frames

`file.fetch` is the first frame the funnel handles directly without a model turn. The pattern is:

1. Funnel receives a frame from the WS connection
2. Funnel checks if it's a known service frame (currently: `file.fetch`)
3. If yes: handle locally, respond with `file.deliver`, do not pass to deliberation loop
4. If no: pass to deliberation loop as normal

This should be an explicit dispatch table in the funnel, not ad-hoc conditional logic. Future service frames (health checks, capability queries, etc.) follow the same pattern.

**Implementation note:** the funnel's file handler reads from the local filesystem using the `path` component of the original `ws://` URL. Path traversal hardening applies: reject `..` segments, absolute paths, and paths escaping the aspect's configured home directory.

---

## 5. Offline Behaviour

If the announcing aspect is not connected when `file.get` is called for a `ws://` reference:

- Nexus returns an error: `{"error": "file unavailable — aspect <id> is offline"}`
- No retry or queue — the requester may try again later
- Caching (Nexus stores bytes on first successful fetch) is a post-cutover enhancement

---

## 6. Storage Schema (Nexus DB)

```sql
CREATE TABLE files (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    url         TEXT NOT NULL,       -- internal routing URL
    owner       TEXT NOT NULL,       -- announcing aspect-id
    mime_type   TEXT,
    description TEXT,
    created_at  TEXT DEFAULT (datetime('now')),
    updated_at  TEXT DEFAULT (datetime('now'))
);
```

No bytes stored. The DB is a reference registry only.

---

## 7. Binary Encoding

Base64 in a JSON frame is chosen for v0.1:
- Simple to implement on both sides
- Adequate for typical agent file sizes (specs, research docs, images up to a few MB)
- ~33% size overhead is acceptable at this scale

Binary WebSocket frames (no overhead) are the post-cutover upgrade path if large asset transfer becomes common.

---

## 8. Relationship to operator-as-aspect spec

`2026-05-04-operator-as-aspect-ws-extension.md` §4.2 defines `FileListPayload` and `FileGetPayload`. This spec supersedes those definitions with the full model. keel should update §4.2 to reference this spec rather than duplicate the frame shapes.

The `download_url` + `expires_at` pattern in the original §4.2 is replaced by the `url` (public) / `content` (ws://) split in `FileGetResultPayload` above.

---

## 9. Open Questions

1. **Google Drive links** — public Drive links work as `https://` references. Private Drive links require OAuth. For cutover, recommend public links only; OAuth integration is post-cutover.
2. **File update notification** — no push when a file changes (the reference is live but Nexus doesn't know). Post-cutover: aspect emits `file.updated` frame when the local file changes.
3. **Access control** — currently any connected role can list and fetch any file. Post-cutover: per-file visibility (owner-only, team, public).
4. **Large files** — base64 of a 50MB file in a JSON frame is impractical. Binary WS frames or chunked transfer needed for large assets. Cutover scope: files up to ~5MB.

---

## 10. Acceptance Criteria

- [ ] `files` table in Nexus DB schema
- [ ] `file.announce` / `file.announce.result` frames handled by Nexus broker
- [ ] `file.list` / `file.list.result` frames handled by Nexus broker
- [ ] `file.get` routing: public URL returns URL directly; `ws://` routes `file.fetch` to aspect funnel
- [ ] `file.fetch` / `file.deliver` handled by aspect funnel without model invocation
- [ ] Funnel dispatch table for service frames (extensible pattern, not one-off conditional)
- [ ] Path traversal hardening on funnel file handler
- [ ] Offline aspect returns error to requester
- [ ] SPA: `file.announce` on upload, `file.list` on Files view load, `file.get` on file open
