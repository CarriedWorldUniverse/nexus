package classification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// TicketTriage classifies a new NEX-* ticket to suggest which aspect
// (or team queue) should own it (NEX-247 — lane 4 of NEX-243).
//
// Separate Verdict shape from the generic three-class triage one
// (trivial/needs-review/suspicious) because ticket ownership is a
// per-aspect routing decision with a confidence dimension, not a
// risk classification.
//
// Fails open with team="oss-nexus-dev" + confidence=low on any
// classifier error — silent drop would let unassigned tickets pile
// up invisibly; default-to-human-queue is the safe pre-deploy
// behaviour.
type TicketTriage struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string // default model; per-call ModelOverride wins

	// AspectRoster maps aspect name → one-line domain description.
	// Embedded in the system prompt so the classifier knows what
	// each aspect owns. Empty falls back to DefaultAspectRoster (the
	// canonical map per NEX-247's ticket description).
	AspectRoster map[string]string

	// Logger, when set, logs each verdict with summary preview + raw
	// model output for post-hoc audit. Same shape as PRTriage.Logger.
	Logger *slog.Logger
}

// TicketTriageInput is the ticket data the classifier needs.
type TicketTriageInput struct {
	Summary     string   // ticket title / one-line summary
	Description string   // full markdown body (trimmed to maxTicketDescLen)
	Labels      []string // jira labels, comma-joined in the prompt
	ModelOverride string // per-call model override; empty = use env/default
}

// TicketTriageVerdict is the classifier's output. Either AssigneeAspect
// OR AssigneeTeam will be set (mutually exclusive). Confidence is a
// soft signal — low means "the operator should look at this rather
// than trust the route".
type TicketTriageVerdict struct {
	AssigneeAspect string `json:"assignee_aspect,omitempty"`
	AssigneeTeam   string `json:"assignee_team,omitempty"`
	Confidence     string `json:"confidence"` // "low" | "medium" | "high"
	Reason         string `json:"reason"`
}

// validConfidence is the closed set of Confidence values.
var validConfidence = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
}

// DefaultAspectRoster is the canonical aspect → domain map per
// NEX-247. Used when TicketTriage.AspectRoster is empty. Operators
// can override per-instance (e.g. for a deployment with different
// aspects) by populating AspectRoster at construction.
var DefaultAspectRoster = map[string]string{
	"shadow":  "coordination, ticketing, spec/planning",
	"keel":    "Frame internals, broker, harness/funnel runtime",
	"anvil":   "cross-stack OSS tooling (nexus-cw products)",
	"plumb":   "convergence / runtime substrate",
	"forge":   "AI/model work",
	"maren":   "art / rendering",
	"wren":    "Unity / game engine",
	"harrow":  "research",
	"verity":  "lore / canon",
}

// defaultTriageTeam is the fallback when the classifier can't pick
// an aspect confidently. Matches NEX-247's DoD.
const defaultTriageTeam = "oss-nexus-dev"

// maxTicketDescLen bounds the description sent to the model. Mirrors
// PRTriage's maxPRDiffLen. Most NEX-* descriptions sit well under
// this; the cap is a safety net against pathological long-tail.
const maxTicketDescLen = 4000

// Classify runs the ticket triage model and returns the verdict.
// Fails open: on any error returns AssigneeTeam=defaultTriageTeam +
// Confidence=low so the ticket lands in the manual-triage queue
// rather than silently dropping.
func (c *TicketTriage) Classify(ctx context.Context, in TicketTriageInput) (TicketTriageVerdict, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return TicketTriageVerdict{}, fmt.Errorf("ticket triage: empty summary")
	}

	roster := c.AspectRoster
	if len(roster) == 0 {
		roster = DefaultAspectRoster
	}

	model := ResolveModel("NEXUS_TICKET_TRIAGE_MODEL", c.Model, in.ModelOverride)

	req := bridle.TurnRequest{
		AppendSystemPrompt: buildTicketTriageSystemPrompt(roster),
		UserMessage:        buildTicketTriageUserMessage(in),
		Provider:           c.Provider,
		Model:              model,
		MaxSteps:           1,
	}

	result, err := c.Harness.RunTurn(ctx, req, nullRunner{}, discardSink{})
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("ticket triage: harness error — failing open to manual queue",
				"err", err)
		}
		return failOpenVerdict("classifier_error"), nil
	}

	verdict, parseErr := ParseTicketTriageVerdict(result.FinalText)
	if parseErr != nil {
		if c.Logger != nil {
			c.Logger.Warn("ticket triage: parse error — failing open to manual queue",
				"raw", result.FinalText, "err", parseErr)
		}
		return failOpenVerdict("parse_failure"), nil
	}

	if c.Logger != nil {
		preview := summary
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		c.Logger.Info("ticket triage verdict",
			"assignee_aspect", verdict.AssigneeAspect,
			"assignee_team", verdict.AssigneeTeam,
			"confidence", verdict.Confidence,
			"reason", verdict.Reason,
			"model", model,
			"summary_preview", preview)
	}

	return verdict, nil
}

