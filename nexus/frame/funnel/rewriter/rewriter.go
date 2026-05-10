// Package rewriter implements per-turn distillation of claude-code's
// session jsonl. Between funnel turns it walks the records produced by
// the just-completed turn, distills heavy content (large tool_result
// payloads, long assistant text blocks), and writes the result back
// in place — preserving structure (uuids, parentUuid, tool_use_id,
// stop_reason, toolUseResult metadata).
//
// Older turns are immutable. Only the JUST-COMPLETED turn's tail is
// rewritten before the next --resume fires. This preserves
// claude-code's prompt-cache prefix stability: every record above the
// turn boundary stays bit-identical, so resume reads stay warm.
//
// Records carry a `_nexus_distilled` marker after rewriting; subsequent
// passes skip them. The marker is a sibling root field — claude-code
// ignores unknown root keys when replaying.
//
// See G:/My Drive/nexus/general/specs/2026-05-10-jsonl-rewriter-spec.md
// for the full design.
//
// Part 1 of the build (this file): package skeleton + jsonl scan/rewrite
// primitives + marker handling + atomic temp-rename. No distillation
// logic yet — Distiller is an interface; tests use a stub. Funnel wiring
// lands in Part 3.
package rewriter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// MarkerKey is the root-level field name added to records after they
// have been distilled. Recording the marker on the record itself (not
// in a sidecar file) keeps idempotence tied to the data — rsync, file
// copy, jsonl rotation all preserve the marker without further work.
const MarkerKey = "_nexus_distilled"

// Marker is the value stored under MarkerKey on rewritten records.
//
// At, Model, OriginalBytes are diagnostic — the rewriter doesn't read
// them on subsequent passes (presence of the field is what gates
// idempotence), but they let humans audit what happened and let
// follow-up tooling measure compression ratios without an instrumented
// re-read.
type Marker struct {
	At            time.Time `json:"at"`
	Model         string    `json:"model"`
	OriginalBytes int       `json:"originalBytes"`
}

// Distiller distills the heavy fields on individual records. Implementations
// call out to a fast model (haiku) over bridle. Tests pass a stub.
//
// The interface is content-only: the rewriter handles all the structural
// scaffolding (record identification, threshold gating, marker writing,
// atomic temp-rename) and only asks the distiller to compress strings.
//
// DistillToolResult takes a tool name (Bash/Read/Grep/Agent/...) so the
// implementation can branch on per-tool heuristics. Empty tool name is
// allowed (caller couldn't identify it) — implementations should fall
// back to a generic "summarize this output" pass.
//
// Errors are returned, not logged: the rewriter decides whether to skip
// the record, abort the turn, or fall through to fresh-session-id mode
// based on consecutive failure counts.
type Distiller interface {
	DistillToolResult(ctx context.Context, tool, content string) (string, error)
	DistillAssistantText(ctx context.Context, content string) (string, error)
}

// Config is the per-rewriter configuration. SessionPath, ModelName, and
// Distiller are required; thresholds default to spec values when zero.
type Config struct {
	// SessionPath is the absolute path to claude-code's session jsonl,
	// e.g. ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl.
	SessionPath string

	// Distiller compresses heavy content. Required.
	Distiller Distiller

	// ModelName is recorded in the marker for diagnostic purposes.
	// Should match the model the distiller actually used.
	ModelName string

	// ToolResultThreshold — tool_result content shorter than this is
	// passed through untouched. Defaults to 1000 (per spec) when zero.
	ToolResultThreshold int

	// AssistantTextThreshold — assistant[text] content shorter than
	// this is passed through. Defaults to 500 (per spec) when zero.
	AssistantTextThreshold int

	// Logger receives diagnostic output. Required (callers tend to pass
	// a no-op logger in tests rather than nil).
	Logger *slog.Logger
}

// Rewriter walks claude-code session jsonl and distills the just-
// completed turn before the next --resume. Single-instance per session
// (claude-code holds the jsonl during a turn; the funnel serialises
// turns; this is naturally non-concurrent).
type Rewriter struct {
	cfg Config
}

