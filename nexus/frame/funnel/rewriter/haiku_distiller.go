// Real haiku-backed Distiller via bridle. Replaces the stub used by
// Part 1 tests with a fast-model implementation that drives the
// per-record content rewrites at runtime.

package rewriter

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// HaikuDistiller implements Distiller by calling a fast model
// (claude-haiku-4-5 by default) via a bridle harness. One harness +
// provider config covers both DistillToolResult and DistillAssistantText
// — the prompts differ but the runtime path is the same.
//
// Per-tool branching lives here: DistillToolResult takes a tool name
// and dispatches to specialised handling for the heavy hitters
// (Bash/Read/Grep/Agent). Other tools fall through to a generic
// distill prompt.
type HaikuDistiller struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string
	// Timeout caps each distillation call. Defaults to 5s when zero —
	// haiku is fast but a stalled provider should not stall a turn
	// boundary. The rewriter logs the failure and leaves the record
	// un-marked so a later pass can retry.
	Timeout time.Duration
	// AspectID is recorded against bridle's TurnRequest for audit /
	// usage attribution. Empty is fine for tests; production should
	// pass the Frame's aspect name.
	AspectID string
}

// NewHaikuDistiller returns a configured HaikuDistiller. Returns error
// if required fields are missing.
func NewHaikuDistiller(harness *bridle.Harness, provider bridle.ProviderID, model string) (*HaikuDistiller, error) {
	if harness == nil {
		return nil, fmt.Errorf("rewriter: NewHaikuDistiller requires harness")
	}
	if provider == "" {
		return nil, fmt.Errorf("rewriter: NewHaikuDistiller requires provider")
	}
	if model == "" {
		return nil, fmt.Errorf("rewriter: NewHaikuDistiller requires model")
	}
	return &HaikuDistiller{
		Harness:  harness,
		Provider: provider,
		Model:    model,
		Timeout:  5 * time.Second,
	}, nil
}

const (
	// distillerSystemPromptToolResult is the system prompt for tool
	// result distillation. Per-tool prompts (below) are bolted onto
	// this as the user message context; this stays generic so the
	// model knows it's compressing for context, not summarising for
	// humans.
	distillerSystemPromptToolResult = "You compress tool-call output for an LLM's context window. Output ONLY the compressed text — no preamble, no explanation, no markdown framing. Preserve: what was queried/run, the key signal, any error or non-zero exit, the count if it matters. Drop: verbose lines that the model already extracted in its next turn, decorative borders, repeated headers. Keep it dense — full sentences only when they pack more signal per byte than fragments. Hard cap: 200 bytes."

	// distillerSystemPromptAssistantText is for assistant reasoning
	// prose. The model already extracted decisions; the rewriter is
	// removing exploration scaffolding that won't be re-read.
	distillerSystemPromptAssistantText = "You compress an LLM's prior turn reasoning for its own future context. Output ONLY the compressed text — no preamble. Preserve: conclusions, decisions, plans, hand-offs, commitments to action, references to specific files/identifiers. Drop: exploration prose, hedging, restated context. Hard cap: 150 bytes."
)

// DistillToolResult dispatches per tool name. Tool name may be empty
// (caller couldn't identify it from the surrounding records); the
// generic prompt handles that case.
//
// Per-tool prompts live below. Heavy hitters (Bash/Read/Grep/Agent)
// get specialised guidance because their content shapes are
// predictable; everything else falls through to the generic case.
func (d *HaikuDistiller) DistillToolResult(ctx context.Context, tool, content string) (string, error) {
	prompt := buildToolResultPrompt(tool, content)
	return d.runDistill(ctx, distillerSystemPromptToolResult, prompt, "tool_result/"+tool)
}

// DistillAssistantText compresses assistant reasoning prose.
func (d *HaikuDistiller) DistillAssistantText(ctx context.Context, content string) (string, error) {
	return d.runDistill(ctx, distillerSystemPromptAssistantText, content, "assistant_text")
}

