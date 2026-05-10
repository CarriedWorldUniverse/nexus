package rewriter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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

	records, fallback, totalBytes, err := readSession(rw.cfg.SessionPath)
	if err != nil {
		return Stats{}, err
	}

	boundary := findTurnBoundaryIndex(records, turnBoundaryUUID)
	if boundary < 0 {
		return Stats{}, ErrNoBoundary
	}
	start := scopeStart(records, boundary)

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

		changed, distillerErr := rw.rewriteOne(ctx, rec)
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

	if err := writeSessionAtomic(rw.cfg.SessionPath, records, fallback); err != nil {
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
func (rw *Rewriter) rewriteOne(ctx context.Context, rec rawRecord) (bool, error) {
	switch rec.recordType() {
	case "assistant":
		return rw.rewriteAssistant(ctx, rec)
	case "user":
		return rw.rewriteUser(ctx, rec)
	}
	return false, nil
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
// Tool name is not directly available in the user record; Part 2 will
// thread it in by walking back to the preceding assistant tool_use
// block. For now we pass the empty string and let the distiller fall
// back to a generic summary.
func (rw *Rewriter) rewriteUser(ctx context.Context, rec rawRecord) (bool, error) {
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

		// tool_result.content can be a string OR an array of
		// {type:text,text:...} blocks. claude-code emits the array
		// form for most tools but Bash exit-error results sometimes
		// arrive as a bare string. Handle both.
		innerContentRaw, ok := block["content"]
		if !ok {
			continue
		}

		newInner, distillerChanged, err := rw.distillToolResultContent(ctx, innerContentRaw)
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
	return true, nil
}

// distillToolResultContent handles both shapes of tool_result.content:
//
//   - string: bare text (Bash error path tends to emit this)
//   - []{type, text, ...}: structured blocks (the common shape)
//
// Returns the (possibly rewritten) raw content, a bool indicating
// whether anything changed, and any distiller error.
func (rw *Rewriter) distillToolResultContent(ctx context.Context, raw json.RawMessage) (json.RawMessage, bool, error) {
	// Try string form first — cheap parse.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		// Spec: distill when len > threshold (strict). Skip when <=.
		if len(asString) <= rw.cfg.ToolResultThreshold {
			return raw, false, nil
		}
		distilled, err := rw.cfg.Distiller.DistillToolResult(ctx, "", asString)
		if err != nil {
			return raw, false, err
		}
		if distilled == asString {
			return raw, false, nil
		}
		out, _ := json.Marshal(distilled)
		return out, true, nil
	}

	// Array form: distill each text block above threshold.
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Unrecognised shape — leave it alone rather than corrupt it.
		return raw, false, nil
	}
	changed := false
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
		distilled, err := rw.cfg.Distiller.DistillToolResult(ctx, "", text)
		if err != nil {
			return raw, changed, err
		}
		if distilled == text {
			continue
		}
		newText, _ := json.Marshal(distilled)
		block["text"] = newText
		newBlock, err := json.Marshal(block)
		if err != nil {
			return raw, changed, fmt.Errorf("rewriter: remarshal tool_result text block: %w", err)
		}
		blocks[i] = newBlock
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw, changed, fmt.Errorf("rewriter: remarshal tool_result blocks: %w", err)
	}
	return out, true, nil
}
