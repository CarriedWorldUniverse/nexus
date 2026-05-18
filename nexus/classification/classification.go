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

import "os"

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