// New returns a configured Rewriter. Returns error if cfg is missing
// required fields.
func New(cfg Config) (*Rewriter, error) {
	if cfg.SessionPath == "" {
		return nil, errors.New("rewriter: SessionPath required")
	}
	if cfg.Distiller == nil {
		return nil, errors.New("rewriter: Distiller required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("rewriter: Logger required")
	}
	if cfg.ToolResultThreshold == 0 {
		cfg.ToolResultThreshold = 1000
	}
	if cfg.AssistantTextThreshold == 0 {
		cfg.AssistantTextThreshold = 500
	}
	return &Rewriter{cfg: cfg}, nil
}

// Stats summarises the work done by a single DistillTurn invocation.
// Empty (zero-valued) Stats means no records matched the rewrite scope
// — common on the first turn (no prior turn boundary) or when the turn
// produced nothing distillable.
type Stats struct {
	RecordsScanned   int
	RecordsRewritten int
	RecordsSkipped   int // already carried _nexus_distilled marker
	BytesBefore      int
	BytesAfter       int
	DistillerErrors  int
}

// rawRecord is the minimal projection of a jsonl record needed to
// decide whether to rewrite. We deliberately keep records as
// json.RawMessage maps so we can rewrite individual fields without
// disturbing unrelated structure (uuids, parentUuid, version, cwd,
// gitBranch, sessionId, ...).
type rawRecord map[string]json.RawMessage

// hasMarker reports whether the record already carries _nexus_distilled.
// Idempotence: rewritten records are skipped on subsequent passes.
func (r rawRecord) hasMarker() bool {
	_, ok := r[MarkerKey]
	return ok
}

// stampMarker writes _nexus_distilled onto the record. The Marker is
// JSON-encoded inline; failure is propagated (json.Marshal of a
// time-stamped struct shouldn't fail in practice, but errors.Is callers
// can distinguish marker errors from distiller errors).
func (r rawRecord) stampMarker(m Marker) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("rewriter: marshal marker: %w", err)
	}
	r[MarkerKey] = raw
	return nil
}

// recordType returns the value of the "type" field, or "" if missing
// or not a string. Used by the rewrite scope filter.
func (r rawRecord) recordType() string {
	raw, ok := r["type"]
	if !ok {
		return ""
	}
	var t string
	if err := json.Unmarshal(raw, &t); err != nil {
		return ""
	}
	return t
}

// uuid returns the value of the "uuid" field, or "" if missing. Used
// to identify the turn boundary.
func (r rawRecord) uuid() string {
	raw, ok := r["uuid"]
	if !ok {
		return ""
	}
	var u string
	if err := json.Unmarshal(raw, &u); err != nil {
		return ""
	}
	return u
}

// readSession reads every record from the session jsonl, returning the
// raw records in file order. Lines that fail to parse are returned as
// nil entries — the caller writes them back unchanged. Defensive: a
// malformed line shouldn't make us drop the rest of the session.
//
// Returns the records, the original line bytes (for unchanged write-
// back), and the total byte count. Caller closes the file.
func readSession(path string) ([]rawRecord, [][]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("rewriter: open session: %w", err)
	}
	defer f.Close()

	var (
		records []rawRecord
		lines   [][]byte
		total   int
	)
	// Buffered scanner with a large buffer — claude-code occasionally
	// emits >1MB single-line records (large tool_result content), and
	// the default 64KB scanner buffer truncates them.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		// Copy: scanner.Bytes() reuses the buffer between calls.
		line := append([]byte(nil), scanner.Bytes()...)
		lines = append(lines, line)
		total += len(line) + 1 // +1 for the newline

		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Defensive: keep the line intact, mark record as nil.
			records = append(records, nil)
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("rewriter: scan session: %w", err)
	}
	return records, lines, total, nil
}

