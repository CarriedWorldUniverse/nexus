package rewriter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DistillTail rewrites records from the previous distilled boundary
// up to the LAST record in the session. Use this when the caller
// doesn't know the precise turn-boundary uuid (claude-code's
// subprocess provider doesn't expose it). The rewriter walks back
// from EOF to the most recent _nexus_distilled marker (or session
// start) and distills that span.
//
// Returns ErrNoBoundary if the session has zero parseable records.
// Otherwise behaves identically to DistillTurn — same atomicity,
// idempotence, error handling, marker semantics.
func (rw *Rewriter) DistillTail(ctx context.Context) (Stats, error) {
	records, fallback, totalBytes, err := readSession(rw.sessionPath())
	if err != nil {
		return Stats{}, err
	}
	// Find the last parseable record — that's the end of the most
	// recent turn. nil entries (parse failures) get skipped.
	boundary := -1
	for i := len(records) - 1; i >= 0; i-- {
		if records[i] != nil {
			boundary = i
			break
		}
	}
	if boundary < 0 {
		return Stats{}, ErrNoBoundary
	}
	return rw.distill(ctx, records, fallback, totalBytes, boundary)
}

// DistillTurn rewrites records from the previous distilled boundary up
// to and including the record identified by turnBoundaryUUID.
//
// Idempotence: records already carrying _nexus_distilled are skipped.
// Atomicity: the new file is written via a temp + rename. On any error,
// the original session jsonl is unchanged.
//
// Part 1 status: walks scope correctly, identifies rewritable records
// (assistant[text], user[tool_result] above thresholds), but the
// per-record distillation calls are gated through the same Distiller
// interface so a stub implementation drives the test suite. Part 2
// extends the in-record branching to handle Agent/Bash/Read/Grep
// heuristics distinctly.
func (rw *Rewriter) DistillTurn(ctx context.Context, turnBoundaryUUID string) (Stats, error) {
	if turnBoundaryUUID == "" {
		return Stats{}, ErrNoBoundary
	}

	records, fallback, totalBytes, err := readSession(rw.sessionPath())
	if err != nil {
		return Stats{}, err
	}

	boundary := findTurnBoundaryIndex(records, turnBoundaryUUID)
	if boundary < 0 {
		return Stats{}, ErrNoBoundary
	}
	return rw.distill(ctx, records, fallback, totalBytes, boundary)
}

// distill is the shared implementation behind DistillTurn and
// DistillTail. Caller has already located the boundary index; this
// function walks the rewrite scope, calls the distiller per record,
// stamps markers, and writes the session back atomically.
func (rw *Rewriter) distill(ctx context.Context, records []rawRecord, fallback [][]byte, totalBytes, boundary int) (Stats, error) {
	start := scopeStart(records, boundary)

	// Pre-compute tool_use_id → tool name from every assistant
	// tool_use AT OR BEFORE the boundary. Tool_uses always precede
	// their tool_result in claude-code's stream, so records past
	// boundary can't pair with anything in scope. Slicing keeps this
	// O(boundary) instead of O(session) — matters once sessions grow
	// past tens of thousands of records on the critical between-turn
	// path.
	toolNames := buildToolUseMap(records[:boundary+1])

	stats := Stats{BytesBefore: totalBytes}
	mutated := false

	for i := start; i <= boundary; i++ {
		rec := records[i]
		if rec == nil {
			// Defensive: skip lines we couldn't parse on read.
			continue
		}
		stats.RecordsScanned++
		if rec.hasMarker() {
			stats.RecordsSkipped++
			continue
		}

		changed, distillerErr := rw.rewriteOne(ctx, rec, toolNames)
		if distillerErr != nil {
			stats.DistillerErrors++
			rw.cfg.Logger.Warn("rewriter: distiller error",
				"uuid", rec.uuid(), "type", rec.recordType(), "err", distillerErr)
			// Don't stamp the marker on errored records — they'll be
			// retried if DistillTurn is called again with a later
			// boundary that still includes them. (In practice that
			// won't happen because the funnel only calls us once per
			// turn, but the property is worth preserving.)
			continue
		}

		if changed {
			// Stamp marker only when we actually changed something.
			// Records that fell through every threshold are left
			// alone — no marker, no rewrite — so the next pass can
			// still reconsider them if thresholds change.
			if err := rec.stampMarker(Marker{
				At:            time.Now().UTC(),
				Model:         rw.cfg.ModelName,
				OriginalBytes: len(fallback[i]),
			}); err != nil {
				return stats, err
			}
			stats.RecordsRewritten++
			mutated = true
		}
	}

	if !mutated {
		// Fast path: nothing actually changed, leave the file untouched.
		// Avoids racing with claude-code on the next --resume open.
		stats.BytesAfter = stats.BytesBefore
		return stats, nil
	}

	if err := writeSessionAtomic(rw.sessionPath(), records, fallback); err != nil {
		return stats, err
	}

	// Approximate post-write byte count by re-marshalling — saves a
	// stat() round-trip and is accurate to within \n placement.
	for _, rec := range records {
		if rec == nil {
			continue
		}
		b, _ := json.Marshal(rec)
		stats.BytesAfter += len(b) + 1
	}
	for i, rec := range records {
		if rec == nil && i < len(fallback) {
			stats.BytesAfter += len(fallback[i]) + 1
		}
	}
	return stats, nil
}

