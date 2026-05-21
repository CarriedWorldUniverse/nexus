// Package classification provides AI-switchable classification lanes
// for PR triage, comms digest, activity summarization, and ticket
// auto-triage. Each lane defaults to a cheap model (DeepSeek) with
// per-lane env var overrides and per-call model_override support.
//
// Model selection priority (highest to lowest):
//  1. Per-call model_override
//  2. Per-lane env var (e.g. NEXUS_PR_TRIAGE_MODEL)
//  3. Hardcoded default (typically "deepseek-chat")
package classification

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ResolveModel resolves the model for a classification lane.
// envVar is the lane-specific env var name (e.g. "NEXUS_PR_TRIAGE_MODEL").
// defaultModel is the hardcoded fallback (e.g. "deepseek-chat").
// perCallOverride is the optional per-call model_override; empty means
// use env var or default.
func ResolveModel(envVar, defaultModel, perCallOverride string) string {
	if perCallOverride != "" {
		return perCallOverride
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return defaultModel
}

// VerdictClass labels the classification output.
type VerdictClass string

const (
	ClassTrivial     VerdictClass = "trivial"
	ClassNeedsReview VerdictClass = "needs-review"
	ClassSuspicious  VerdictClass = "suspicious"
)

// Verdict is the structured classification output shared by all lanes.
type Verdict struct {
	Class  VerdictClass `json:"class"`
	Reason string       `json:"reason"`
}

// validClasses is the closed set of VerdictClass values.
var validClasses = map[VerdictClass]bool{
	ClassTrivial:     true,
	ClassNeedsReview: true,
	ClassSuspicious:  true,
}

// ParseVerdict extracts a Verdict from a model's JSON response.
// Tolerates ``` fences and surrounding prose (same pattern as
// filter.go parseJudgeJSON). Fails on invalid class values — the
// caller should fail open.
func ParseVerdict(raw string) (Verdict, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}

	// Locate the JSON object — models sometimes prepend/append prose.
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < 0 || end < start {
		return Verdict{}, fmt.Errorf("parse verdict: no JSON object in response: %q", trimmed)
	}
	objText := trimmed[start : end+1]

	var v Verdict
	if err := json.Unmarshal([]byte(objText), &v); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict: %w", err)
	}
	if v.Class == "" {
		return Verdict{}, fmt.Errorf("parse verdict: missing class field")
	}
	if v.Reason == "" {
		return Verdict{}, fmt.Errorf("parse verdict: missing reason field")
	}
	if !validClasses[v.Class] {
		return Verdict{}, fmt.Errorf("parse verdict: unknown class %q", v.Class)
	}
	return v, nil
}
