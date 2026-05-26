package classification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// CommsDigest classifies a single chat message as needs-attention or
// background — feeds the operator-offline-catch-up digest (NEX-245,
// lane 2 of NEX-243).
//
// Per-message granularity (vs whole-backlog batch) keeps the
// classifier prompt small + per-call cost predictable. The caller
// (CLI subcommand / dashboard / scheduled job — Slice 2) aggregates
// verdicts into a digest UI.
//
// Fails open with class=needs-attention on any classifier error.
// Silent drop of a real "needs-attention" message is worse than the
// operator scanning one extra background line.
type CommsDigest struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string // default model; per-call ModelOverride wins

	// OperatorName is whose attention we're checking for. Empty
	// falls back to "operator" — matches the convention in the
	// existing chat-routing layer (nexus/chat/router.go).
	OperatorName string

	// Logger, when set, logs each verdict with summary preview +
	// raw model output for post-hoc audit.
	Logger *slog.Logger
}

// CommsDigestInput is the per-message data the classifier needs.
type CommsDigestInput struct {
	From         string // sender name ("anvil", "operator", "shadow", etc.)
	Text         string // message content (trimmed to maxCommsDigestTextLen)
	ThreadHint   string // optional 1-3-line context from surrounding messages
	ModelOverride string // per-call override; empty = use env/default
}

// CommsDigestClass is the closed set of digest verdict labels.
type CommsDigestClass string

const (
	// CommsClassNeedsAttention — direct question, decision needed,
	// blocker, error, explicit @operator mention, anything the
	// operator would want to know about on return.
	CommsClassNeedsAttention CommsDigestClass = "needs-attention"
	// CommsClassBackground — routine status, peer ack, auto-
	// broadcast, low-signal noise the operator can skip.
	CommsClassBackground CommsDigestClass = "background"
)

// CommsDigestVerdict is the classifier output. Reason is a short
// phrase suitable for showing alongside the message in the digest UI.
type CommsDigestVerdict struct {
	Class  CommsDigestClass `json:"class"`
	Reason string           `json:"reason"`
}

// validCommsClasses is the closed set of CommsDigestClass values.
var validCommsClasses = map[CommsDigestClass]bool{
	CommsClassNeedsAttention: true,
	CommsClassBackground:     true,
}

// maxCommsDigestTextLen bounds the message text sent to the model.
// Most chat messages sit well under this; the cap is a safety net
// against pathological long-tail (operator pasting a 50KB log dump).
const maxCommsDigestTextLen = 2000

// Classify runs the comms-digest classifier and returns the verdict.
// Fails open with class=needs-attention on any error — the digest UI
// surfacing one extra background line is much cheaper than silently
// dropping a real attention item.
func (c *CommsDigest) Classify(ctx context.Context, in CommsDigestInput) (CommsDigestVerdict, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return CommsDigestVerdict{}, fmt.Errorf("comms digest: empty text")
	}
	if len(text) > maxCommsDigestTextLen {
		text = text[:maxCommsDigestTextLen] + "…"
	}

	operatorName := c.OperatorName
	if operatorName == "" {
		operatorName = "operator"
	}

	model := ResolveModel("NEXUS_COMMS_DIGEST_MODEL", c.Model, in.ModelOverride)

	req := bridle.TurnRequest{
		AppendSystemPrompt: buildCommsDigestSystemPrompt(operatorName),
		UserMessage:        buildCommsDigestUserMessage(in.From, text, in.ThreadHint),
		Provider:           c.Provider,
		Model:              model,
		MaxSteps:           1,
	}

	result, err := c.Harness.RunTurn(ctx, req, nullRunner{}, discardSink{})
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("comms digest: harness error — failing open to needs-attention",
				"err", err, "from", in.From)
		}
		return CommsDigestVerdict{Class: CommsClassNeedsAttention, Reason: "classifier_error"}, nil
	}

	verdict, parseErr := ParseCommsDigestVerdict(result.FinalText)
	if parseErr != nil {
		if c.Logger != nil {
			c.Logger.Warn("comms digest: parse error — failing open to needs-attention",
				"raw", result.FinalText, "err", parseErr, "from", in.From)
		}
		return CommsDigestVerdict{Class: CommsClassNeedsAttention, Reason: "parse_failure"}, nil
	}

	if c.Logger != nil {
		preview := text
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		c.Logger.Info("comms digest verdict",
			"class", verdict.Class,
			"reason", verdict.Reason,
			"from", in.From,
			"model", model,
			"text_preview", preview)
	}
	return verdict, nil
}

// ParseCommsDigestVerdict extracts a CommsDigestVerdict from the
// model's JSON response. Tolerates ```json fences + prose around
// the JSON (same shape as ParseVerdict / ParseTicketTriageVerdict).
// Rejects invalid class values + missing reason.
func ParseCommsDigestVerdict(raw string) (CommsDigestVerdict, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < 0 || end < start {
		return CommsDigestVerdict{}, fmt.Errorf("parse comms verdict: no JSON object in response: %q", trimmed)
	}
	objText := trimmed[start : end+1]

	var v CommsDigestVerdict
	if err := json.Unmarshal([]byte(objText), &v); err != nil {
		return CommsDigestVerdict{}, fmt.Errorf("parse comms verdict: %w", err)
	}
	if !validCommsClasses[v.Class] {
		return CommsDigestVerdict{}, fmt.Errorf("parse comms verdict: invalid class %q (want needs-attention / background)", v.Class)
	}
	if v.Reason == "" {
		return CommsDigestVerdict{}, fmt.Errorf("parse comms verdict: missing reason field")
	}
	return v, nil
}

// buildCommsDigestSystemPrompt assembles the per-call system prompt.
// Operator name is inlined so the model knows whose attention to
// optimise for (a multi-operator deployment would have multiple
// classifiers each scoped to one operator's perspective).
func buildCommsDigestSystemPrompt(operatorName string) string {
	return `You classify chat messages for an offline-operator catch-up digest. The operator is "` + operatorName + `". Your job: which of these messages does ` + operatorName + ` need to read, and which can they skip?

Respond with ONLY valid JSON (no markdown, no explanation):
{"class": "needs-attention"|"background", "reason": "one short phrase"}

CLASS DEFINITIONS:

"needs-attention" — ` + operatorName + ` should see this on catch-up. Includes:
- Direct @-mention or question to ` + operatorName + `
- Decision needed / blocked-on-operator state
- Error, failure, cascade, anything operator-recoverable
- Aspect explicitly says "waiting on ` + operatorName + `" or similar
- Cross-cutting strategic update (new initiative, deployment, deadline)

"background" — routine traffic, safe to skim or skip:
- Peer-to-peer aspect ack ("done", "👍", "ok")
- Automated broadcast / status pulse / heartbeat
- Per-turn confirmation noise the aspect emits during normal work
- Discussion between two aspects with no operator-action implication

BIAS: when uncertain prefer "needs-attention". Operator missing one important thread costs more than operator scanning one extra line.

Respond with ONLY the JSON object.`
}

// buildCommsDigestUserMessage formats the per-message classification
// context. Bounded text length keeps the prompt cost predictable.
func buildCommsDigestUserMessage(from, text, threadHint string) string {
	var b strings.Builder
	b.WriteString("FROM: ")
	b.WriteString(strings.TrimSpace(from))
	b.WriteByte('\n')
	if hint := strings.TrimSpace(threadHint); hint != "" {
		b.WriteString("\nTHREAD CONTEXT:\n")
		b.WriteString(hint)
		b.WriteByte('\n')
	}
	b.WriteString("\nMESSAGE:\n")
	b.WriteString(text)
	return b.String()
}
