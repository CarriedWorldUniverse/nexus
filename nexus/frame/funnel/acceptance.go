// Verified task_done — Unit B (NET-22/23/24, "verified task_done +
// respond-only completion"). Live evidence 2026-07-05: keel-builder called
// task_done with a confabulated summary ("0 conflicts, 100% memory match")
// and NEVER produced the required token — the Job "completed successfully"
// because task_done trusted the model's self-report unconditionally. This
// file adds a one-shot cheap-judge classification, run when the work item
// carries acceptance criteria, that checks the model's completion claim
// against what it actually reported before honoring task_done.
//
// Deliberately a SEPARATE type from CheapModelFilter rather than an
// overload of its four-class post/scratch/complete/blocked scheme:
// task_done is an explicit, one-time model CLAIM of completion (not an
// ordinary turn to classify for chat-worthiness), and the verdict shape
// (met/not-met against caller-supplied criteria text) doesn't fit the
// filter's Class enum. Mirrors CheapModelFilter.Judge's harness-call
// shape (one-shot TurnRequest, MaxSteps=1, bounded timeout, fail-open on
// harness/parse error) so the two stay easy to reason about side by side.
package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// acceptanceJudgeTimeout bounds a single verification call — same rationale
// as filterJudgeTimeout (CheapModelFilter): a hung judge must not hang the
// builder's task_done path indefinitely. The caller fails open past this.
const acceptanceJudgeTimeout = 30 * time.Second

// maxAcceptanceCriteriaLen / maxAcceptanceOutputLen bound the prompt cost,
// mirroring maxJudgeTriggerLen/maxJudgeCandidateLen in filter.go.
const (
	maxAcceptanceCriteriaLen = 4000
	maxAcceptanceOutputLen   = 4000
)

// acceptanceJudgePrompt instructs the cheap model to verify — skeptically —
// a builder's completion claim against the work item's acceptance criteria.
// Reply format is a single JSON object: {"met": bool, "reason": "..."}.
//
// The explicit skepticism instruction is the point of this whole unit: NET-24
// showed a model self-reporting success ("0 conflicts, 100% memory match")
// having produced nothing that meets the DoD. The judge's job is to catch
// that gap, not to rubber-stamp the agent's own narrative.
const acceptanceJudgePrompt = `You are verifying whether an AI agent's claimed task completion actually satisfies the stated acceptance criteria.

Respond with ONLY valid JSON (no markdown, no explanation):
{"met": true|false, "reason": "one short phrase"}

Be skeptical of the agent's own self-report — it may be inaccurate, incomplete, or confabulated (an agent has been observed to claim success in vivid detail without ever producing the required output). Judge ONLY against the acceptance criteria provided and the agent's reported output:

- If the criteria name a specific required artifact, token, or string, and it is not present verbatim (or clearly produced) in the reported output, "met" MUST be false.
- If the criteria are satisfied by what the agent reported, "met" is true.
- When genuinely ambiguous (the criteria are satisfied by the report but you cannot independently confirm), prefer true — this check is a backstop against confabulation, not a re-run of the whole task; do not invent stricter requirements than the criteria state.

Respond with ONLY the JSON object.`

// AcceptanceVerdict is AcceptanceVerifier.Verify's classification result.
type AcceptanceVerdict struct {
	Met    bool
	Reason string
}

// AcceptanceVerifier runs the one-shot cheap-judge call described above.
// Constructed by judge.BuildAcceptanceVerifier (mirrors judge.BuildFilter's
// provider/model resolution) — this type just owns the RunTurn mechanics.
type AcceptanceVerifier struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string

	// AspectID identifies the builder being judged, for attribution in the
	// TurnRequest and the Logger/ObservabilityHook lines below — mirrors
	// FilterInput.AspectID in filter.go.
	AspectID string

	// AspectHome/ProviderEnv/EnforceJSONSchema mirror the identically named
	// CheapModelFilter fields — see filter.go for the rationale (Cwd
	// anchoring, judge-credential overlay, OpenAI-strict-mode opt-in).
	AspectHome        string
	ProviderEnv       map[string]string
	EnforceJSONSchema bool

	Logger            *slog.Logger
	ObservabilityHook ObservabilityHook
}