// rewriteOne dispatches per record-type. Returns (changed, err).
//
// Currently handles:
//   - assistant[text] above AssistantTextThreshold
//   - user[tool_result] above ToolResultThreshold
//
// Other record kinds (file-history-snapshot, attachment, system,
// permission-mode, tool_use, etc.) pass through unchanged.
//
// changed=true means at least one content field was replaced. Caller
// stamps the marker when changed=true. err means the distiller
// failed; caller logs and skips marker — the record will still appear
// untouched, but with no marker, so a subsequent DistillTurn run can
// retry.
func (rw *Rewriter) rewriteOne(ctx context.Context, rec rawRecord, toolNames map[string]string) (bool, error) {
	switch rec.recordType() {
	case "assistant":
		return rw.rewriteAssistant(ctx, rec)
	case "user":
		return rw.rewriteUser(ctx, rec, toolNames)
	}
	return false, nil
}

// buildToolUseMap walks every assistant tool_use block in the session
// and returns a map from tool_use_id → tool name. Used by rewriteUser
// to pick the per-tool distillation prompt. O(n) over the session;
// runs once per DistillTurn.
//
// Skips records that fail to parse — the map is best-effort. Missing
// entries fall through to the generic distillation prompt.
func buildToolUseMap(records []rawRecord) map[string]string {
	out := make(map[string]string)
	for _, rec := range records {
		if rec == nil || rec.recordType() != "assistant" {
			continue
		}
		msgRaw, ok := rec["message"]
		if !ok {
			continue
		}
		var msgMap map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msgMap); err != nil {
			continue
		}
		contentRaw, ok := msgMap["content"]
		if !ok {
			continue
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			continue
		}
		for _, blockRaw := range blocks {
			var block map[string]json.RawMessage
			if err := json.Unmarshal(blockRaw, &block); err != nil {
				continue
			}
			var blockType string
			if t, ok := block["type"]; ok {
				_ = json.Unmarshal(t, &blockType)
			}
			if blockType != "tool_use" {
				continue
			}
			var id, name string
			if v, ok := block["id"]; ok {
				_ = json.Unmarshal(v, &id)
			}
			if v, ok := block["name"]; ok {
				_ = json.Unmarshal(v, &name)
			}
			if id != "" && name != "" {
				out[id] = name
			}
		}
	}
	return out
}

