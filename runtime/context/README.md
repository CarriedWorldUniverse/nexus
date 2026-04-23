# runtime/context

Session persistence for `global` and `thread` context modes.

Format: **tree-structured JSONL** per spec §2.6 — every entry carries `id` + `parentId`; active branch defined by head pointer in a sidecar file.

## Layout

- `tree/` — tree operations (§4.6): rewind, fork, branch-summary, replay-from-head, compaction application.
- (flat entry I/O lives at this level once implemented — append, read-by-id, walk-parents, token-count-active-branch.)

## Compaction

Per spec §2.7. `shouldCompact(tokens, window, reserve) := tokens > window - reserve`. Runtime-driven, not provider-driven. Produces `compaction` entries with `firstKeptEntryId` so pre-compact history is preserved in-tree.

## Modes

- `global` — one tree per aspect at `<home>/session/global.jsonl` + `global.head`.
- `thread` — one tree per thread at `<home>/session/threads/<thread_id>.jsonl` + `<thread_id>.head`.
- `stateless` — no persistence, no code paths here.