// Verify classifies whether output (the model's task_done summary, plus
// whatever else the caller wants judged) satisfies criteria (the work
// item's acceptance criteria, formatted as text). Returns an error only on
// a genuine judge failure (harness error, unparseable verdict) — the
// caller (builderOnTaskDone) is responsible for the fail-open contract
// (met=true, i.e. honor task_done) on error; Verify itself never guesses.
func (v *AcceptanceVerifier) Verify(parent context.Context, criteria, output string) (AcceptanceVerdict, error) {
	if v == nil || v.Harness == nil || v.Provider == "" || v.Model == "" {
		return AcceptanceVerdict{}, errors.New("acceptance verifier not configured")
	}
	criteria = truncate(strings.TrimSpace(criteria), maxAcceptanceCriteriaLen)
	output = truncate(strings.TrimSpace(output), maxAcceptanceOutputLen)

	ctx, cancel := context.WithTimeout(parent, acceptanceJudgeTimeout)
	defer cancel()

	msg := "ACCEPTANCE CRITERIA:\n" + criteria + "\n\nAGENT'S REPORTED COMPLETION:\n" + output
	temperature := judgeTemperature
	req := bridle.TurnRequest{
		AspectID:           v.AspectID,
		AppendSystemPrompt: acceptanceJudgePrompt,
		UserMessage:        msg,
		Provider:           v.Provider,
		Model:              v.Model,
		MaxSteps:           1, // pure text; no tools
		Cwd:                v.AspectHome,
		ProviderEnv:        v.ProviderEnv,
		Temperature:        &temperature,
		MaxOutputTokens:    judgeMaxOutputTokens,
		ResponseFormat:     judgeResponseFormat(v.EnforceJSONSchema),
	}

	turnID := "acceptance-verify"
	if v.ObservabilityHook != nil {
		v.ObservabilityHook.BeginTurn(turnID, "acceptance-judge", v.Model, string(v.Provider), 0)
	}
	sink := turnSink(v.ObservabilityHook)
	result, err := v.Harness.RunTurn(ctx, req, NullRunner{}, sink)
	if v.ObservabilityHook != nil {
		v.ObservabilityHook.EndTurn()
	}
	if err != nil {
		return AcceptanceVerdict{}, fmt.Errorf("acceptance verifier: harness: %w", err)
	}

	verdict, perr := parseAcceptanceJSON(result.FinalText)
	if v.Logger != nil {
		v.Logger.Info("acceptance verifier decision",
			"met", verdict.Met, "reason", verdict.Reason, "judge_raw", result.FinalText, "parse_err", perr)
	}
	if perr != nil {
		return AcceptanceVerdict{}, fmt.Errorf("acceptance verifier: parse: %w", perr)
	}
	return verdict, nil
}

// parseAcceptanceJSON parses the judge's {"met": bool, "reason": string}
// reply, tolerating fenced-code wrappers and surrounding prose exactly like
// parseJudgeJSON.
func parseAcceptanceJSON(raw string) (AcceptanceVerdict, error) {
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
		return AcceptanceVerdict{}, fmt.Errorf("no JSON object in response: %q", trimmed)
	}
	objText := trimmed[start : end+1]

	var v struct {
		Met    *bool  `json:"met"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(objText), &v); err != nil {
		return AcceptanceVerdict{}, fmt.Errorf("json unmarshal: %w (text=%q)", err, objText)
	}
	if v.Met == nil {
		return AcceptanceVerdict{}, fmt.Errorf("missing met field: %s", objText)
	}
	reason := strings.TrimSpace(v.Reason)
	if len(reason) > 200 {
		reason = reason[:200] + "…"
	}
	return AcceptanceVerdict{Met: *v.Met, Reason: reason}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