// rewriteAssistant handles `{type: assistant, message: {content: [...]}}`.
// Only `content[i].type == "text"` blocks above AssistantTextThreshold
// are distilled. tool_use blocks are structured calls — never modified.
func (rw *Rewriter) rewriteAssistant(ctx context.Context, rec rawRecord) (bool, error) {
	msgRaw, ok := rec["message"]
	if !ok {
		return false, nil
	}
	var msg struct {
		Content []json.RawMessage `json:"content"`
		// Other fields preserved via passthrough roundtrip.
		Other map[string]json.RawMessage `json:"-"`
	}
	// Two-pass parse: pull content out, keep the rest of the message
	// fields opaque so we don't accidentally drop usage/model/stop_reason.
	var msgMap map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msgMap); err != nil {
		return false, nil
	}
	contentRaw, ok := msgMap["content"]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(contentRaw, &msg.Content); err != nil {
		return false, nil
	}

	changed := false
	for i, blockRaw := range msg.Content {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		var blockType string
		if t, ok := block["type"]; ok {
			_ = json.Unmarshal(t, &blockType)
		}
		if blockType != "text" {
			continue
		}
		var text string
		if t, ok := block["text"]; ok {
			_ = json.Unmarshal(t, &text)
		}
		// Spec: distill when len > threshold (strict). Skip when <=.
		if len(text) <= rw.cfg.AssistantTextThreshold {
			continue
		}
		distilled, err := rw.cfg.Distiller.DistillAssistantText(ctx, text)
		if err != nil {
			return changed, err
		}
		if distilled == text {
			continue
		}
		newText, _ := json.Marshal(distilled)
		block["text"] = newText
		newBlock, err := json.Marshal(block)
		if err != nil {
			return changed, fmt.Errorf("rewriter: remarshal text block: %w", err)
		}
		msg.Content[i] = newBlock
		changed = true
	}
	if !changed {
		return false, nil
	}

	newContent, err := json.Marshal(msg.Content)
	if err != nil {
		return changed, fmt.Errorf("rewriter: remarshal assistant content: %w", err)
	}
	msgMap["content"] = newContent
	newMsg, err := json.Marshal(msgMap)
	if err != nil {
		return changed, fmt.Errorf("rewriter: remarshal assistant message: %w", err)
	}
	rec["message"] = newMsg
	return true, nil
}

