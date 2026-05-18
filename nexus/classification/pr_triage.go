package classification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// PRTriageInput is the data needed to classify a PR.
type PRTriageInput struct {
	Diff          string // unified diff text (trimmed to maxPRDiffLen)
	ModelOverride string // per-call model override; empty = use env/default
}

// PRTriage classifies a GitHub PR diff. Follows the CheapModelFilter
// pattern: system prompt + user message → model → parse JSON verdict.
// Fails open (class=needs-review) on any error — missing a real PR is
// worse than flagging a trivial one.
type PRTriage struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string // default model; per-call model_override wins

	// Logger, when set, logs each classification with input preview +
	// model raw output + verdict for post-hoc audit.
	Logger *slog.Logger
}

// Classify runs the PR triage model and returns the verdict.
// Fails open: on any error, returns needs-review so the PR is not
// silently ignored.
func (c *PRTriage) Classify(ctx context.Context, in PRTriageInput) (Verdict, error) {
	diff := strings.TrimSpace(in.Diff)
	if diff == "" {
		return Verdict{}, fmt.Errorf("pr triage: empty diff")
	}
	if len(diff) > maxPRDiffLen {
		diff = diff[:maxPRDiffLen] + "…"
	}

	model := ResolveModel("NEXUS_PR_TRIAGE_MODEL", c.Model, in.ModelOverride)

	req := bridle.TurnRequest{
		AppendSystemPrompt: prTriageSystemPrompt,
		UserMessage:        buildPRTriageUserMessage(diff),
		Provider:           c.Provider,
		Model:              model,
		MaxSteps:           1,
	}

	result, err := c.Harness.RunTurn(ctx, req, nullRunner{}, discardSink{})
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("pr triage: harness error — failing open", "err", err)
		}
		return Verdict{Class: ClassNeedsReview, Reason: "classifier_error"}, nil
	}

	verdict, parseErr := ParseVerdict(result.FinalText)
	if parseErr != nil {
		if c.Logger != nil {
			c.Logger.Warn("pr triage: parse error — failing open",
				"raw", result.FinalText, "err", parseErr)
		}
		return Verdict{Class: ClassNeedsReview, Reason: "parse_failure"}, nil
	}

	if c.Logger != nil {
		preview := diff
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		c.Logger.Info("pr triage verdict",
			"class", verdict.Class,
			"reason", verdict.Reason,
			"model", model,
			"diff_preview", preview)
	}

	return verdict, nil
}

// maxPRDiffLen bounds the diff sent to the model for predictable cost.
// Mirrors filter.go's maxJudgeCandidateLen.
const maxPRDiffLen = 4000

// prTriageSystemPrompt is the classifier's system prompt.
const prTriageSystemPrompt = `You are a PR triage classifier. Given a git diff, classify it into one of three categories.

Respond with ONLY valid JSON (no markdown, no explanation):
{"class": "trivial"|"needs-review"|"suspicious", "reason": "one short phrase"}

CLASS DEFINITIONS:

"trivial" — The change is obviously safe and needs no review:
- Typo fixes, whitespace changes, comment edits
- Single-line changes in non-critical paths
- Config value tweaks with obvious intent
- Dependency version bumps in lockfiles only

"needs-review" — Normal PR that warrants reviewer attention:
- New function, type, or feature
- Logic changes in existing code
- Test additions or changes
- Documentation that introduces new information

"suspicious" — The change warrants operator attention:
- Large diff touching many files
- Changes to auth, credentials, or security-sensitive paths
- Hardcoded secrets, tokens, or keys
- Unusual patterns (committed .env files, binary files, large generated code)
- Changes that remove tests or safety checks
- Force-push or rewrite of git history

BIAS: when uncertain between trivial and needs-review, prefer needs-review (safer to have a human look). When uncertain between needs-review and suspicious, prefer suspicious. False negatives (missing a risky PR) are worse than false positives (flagging a safe one).

Respond with ONLY the JSON object.`

func buildPRTriageUserMessage(diff string) string {
	return "PR DIFF:\n" + diff
}

// nullRunner satisfies bridle.ToolRunner for classification-only turns
// where MaxSteps=1 and no tools are registered. Never called — the
// model receives no tool definitions so it can't request tool calls.
type nullRunner struct{}

func (nullRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage("{}"), nil
}

// discardSink drops all events. Used for classification-only turns
// where observability is handled by the Logger field.
type discardSink struct{}

func (discardSink) Emit(_ bridle.Event) {}