// failOpenVerdict returns the "manual queue" routing with a low
// confidence + the reason as the diagnostic. Used by every error
// path in Classify.
func failOpenVerdict(reason string) TicketTriageVerdict {
	return TicketTriageVerdict{
		AssigneeTeam: defaultTriageTeam,
		Confidence:   "low",
		Reason:       reason,
	}
}

// ParseTicketTriageVerdict extracts a TicketTriageVerdict from a
// model's JSON response. Tolerates ```json fences + surrounding
// prose (same shape as ParseVerdict). Fails on:
//   - missing/invalid Confidence
//   - both assignee_aspect AND assignee_team set (mutually exclusive)
//   - neither assignee_aspect NOR assignee_team set
//   - missing Reason
func ParseTicketTriageVerdict(raw string) (TicketTriageVerdict, error) {
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
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: no JSON object in response: %q", trimmed)
	}
	objText := trimmed[start : end+1]

	var v TicketTriageVerdict
	if err := json.Unmarshal([]byte(objText), &v); err != nil {
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: %w", err)
	}
	if !validConfidence[v.Confidence] {
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: invalid confidence %q (want low/medium/high)", v.Confidence)
	}
	if v.AssigneeAspect != "" && v.AssigneeTeam != "" {
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: both assignee_aspect (%q) and assignee_team (%q) set; choose one", v.AssigneeAspect, v.AssigneeTeam)
	}
	if v.AssigneeAspect == "" && v.AssigneeTeam == "" {
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: neither assignee_aspect nor assignee_team set")
	}
	if v.Reason == "" {
		return TicketTriageVerdict{}, fmt.Errorf("parse ticket verdict: missing reason")
	}
	return v, nil
}

// buildTicketTriageSystemPrompt assembles the system prompt with
// the aspect roster baked in. Operators with custom rosters get
// their map rendered as-is; defaults to DefaultAspectRoster.
//
// The "fail to oss-nexus-dev queue when unsure" instruction matches
// failOpenVerdict — keeps the model's confidence behaviour
// consistent with the error-path fallback.
func buildTicketTriageSystemPrompt(roster map[string]string) string {
	var b strings.Builder
	b.WriteString(`You are a ticket triage classifier. Given a NEX-* ticket summary + description + labels, choose which aspect or team should own the work.

Respond with ONLY valid JSON (no markdown, no explanation):
{"assignee_aspect": "<name>" | "", "assignee_team": "<name>" | "", "confidence": "low"|"medium"|"high", "reason": "one short phrase"}

Exactly ONE of assignee_aspect / assignee_team is set; the other is empty string. Both are JSON strings.

ASPECT ROSTER (name → domain):

`)
	// Render the roster in a stable order so prompts hash
	// deterministically across runs.
	names := make([]string, 0, len(roster))
	for name := range roster {
		names = append(names, name)
	}
	// Roster is small; bubble sort by alpha keeps the output
	// deterministic without pulling in sort just for this.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString(" — ")
		b.WriteString(roster[name])
		b.WriteString("\n")
	}
	b.WriteString(`
CONFIDENCE DEFINITIONS:

"high" — Summary + description + labels clearly map to one aspect's domain (e.g. "broker" or "funnel" → keel; "compaction" or "rewriter" → keel; "Unity" or "wren" → wren).

"medium" — Domain signal is present but ambiguous between two aspects, or labels point one way and description another. Pick the better fit; flag with medium.

"low" — Genuinely unclear which aspect owns. PREFER setting assignee_team="oss-nexus-dev" with confidence=low rather than guessing an aspect. False-routing a ticket to the wrong aspect costs more than landing in the manual queue.

ROUTING DEFINITIONS:

assignee_aspect="<name>" — Use when the ticket clearly belongs to one named aspect from the roster above. Reason should cite the domain signal that matched.

assignee_team="oss-nexus-dev" — Use for fallback routing when no aspect clearly owns. Reason should explain what made it ambiguous.

Respond with ONLY the JSON object.`)
	return b.String()
}

// buildTicketTriageUserMessage formats the ticket data for the
// classifier. Bounded description length keeps the prompt cost
// predictable.
func buildTicketTriageUserMessage(in TicketTriageInput) string {
	desc := strings.TrimSpace(in.Description)
	if len(desc) > maxTicketDescLen {
		desc = desc[:maxTicketDescLen] + "…"
	}
	labels := strings.Join(in.Labels, ", ")
	if labels == "" {
		labels = "(none)"
	}
	return fmt.Sprintf("TICKET SUMMARY:\n%s\n\nLABELS:\n%s\n\nDESCRIPTION:\n%s",
		strings.TrimSpace(in.Summary), labels, desc)
}
