// Package tree implements the tree-structured session JSONL format
// per registration spec §2.6. A session is a single append-only
// `.jsonl` file plus a tiny sidecar pointer file recording the
// current active head. Every entry carries an `id` and a `parentId`
// (empty for the root). Branching is cheap (the JSONL grows, the
// sidecar moves); rewind / fork / branch-summary operations are
// implemented by moving the head pointer without truncating history.
package tree

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// EntryKind mirrors the spec §2.6 kinds. These are the canonical
// session-tree kinds; providers.EntryKind is a superset (adds things
// like custom.*) but the tree owns the ones listed here.
type EntryKind string

const (
	KindTurnUser       EntryKind = "turn.user"
	KindTurnAssistant  EntryKind = "turn.assistant"
	KindTurnToolResult EntryKind = "turn.tool_result"
	KindSystemPrompt   EntryKind = "system.prompt"
	KindCompaction     EntryKind = "compaction"
	KindBranchSummary  EntryKind = "branch_summary"
)

// Entry is a single session-tree record as stored on disk.
type Entry struct {
	ID       string         `json:"id"`
	ParentID string         `json:"parentId,omitempty"`
	Kind     EntryKind      `json:"kind"`
	TS       time.Time      `json:"ts"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// headSidecar is the on-disk active-head pointer file format.
type headSidecar struct {
	Head string `json:"head"`
}

// Tree is a handle on a session's JSONL + sidecar pair. Safe for
// concurrent use within a single process — all mutating operations
// take the tree mutex. Cross-process concurrency is NOT supported
// (session files are owned by a single agent runtime).
// AppendHook is invoked (synchronously, after the tree mutex is
// released) for each successful Append. Used by the agent runtime to
// forward entries upward as session.entry.appended frames for Nexus-
// side observability. Implementations must not call back into the
// tree; they should hand the entry off to a channel / queue.
type AppendHook func(Entry)

type Tree struct {
	jsonlPath   string
	sidecarPath string
	mu          sync.Mutex
	entropy     io.Reader // ULID entropy — package-level var would leak between tests

	// onAppend is optional. Set via SetAppendHook.
	onAppend AppendHook
}

// SetAppendHook installs a callback that fires after each successful
// Append. The hook runs on the Append caller's goroutine, after the
// tree mutex has been released, so it must not reach back into the
// tree or do anything slow. Pass nil to clear.
func (t *Tree) SetAppendHook(h AppendHook) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onAppend = h
}

// Open opens (or creates) the session files at dir/<name>.jsonl and
// dir/<name>.head.json. Creates the directory if missing. Works on
// both a brand-new session and an existing one.
func Open(dir, name string) (*Tree, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tree: mkdir %q: %w", dir, err)
	}
	t := &Tree{
		jsonlPath:   filepath.Join(dir, name+".jsonl"),
		sidecarPath: filepath.Join(dir, name+".head.json"),
		entropy:     ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0),
	}
	return t, nil
}

// JSONLPath returns the session log path.
func (t *Tree) JSONLPath() string { return t.jsonlPath }

// SidecarPath returns the head-pointer file path.
func (t *Tree) SidecarPath() string { return t.sidecarPath }

// -------------------------------------------------------------------
// Append / Head / Replay
// -------------------------------------------------------------------

// Append writes a new entry to the JSONL, records it as the new
// active head, and returns it with ID and TS populated. ParentID
// defaults to the current head if the caller leaves it empty; pass
// a specific ParentID to branch off an earlier entry.
//
// If an AppendHook is installed, it fires after the mutex is
// released with the written entry — the hook gets the fully
// populated Entry (id + ts + parent) and can forward it.
func (t *Tree) Append(ctx context.Context, e Entry) (Entry, error) {
	written, hook, err := t.appendLocked(ctx, e)
	if err != nil {
		return Entry{}, err
	}
	if hook != nil {
		hook(written)
	}
	return written, nil
}

// appendLocked does the file-I/O work under the mutex and returns
// both the written entry and the currently-installed hook (captured
// under the lock so concurrent SetAppendHook doesn't produce a stale
// pointer dereference).
func (t *Tree) appendLocked(ctx context.Context, e Entry) (Entry, AppendHook, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if e.Kind == "" {
		return Entry{}, nil, errors.New("tree.Append: Kind required")
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	if e.ID == "" {
		id, err := ulid.New(ulid.Timestamp(e.TS), t.entropy)
		if err != nil {
			return Entry{}, nil, fmt.Errorf("tree.Append: ulid: %w", err)
		}
		e.ID = id.String()
	}
	if e.ParentID == "" {
		head, err := t.readHead()
		if err != nil {
			return Entry{}, nil, fmt.Errorf("tree.Append: read head: %w", err)
		}
		e.ParentID = head // empty string for root entry
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return Entry{}, nil, fmt.Errorf("tree.Append: marshal: %w", err)
	}

	f, err := os.OpenFile(t.jsonlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Entry{}, nil, fmt.Errorf("tree.Append: open jsonl: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(raw, '\n')); err != nil {
		return Entry{}, nil, fmt.Errorf("tree.Append: write: %w", err)
	}
	// fsync the JSONL before updating the head pointer so a crash
	// between the two can't leave the sidecar pointing at an entry
	// that wasn't durably written.
	if err := f.Sync(); err != nil {
		return Entry{}, nil, fmt.Errorf("tree.Append: sync: %w", err)
	}
	if err := t.writeHead(e.ID); err != nil {
		return Entry{}, nil, fmt.Errorf("tree.Append: write head: %w", err)
	}
	// Capture the currently-installed hook under the lock — caller
	// invokes it after the mutex is released.
	return e, t.onAppend, nil
}

// Head returns the current active-head entry ID. Empty string on a
// brand-new session.
func (t *Tree) Head() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readHead()
}

// SetHead moves the active head pointer to the given entry ID.
// Intended for rewind / fork operations — the entry must already
// exist in the JSONL (otherwise the next Replay would fail).
func (t *Tree) SetHead(ctx context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if id == "" {
		return errors.New("tree.SetHead: empty id")
	}
	_, ok, err := t.findEntry(id)
	if err != nil {
		return fmt.Errorf("tree.SetHead: scan: %w", err)
	}
	if !ok {
		return fmt.Errorf("tree.SetHead: entry %q not found in log", id)
	}
	return t.writeHead(id)
}

// Replay returns the entries on the active branch, oldest first.
// Walks from the current head back along parentId links, collecting
// matching entries, then reverses. Callers get a chronological
// view of the session regardless of where the JSONL physically
// stored branches.
func (t *Tree) Replay(ctx context.Context) ([]Entry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	head, err := t.readHead()
	if err != nil {
		return nil, fmt.Errorf("tree.Replay: head: %w", err)
	}
	if head == "" {
		return nil, nil
	}

	index, err := t.loadIndex()
	if err != nil {
		return nil, fmt.Errorf("tree.Replay: index: %w", err)
	}

	var branch []Entry
	cur := head
	for cur != "" {
		e, ok := index[cur]
		if !ok {
			return nil, fmt.Errorf("tree.Replay: entry %q missing from log", cur)
		}
		branch = append(branch, e)
		cur = e.ParentID
	}
	// The walk went head → root; reverse to chronological order.
	// Must NOT sort by timestamp — same-millisecond entries or
	// backdated ones can land across parent/child boundaries and
	// silently corrupt the branch.
	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}
	return branch, nil
}

// All returns every entry in the log, regardless of which branch
// they belong to. Useful for export / backup / audit. Ordered by
// file position, not timestamp.
func (t *Tree) All(ctx context.Context) ([]Entry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readAll()
}

// Fork sets the head to an earlier entry. A subsequent Append will
// link its ParentID to this entry, creating a new branch in the tree.
// Shorthand for SetHead; exists so callers can self-document intent.
func (t *Tree) Fork(ctx context.Context, id string) error { return t.SetHead(ctx, id) }

// -------------------------------------------------------------------
// Internal helpers
// -------------------------------------------------------------------

func (t *Tree) readHead() (string, error) {
	raw, err := os.ReadFile(t.sidecarPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if len(raw) == 0 {
		return "", nil
	}
	var s headSidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("sidecar corrupt: %w", err)
	}
	return s.Head, nil
}

func (t *Tree) writeHead(id string) error {
	raw, err := json.Marshal(headSidecar{Head: id})
	if err != nil {
		return err
	}
	// Write + fsync + rename for atomicity on the head pointer.
	tmp := t.sidecarPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, t.sidecarPath)
}

// loadIndex returns a map id → entry for every entry in the log.
// O(n) per call; acceptable for typical session sizes (hundreds to
// low thousands of turns). Streamed scan could replace this when
// sessions grow past that scale.
func (t *Tree) loadIndex() (map[string]Entry, error) {
	entries, err := t.readAll()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out, nil
}

// findEntry locates one entry by ID without building a full index.
// Returns (entry, true, nil) on hit, (_, false, nil) on miss,
// (_, _, err) on I/O failure.
func (t *Tree) findEntry(id string) (Entry, bool, error) {
	f, err := os.Open(t.jsonlPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // allow entries up to 1 MB
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupt lines (same rationale as readAll).
			continue
		}
		if e.ID == id {
			return e, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Entry{}, false, err
	}
	return Entry{}, false, nil
}

func (t *Tree) readAll() ([]Entry, error) {
	f, err := os.Open(t.jsonlPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A partial write from a crashed Append can leave the last
			// line unparseable. Log once and skip rather than bricking
			// the whole session — the sidecar will still point at the
			// last durably-written entry, which readers can find.
			slog.Warn("tree: skipping unparseable entry",
				"path", t.jsonlPath, "line", lineNum, "err", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