// rewriteUser handles `{type: user, message: {content: [{type: tool_result, ...}]}}`.
// Distills `content[i].content[j].text` for tool_result blocks above
// ToolResultThreshold. The tool_use_id is left untouched so claude-
// code's matching of result→use stays intact.
//
// Tool name comes from the toolNames map (built once per DistillTurn
// from every prior tool_use record). Empty tool_use_id or unknown id
// → empty tool name, which falls through to the generic distill prompt.
//
// For Agent/Task tool_results, the record's TOP-LEVEL `toolUseResult`
// field also carries a `content` field (claude-code's metadata). We
// sync it to the distilled string so claude-code's UI/cost tracking
// sees the same compressed content rather than the original verbose
// report.
func (rw *Rewriter) rewriteUser(ctx context.Context, rec rawRecord, toolNames map[string]string) (bool, error) {
	msgRaw, ok := rec["message"]
	if !ok {
		return false, nil
	}
	var msgMap map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msgMap); err != nil {
		return false, nil
	}
	contentRaw, ok := msgMap["content"]
	if !ok {
		return false, nil
	}
	var content []json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return false, nil
	}

	changed := false
	// Track distilled outputs by tool_use_id so we can mirror them
	// into the record's top-level toolUseResult field for Agent calls
	// (Part 2 spec: drop the duplication).
	distilledByID := make(map[string]string)

	for i, blockRaw := range content {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		var blockType string
		if t, ok := block["type"]; ok {
			_ = json.Unmarshal(t, &blockType)
		}
		if blockType != "tool_result" {
			continue
		}

		// Look up the tool name via tool_use_id. Missing → empty
		// string → generic distill prompt.
		var toolUseID string
		if v, ok := block["tool_use_id"]; ok {
			_ = json.Unmarshal(v, &toolUseID)
		}
		toolName := toolNames[toolUseID]

		// tool_result.content can be a string OR an array of
		// {type:text,text:...} blocks. claude-code emits the array
		// form for most tools but Bash exit-error results sometimes
		// arrive as a bare string. Handle both.
		innerContentRaw, ok := block["content"]
		if !ok {
			continue
		}

		newInner, distilled, distillerChanged, err := rw.distillToolResultContent(ctx, toolName, innerContentRaw)
		if err != nil {
			return changed, err
		}
		if !distillerChanged {
			continue
		}

		block["content"] = newInner
		newBlock, err := json.Marshal(block)
		if err != nil {
			return changed, fmt.Errorf("rewriter: remarshal tool_result block: %w", err)
		}
		content[i] = newBlock
		changed = true
		if toolUseID != "" && distilled != "" {
			distilledByID[toolUseID] = distilled
		}
	}

	if !changed {
		return false, nil
	}

	newContent, err := json.Marshal(content)
	if err != nil {
		return changed, fmt.Errorf("rewriter: remarshal user content: %w", err)
	}
	msgMap["content"] = newContent
	newMsg, err := json.Marshal(msgMap)
	if err != nil {
		return changed, fmt.Errorf("rewriter: remarshal user message: %w", err)
	}
	rec["message"] = newMsg

	// Agent/Task tool_results carry a top-level toolUseResult.content
	// that duplicates the report. Sync it to the distilled string so
	// claude-code's UI tracking + cost attribution sees the same
	// compressed text. Other tools have small or absent toolUseResult
	// content; we only modify when the distilled value is available.
	//
	// Sync failures are NOT propagated as distiller errors — the
	// inner content distillation has already succeeded, the record's
	// message is correctly rewritten, and the marker SHOULD be
	// stamped. Otherwise a sync error would force the next pass to
	// re-distill already-distilled content, degrading it silently.
	// Sync is best-effort; the inner content is the authoritative
	// form claude-code replays from anyway.
	if err := syncToolUseResult(rec, distilledByID); err != nil {
		rw.cfg.Logger.Warn("rewriter: toolUseResult sync failed (proceeding with marker)",
			"uuid", rec.uuid(), "err", err)
	}

	return true, nil
}

// syncToolUseResult updates rec.toolUseResult.content (for Agent/Task)
// when we distilled the matching tool_result block. This is the spec's
// "drop the duplication" requirement: the same report appears twice in
// the jsonl, so we shrink both copies consistently. For non-Agent
// tools, toolUseResult is either absent or carries small metadata that
// we leave untouched.
//
// distilledByID maps tool_use_id → the distilled string we already
// wrote into the inner block. Looking up the record's tool_use_id
// requires a peek into the tool_result block which we already rewrote
// — easier to track outside, hence the parameter.
//
// rec.toolUseResult exists at the record root, parallel to "message".
// Schema differs per tool — for Agent the shape is:
//
//	{ status, prompt, agentId, agentType, content, totalDurationMs,
//	  totalTokens, totalToolUseCount, usage, toolStats }
//
// We rewrite ONLY content, leaving every other field intact.
func syncToolUseResult(rec rawRecord, distilledByID map[string]string) error {
	if len(distilledByID) == 0 {
		return nil
	}
	turRaw, ok := rec["toolUseResult"]
	if !ok {
		return nil
	}
	// Parse as map so we can mutate content selectively.
	var tur map[string]json.RawMessage
	if err := json.Unmarshal(turRaw, &tur); err != nil {
		// Non-object toolUseResult (Bash error path emits a bare
		// string here). Leave it alone — claude-code's matching is
		// per-tool and we don't want to corrupt unrelated shapes.
		return nil
	}
	// Find the tool_use_id this record corresponds to. The user
	// record's tool_result block carries it, and we already
	// extracted it above — but we need it here too to do the
	// lookup. Pull the first tool_use_id from the rewritten content.
	id := firstToolUseID(rec)
	if id == "" {
		return nil
	}
	distilled, ok := distilledByID[id]
	if !ok {
		return nil
	}
	// Only rewrite if the existing content field is a string (Agent's
	// shape) — array shapes go through the inner-block path already.
	cRaw, hasContent := tur["content"]
	if !hasContent {
		return nil
	}
	var asStr string
	if err := json.Unmarshal(cRaw, &asStr); err != nil {
		return nil
	}
	newC, _ := json.Marshal(distilled)
	tur["content"] = newC
	newTUR, err := json.Marshal(tur)
	if err != nil {
		return fmt.Errorf("rewriter: remarshal toolUseResult: %w", err)
	}
	rec["toolUseResult"] = newTUR
	return nil
}