// runDistill is the shared bridle-call path. Bounded by Timeout.
// Returns the compressed text or the original on parser failure
// (failing closed would mean we never compress anything).
func (d *HaikuDistiller) runDistill(parent context.Context, systemPrompt, userMessage, sessionTag string) (string, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req := bridle.TurnRequest{
		AspectID:     d.AspectID,
		SystemPrompt: systemPrompt,
		// Fresh session per call — distillations don't accumulate.
		// The session ID is sufficiently unique that there's no
		// collision risk, and bridle's claude-api provider treats it
		// as a new conversation.
		Session:     bridle.SessionHandle{ID: distillerSessionID(sessionTag), New: true},
		UserMessage: userMessage,
		Provider:    d.Provider,
		Model:       d.Model,
		MaxSteps:    1, // pure text in/out; no tool use
	}

	result, err := d.Harness.RunTurn(ctx, req, &noopRunner{}, &noopSink{})
	if err != nil {
		// Return the original userMessage on error — the rewriter
		// will treat it as identity and not stamp the marker, so
		// the next pass can retry.
		return userMessage, fmt.Errorf("rewriter: distill: %w", err)
	}
	out := strings.TrimSpace(result.FinalText)
	if out == "" {
		// Empty response — treat as identity. Distillation that
		// produces no text is worse than no distillation at all
		// (callers stamp the marker on changed-from-original; empty
		// would erase the record's content entirely).
		return userMessage, nil
	}
	return out, nil
}

// buildToolResultPrompt assembles the user-message body sent to the
// distiller. Per-tool prefixes give the model enough context to make
// the right tradeoffs (Bash exit code matters; Read line count and
// path matter; Agent reports already in distillable prose).
//
// Length-coupled: prefix + content stays well under haiku's input
// budget for any plausible tool_result we'd actually distill (we
// gate above the threshold but anything multi-MB would tax this).
// Caller is responsible for capping content length upstream if
// the threshold is set extremely high.
func buildToolResultPrompt(tool, content string) string {
	switch tool {
	case "Bash":
		return "Tool: Bash. Output:\n" + content
	case "Read":
		return "Tool: Read (file content). Output:\n" + content
	case "Grep":
		return "Tool: Grep (search results). Output:\n" + content
	case "Agent", "Task":
		// Sub-agent reports — already prose, distill aggressively.
		// Operator's data: median 20KB, max 36KB. These are the
		// highest-ROI compression targets.
		return "Tool: Agent (sub-agent report — already prose, compress aggressively while preserving the key findings). Output:\n" + content
	case "Edit", "Write":
		// Usually small results — won't hit the threshold often.
		return "Tool: " + tool + " (file modification result). Output:\n" + content
	case "":
		return "Tool output:\n" + content
	default:
		return "Tool: " + tool + ". Output:\n" + content
	}
}

// distillerSessionID generates a per-call UUIDv4. claude-code
// rejects non-UUID --session-id values with "Invalid session ID.
// Must be a valid UUID." — and the rewriter is precisely the case
// where the distiller IS claude-code (subprocess-stream). UUID format
// is the lowest-common-denominator that all bridle providers accept.
//
// The tag parameter is retained for future log/observability work but
// no longer threaded into the id (UUIDs don't have string slots).
// Providers that log sessions get the bare UUID.
func distillerSessionID(_ string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is impossible on supported platforms.
		// If it does, fall back to a deterministic-but-likely-unique
		// string — claude-code will reject it but the haiku distiller
		// will return an error and the rewriter will leave the record
		// un-marked, which is the right shape for unrecoverable
		// rng failure.
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)
	}
	// RFC 4122 v4: version 4 in byte 6 high nibble; variant 10 in byte 8.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// noopRunner / noopSink satisfy bridle's runtime interfaces — the
// distiller never calls tools and doesn't care about streaming.
type noopRunner struct{}

func (n *noopRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return nil, nil
}

type noopSink struct{}

func (n *noopSink) Emit(_ bridle.Event) {}