// writeSessionAtomic writes the records back to disk via a temp file
// then renames into place. Avoids the "partial write corrupts the
// session" failure mode where a crash between write and close leaves
// the jsonl half-rewritten.
//
// records[i] takes precedence over fallback[i] when non-nil; otherwise
// the fallback bytes are used (covers parse failures and unmodified
// records). Each line is followed by a single \n (claude-code parses
// jsonl as one record per line; we explicitly write \n not \r\n so
// the rewrite output matches the original line discipline).
//
// Atomicity caveat: os.Rename atomically replaces the destination on
// Unix even if it's open elsewhere (the original inode persists for
// the holder). On Windows the rename returns ERROR_SHARING_VIOLATION
// when the destination is open — we map that to ErrSessionFileBusy so
// the caller can distinguish "claude-code is mid-resume, retry" from
// "disk full, abort". The original jsonl is unchanged in either case;
// the temp file is cleaned up.
func writeSessionAtomic(path string, records []rawRecord, fallback [][]byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nexus-rewriter-*.jsonl.tmp")
	if err != nil {
		return fmt.Errorf("rewriter: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup of the temp file if anything below fails.
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	w := bufio.NewWriter(tmp)
	for i, rec := range records {
		var line []byte
		if rec != nil {
			b, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("rewriter: marshal record %d: %w", i, err)
			}
			line = b
		} else if i < len(fallback) {
			line = fallback[i]
		} else {
			// Should be unreachable: records and fallback are built
			// in lockstep by readSession.
			continue
		}
		if _, err := w.Write(line); err != nil {
			return fmt.Errorf("rewriter: write record %d: %w", i, err)
		}
		if err := w.WriteByte('\n'); err != nil {
			return fmt.Errorf("rewriter: write newline %d: %w", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("rewriter: flush: %w", err)
	}
	// fsync before rename so the rename is durable. Without this, a
	// power loss between rename and writeback leaves a zero-byte file
	// where the session used to be.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("rewriter: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rewriter: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Windows: open destination → ERROR_SHARING_VIOLATION
		// (errno 32). Map to a typed sentinel so callers can retry
		// rather than treat as a hard failure. The os-package error
		// is wrapped so debug detail isn't lost.
		if isFileBusyErr(err) {
			return fmt.Errorf("%w: %v", ErrSessionFileBusy, err)
		}
		return fmt.Errorf("rewriter: rename: %w", err)
	}
	cleanup = false
	return nil
}

// findTurnBoundaryIndex returns the index of the record whose uuid
// matches turnBoundaryUUID, or -1 if not found. Used to identify the
// terminal record of the just-completed turn — rewrite scope walks
// backwards from here to the previous distilled boundary (or session
// start).
func findTurnBoundaryIndex(records []rawRecord, turnBoundaryUUID string) int {
	if turnBoundaryUUID == "" {
		return -1
	}
	for i, rec := range records {
		if rec == nil {
			continue
		}
		if rec.uuid() == turnBoundaryUUID {
			return i
		}
	}
	return -1
}

// scopeStart returns the index where rewrite scope begins (inclusive).
// Walks backwards from boundary looking for the most recent record that
// already carries _nexus_distilled — anything above that has been
// distilled in a prior turn and must not be touched.
//
// Returns 0 if no marker is found (first turn ever — distill from the
// beginning of the session).
func scopeStart(records []rawRecord, boundary int) int {
	for i := boundary - 1; i >= 0; i-- {
		if records[i] != nil && records[i].hasMarker() {
			return i + 1
		}
	}
	return 0
}

// ErrNoBoundary indicates the caller passed a turnBoundaryUUID that
// doesn't appear in the session. Treated as a no-op by DistillTurn —
// nothing to rewrite if we can't locate the boundary.
var ErrNoBoundary = errors.New("rewriter: turn boundary uuid not found in session")

// ErrSessionFileBusy indicates the rename-into-place step failed
// because the destination is open in another process. On Unix
// os.Rename atomically replaces the open file (the prior inode lives
// on for any holder); on Windows the rename returns
// ERROR_SHARING_VIOLATION and leaves the original session jsonl
// intact. The temp file is cleaned up — no corruption — but the
// distillation pass made no progress. Funnel wiring should
// distinguish this from "actually broken" so it can retry once the
// session handle is released.
var ErrSessionFileBusy = errors.New("rewriter: session file is open in another process; rewrite skipped")