// firstToolUseID returns the tool_use_id of the first tool_result
// block in rec.message.content, or "" if none. Best-effort — used by
// syncToolUseResult to correlate with distilledByID.
func firstToolUseID(rec rawRecord) string {
	msgRaw, ok := rec["message"]
	if !ok {
		return ""
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return ""
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return ""
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return ""
	}
	for _, blockRaw := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		var t string
		if v, ok := block["type"]; ok {
			_ = json.Unmarshal(v, &t)
		}
		if t != "tool_result" {
			continue
		}
		var id string
		if v, ok := block["tool_use_id"]; ok {
			_ = json.Unmarshal(v, &id)
		}
		if id != "" {
			return id
		}
	}
	return ""
}

// distillToolResultContent handles both shapes of tool_result.content:
//
//   - string: bare text (Bash error path tends to emit this)
//   - []{type, text, ...}: structured blocks (the common shape)
//
// Returns the (possibly rewritten) raw content, the canonical
// distilled string (for toolUseResult sync — empty when there are
// multiple rewrites in one record so no single string represents the
// whole), a bool indicating whether anything changed, and any
// distiller error.
func (rw *Rewriter) distillToolResultContent(ctx context.Context, tool string, raw json.RawMessage) (json.RawMessage, string, bool, error) {
	// Try string form first — cheap parse.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		// Spec: distill when len > threshold (strict). Skip when <=.
		if len(asString) <= rw.cfg.ToolResultThreshold {
			return raw, "", false, nil
		}
		distilled, err := rw.cfg.Distiller.DistillToolResult(ctx, tool, asString)
		if err != nil {
			return raw, "", false, err
		}
		if distilled == asString {
			return raw, "", false, nil
		}
		out, _ := json.Marshal(distilled)
		return out, distilled, true, nil
	}

	// Array form: distill each text block above threshold.
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Unrecognised shape — leave it alone rather than corrupt it.
		return raw, "", false, nil
	}
	changed := false
	var canonical string
	rewriteCount := 0
	for i, blockRaw := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		var blockType string
		if t, ok := block["type"]; ok {
			_ = json.Unmarshal(t, &blockType)
		}
		if blockType != "text" {
			continue
		}
		var text string
		if t, ok := block["text"]; ok {
			_ = json.Unmarshal(t, &text)
		}
		// Spec: distill when len > threshold (strict).
		if len(text) <= rw.cfg.ToolResultThreshold {
			continue
		}
		distilled, err := rw.cfg.Distiller.DistillToolResult(ctx, tool, text)
		if err != nil {
			return raw, "", changed, err
		}
		if distilled == text {
			continue
		}
		newText, _ := json.Marshal(distilled)
		block["text"] = newText
		newBlock, err := json.Marshal(block)
		if err != nil {
			return raw, "", changed, fmt.Errorf("rewriter: remarshal tool_result text block: %w", err)
		}
		blocks[i] = newBlock
		changed = true
		rewriteCount++
		canonical = distilled
	}
	if !changed {
		return raw, "", false, nil
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw, "", changed, fmt.Errorf("rewriter: remarshal tool_result blocks: %w", err)
	}
	if rewriteCount > 1 {
		canonical = ""
	}
	return out, canonical, true, nil
}
