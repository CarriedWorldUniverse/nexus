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
// maxAcceptanceDiffLen bounds the ACTUAL PR DIFF section (Unit 1 — judge the
// diff, not the narrative); maxAcceptanceJudgeInputLen is the combined ceiling
// Verify truncates its whole output arg to, sized to fit report + delimiter +
// diff so the diff is never silently cut away. The judge is ornith-judge
// (local, free) so a larger prompt is cheap.
const (
	maxAcceptanceCriteriaLen   = 4000
	maxAcceptanceOutputLen     = 4000
	maxAcceptanceDiffLen       = 12000
	maxAcceptanceJudgeInputLen = 18000
)

// acceptanceDiffHeader delimits the authoritative-diff section that
// AugmentOutputWithDiff appends and acceptanceJudgePrompt tells the judge to
// treat as ground truth. The two MUST agree on this exact string.
const acceptanceDiffHeader = "=== ACTUAL PR DIFF (ground truth) ==="

// AugmentOutputWithDiff builds the judge input for Unit 1: the agent's
// reported completion (context) followed by the authoritative unified diff of
// its PR. Each part is capped independently so neither starves the other and
// the total stays under maxAcceptanceJudgeInputLen. Callers pass the raw diff;
// an empty diff returns the report unchanged (nothing to augment).
func AugmentOutputWithDiff(report, diff string) string {
	diff = strings.TrimSpace(diff)
	if diff == "" {
		return report
	}
	report = truncate(strings.TrimSpace(report), maxAcceptanceOutputLen)
	diff = truncate(diff, maxAcceptanceDiffLen)
	return report + "\n\n" + acceptanceDiffHeader + "\n" + diff
}

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

Be skeptical of the agent's own self-report — it may be inaccurate, incomplete, or confabulated (an agent has been observed to claim success in vivid detail without ever producing the required output).

If the input contains a section headed "=== ACTUAL PR DIFF (ground truth) ===", that unified diff is AUTHORITATIVE — judge the acceptance criteria against the DIFF, treating the agent's narrative above it as context only. A change the criteria require MUST actually appear in the diff; if it does not, "met" MUST be false, no matter what the narrative claims. If NO such diff section is present, judge against the agent's reported output as below.

Judge ONLY against the acceptance criteria provided and the evidence (diff when present, else the reported output):

- If the criteria name a specific required artifact, token, or string, and it is not present verbatim (or clearly produced) in the evidence, "met" MUST be false.
- If the criteria are satisfied by the evidence, "met" is true.
- When genuinely ambiguous (the criteria are satisfied by the evidence but you cannot independently confirm), prefer true — this check is a backstop against confabulation, not a re-run of the whole task; do not invent stricter requirements than the criteria state.

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
	// Unit 1: output may carry an appended authoritative-diff section
	// (AugmentOutputWithDiff), so truncate to the combined ceiling, not the
	// report-only cap — otherwise the diff would be cut off before the judge
	// sees it. Report-only inputs are well under this and unaffected.
	output = truncate(strings.TrimSpace(output), maxAcceptanceJudgeInputLen)

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
