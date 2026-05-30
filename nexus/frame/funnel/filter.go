// Post-hoc output filter — runs after each turn's natural reply to
// decide whether it's substantive enough to post. Per Lock 1.3 / Lock 3
// of the aspect-funnel architecture
// (agent-network/docs/2026-05-02-aspect-funnel-architecture.md).
//
// Why post-hoc and not pre-turn:
//
// Aspects always run a turn — Lock 2's Nexus-side routing already
// gated whether the turn happens at all (only addressed messages
// reach the aspect). The filter's job is narrower: did the model
// produce meaningful output for chat, or did it ramble, leak
// thinking, emit empty content, or decide "this isn't for me"?
//
// Per-turn granularity (operator #9147 Q1): each turn's natural
// final reply is judged independently. Mid-turn send_chat calls
// (Lock 3) are intentional and authoritative — the filter does NOT
// rejudge them. The filter only governs the implicit final reply.
//
// Replaces today's agent-network harness #72 fix in the rebuild.

package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

// ExpandBareClaudeTier maps a bare Claude tier ("haiku"/"sonnet"/"opus")
// to a full Anthropic model id for the NATIVE claude-api path. claude-code
// is left untouched — its CLI accepts (and expects) the bare shorthand.
// Non-tier strings and non-Claude providers pass through unchanged.
//
// NEX-369: a bare "haiku" is not a valid Anthropic SDK model id, so a
// native cheap-judge configured with it 404s → degrades + fails open
// (stops filtering). Both the Frame and agentfunnel default the Claude
// judge to "haiku", so both route through here. (Shared so the two
// runtimes can't drift — a step toward the NEX-365 unification.)
func ExpandBareClaudeTier(model string, providerID bridle.ProviderID) string {
	// Only the native Anthropic SDK path needs expansion. claude-code's CLI
	// resolves the shorthand itself; non-Claude providers (openai/…) have no
	// such tiers, so leave them untouched.
	switch providerID {
	case "claude-api", "claude":
	default:
		return model
	}
	switch model {
	case "haiku":
		return "claude-haiku-4-5-20251001"
	case "sonnet":
		return "claude-sonnet-4-6"
	case "opus":
		return "claude-opus-4-8"
	default:
		return model
	}
}

// judgeVerdictSchema is the strict-JSON-schema enforced on cheap-
// judge responses when EnforceJSONSchema is opted in (NEX-300 +
// NEX-297 L2 finding). On providers that support OpenAI's structured
// outputs (real api.openai.com), the model is GUARANTEED to emit a
// payload matching this shape — NEX-292's parse_failure path becomes
// effectively unreachable.
//
// IMPORTANT: NOT portable across "OpenAI-compatible" third-party
// endpoints. NEX-297 L2 live-verified 2026-05-26 that DeepSeek's
// /v1 returns 400 "This response_format type is unavailable now"
// when this strict variant is sent — the looser json_object IS
// supported. So strict mode is opt-in (EnforceJSONSchema=true on the
// filter) for verified-capable providers; default is json_object
// which works on both OpenAI and DeepSeek.
//
// Providers that ignore response_format entirely (Anthropic Messages
// API, claude-code subprocess) fall through to the existing
// parseJudgeJSON path, which tolerates both the four-class shape and
// the legacy {post: bool, reason: string} from older judge models.
//
// Constraints required by OpenAI strict mode:
//   - additionalProperties must be false
//   - every property must appear in required
//   - enum bounds the class to the four NEX-210 verdict labels
const judgeVerdictSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "class":  {"type": "string", "enum": ["complete", "scratch", "goal_not_met", "blocked"]},
    "reason": {"type": "string"}
  },
  "required": ["class", "reason"]
}`

// judgeMaxOutputTokens caps cheap-judge response length. Verdicts
// are tiny JSON objects (well under 100 output tokens in practice);
// 150 leaves headroom for unusually long "reason" strings without
// letting a runaway model exhaust the cheap-tier budget. NEX-300.
const judgeMaxOutputTokens = 150

// judgeResponseFormat returns the response_format spec for the judge
// turn. NEX-297 L2 + NEX-300 follow-up:
//
//   strict=false (default): json_object — guarantees the model
//     returns valid JSON but doesn't enforce shape. Portable across
//     OpenAI AND DeepSeek /v1 AND OpenAI-compatible third-party
//     endpoints. The prompt + parseJudgeJSON tolerance handle shape.
//
//   strict=true (operator opt-in via EnforceJSONSchema): json_schema
//     with judgeVerdictSchema and Strict=true. Model is GUARANTEED to
//     emit a payload matching the schema. NEX-292's parse_failure
//     path becomes effectively unreachable. Only safe on verified-
//     capable providers (real api.openai.com); flagged off-by-default
//     because DeepSeek /v1 rejects this variant with 400 "type
//     unavailable" and every judge call would error.
func judgeResponseFormat(strict bool) *bridle.ResponseFormat {
	if strict {
		return &bridle.ResponseFormat{
			Type:        "json_schema",
			Name:        "judge_verdict",
			Description: "Cheap-judge classifier verdict for a candidate aspect reply.",
			Strict:      true,
			Schema:      json.RawMessage(judgeVerdictSchema),
		}
	}
	return &bridle.ResponseFormat{Type: "json_object"}
}

// judgeTemperature is 0 so the cheap-judge produces deterministic
// verdicts for the same input. Classifier work, not creative —
// determinism aids cross-aspect consistency (NEX-294's motivation)
// + makes "why did the filter suppress X" debuggable.
const judgeTemperature = 0.0

// OutputFilter judges a turn's natural final reply. Returns shouldPost
// true if the reply is substantive enough to surface in chat, false to
// suppress. Reason is a short string for telemetry; ignored when
// shouldPost is true.
//
// Implementations MUST be safe to call from any goroutine.
//
// Implementations MUST respect ctx — Judge runs inside a goroutine
// that the funnel's runFilter races against ctx.Done(). If Judge
// ignores ctx and the parent cancels, the goroutine leaks for the
// remaining lifetime of whatever resources Judge holds. CheapModelFilter
// satisfies this via context.WithTimeout around the bridle call; custom
// filters MUST do the same.
type OutputFilter interface {
	Judge(ctx context.Context, in FilterInput) FilterDecision
}

// FilterInput carries everything an OutputFilter needs to decide.
// Kept small and provider-agnostic so the same shape works whether
// the filter is a regex, a heuristic, or a model call.
type FilterInput struct {
	// FinalText is the model's natural reply at end-of-turn — the
	// candidate post.
	FinalText string

	// AspectID identifies which aspect produced the output. Useful
	// for filters with per-aspect calibration (e.g. forge's filter
	// can be looser since training reports legitimately contain
	// metric numbers that look like noise).
	AspectID string

	// TurnID joins to the Lock 5 lifecycle events for this turn so
	// telemetry can correlate filter decisions with turn outcomes.
	TurnID string

	// TriggerFrom is the aspect identity of the message that triggered
	// this deliberation, when known. Empty for autonomous turns (no
	// inbound message). Used by the cheap-judge to:
	//   1. Skip the judge entirely when from=="operator" — operator
	//      @-mentions are load-bearing; ghost-silence is worse than a
	//      thin reply (operator-bypass, lifted from agent-network
	//      #10039/#10040).
	//   2. Show the judge what the candidate is replying TO, so short
	//      substantive replies don't read as scratch in isolation.
	TriggerFrom string

	// TriggerText is the content of the triggering message. Bounded
	// at the judge boundary to keep prompt cost predictable.
	TriggerText string

	// TriggerMsgID is the chat msg_id that triggered the deliberation,
	// or 0 for autonomous turns. Forwarded to the judge subprocess's
	// BeginTurn so the observability hub can correlate the judge tile
	// back to its originating chat message — without it, judge tiles
	// orphan in the activity stream.
	TriggerMsgID int64

	// DoD is the Definition of Done for the current ticket, when the
	// turn is part of a goal-pursuit loop (NEX-210). The judge prompt
	// receives this as additional context and compares the turn's
	// artifacts against the DoD criteria. Empty when no goal is active
	// — the judge operates in legacy binary post/suppress mode.
	DoD string

	// PriorTurnFinalText is the natural reply from the previous turn
	// in a goal-pursuit loop (NEX-249). Provided so the judge can
	// detect "looping, same output as prior turn, no new artifacts"
	// (its prompt's blocked criterion). Without it the judge sees one
	// turn at a time and can't catch repetition; the prefer-continue
	// bias then keeps returning goal_not_met until the MaxTurns cap.
	// Empty on the first goal-loop turn (no prior) and on turns
	// outside any goal-pursuit.
	PriorTurnFinalText string

	// ToolNames lists the tool names the model invoked during this
	// turn (provider-order). Empty for text-only turns. The judge uses
	// this to distinguish "thin text but real work happened via tools"
	// (post-as-complete) from "thin text and no work" (scratch). Pre-
	// existing behavior judged solely on FinalText and routinely
	// labeled tool-effecting turns as scratch when the model emitted
	// only a terse confirmation.
	//
	// Just names, not args/results — the judge doesn't need the
	// payload; it needs the signal that tool calls happened.
	ToolNames []string

	// Partial signals the FinalText is a recovered partial — the
	// underlying turn errored mid-stream (claudecode StopReasonProcessExit,
	// stream timeout, etc.) and the harness preserved what the model
	// said before the failure. Filters MAY use this to lean toward
	// posting (the user has been waiting; suppressing a half-formed
	// answer because of a transport error is worse than letting it
	// through). CheapModelFilter mentions this in the user message so
	// the cheap judge can apply the same lean.
	Partial bool

	// ThreadTail is the recent posts in the thread this reply lands in,
	// oldest-first, each pre-formatted as "from: content". It exists so
	// the judge can match INTENT across the thread rather than judging
	// the candidate in isolation: a varied-but-content-free reply
	// ("amazing, congrats! 🎉") reads as substantive on its own, but as
	// the Nth entry in an acknowledgement loop it's churn the judge
	// should suppress. Because the filter is post-hoc, a scratch verdict
	// means no post → the other aspects never re-trigger → the @all /
	// broadcast fan-out self-extinguishes. This is why the AI judge
	// exists (intent, not text-match); Tier-1 damping only catches
	// VERBATIM repeats and is blind to varied churn.
	//
	// Bounded by buildJudgeUserMessage (last loopTailMaxLines, each line
	// capped) so a long thread can't blow the judge prompt. Empty for
	// turns with no thread context (autonomous, or runtimes that don't
	// plumb it yet — the agentfunnel path is a follow-up).
	ThreadTail []string
}

// FilterDecision is the result of Judge.
type FilterDecision struct {
	// ShouldPost true: post the FinalText to chat. False: suppress.
	// Derived from Class for cheap-model filters; set directly by
	// hard-rules and always-post filters.
	ShouldPost bool

	// Reason is a short, machine-readable label for telemetry. For
	// allowed posts, conventionally empty. For suppressions, one of
	// the FilterReason* constants below or a free-form reason from
	// custom filters.
	Reason string

	// Class is the four-class verdict (NEX-210). Empty for legacy
	// filters that predate the classification expansion — callers
	// should treat empty-Class as "complete" when ShouldPost and
	// "scratch" when !ShouldPost.
	Class string

	// SystemNotice, when non-empty, instructs the funnel's return
	// handler to post a separate system-author message to the thread
	// (in addition to any normal reply driven by ShouldPost). Used by
	// CheapModelFilter to surface judge-degradation entry/exit in-band
	// so failures aren't silent. Empty = no notice. NEX-292.
	SystemNotice string
}

// FilterClass values for the four-class judge (NEX-210).
const (
	FilterClassComplete   = "complete"     // DoD met, work done
	FilterClassScratch    = "scratch"      // non-substantive output
	FilterClassGoalNotMet = "goal_not_met" // substantive but DoD not yet satisfied
	FilterClassBlocked    = "blocked"      // aspect says blocked or no progress visible
)

// FilterReason* are canonical suppression labels. New filters should
// use these where they fit and only invent new strings for genuinely
// novel cases.
const (
	FilterReasonEmpty        = "empty_output"
	FilterReasonSelfSuppress = "self_suppress" // model said "I don't have anything to add"
	FilterReasonScratch      = "scratch"       // looks like internal thinking, not a reply
	FilterReasonRamble       = "ramble"        // long, no actual content
)

// AlwaysPostFilter posts every non-empty reply unmodified. Default
// when no filter is configured — matches the v1 §6.5 Frame harness
// behavior (no triage). Used in tests and as the safe fallback.
type AlwaysPostFilter struct{}

// Judge returns ShouldPost=true unless FinalText is empty/whitespace.
// Even the trivial empty check is worth doing — provider quirks
// occasionally return zero-content responses, and posting a blank
// line to chat looks broken.
func (AlwaysPostFilter) Judge(_ context.Context, in FilterInput) FilterDecision {
	if strings.TrimSpace(in.FinalText) == "" {
		return FilterDecision{ShouldPost: false, Reason: FilterReasonEmpty}
	}
	return FilterDecision{ShouldPost: true}
}

// HardRulesFilter applies a small set of regex/string rules before
// any model call. Catches the obvious self-suppress cases ("I don't
// have anything to add", "this isn't for me", etc.) cheaply. Falls
// back to a wrapped filter for ambiguous cases.
//
// Why this layer exists: in agent-network harness, the model often
// emits self-suppress phrases when it engages on a chat that
// addresses someone else. Catching those at the regex layer saves
// a cheap-model call per turn — meaningful at scale.
type HardRulesFilter struct {
	// Inner runs when no hard rule matches. nil falls back to
	// AlwaysPostFilter.
	Inner OutputFilter
}

// selfSuppressSubstrings are unambiguous self-suppress phrases — a
// substantive reply would not plausibly contain these strings even
// in the middle of useful content. Substring match is safe.
//
// Kept narrow on purpose. The temptation to add every new shape the
// model invents ("not me. silence.", "addressed to @X, not me", etc)
// is whack-a-mole — every novel phrasing slips through and we
// re-patch. The cheap-model judge in filterJudgePrompt is the
// off-cycle AI evaluator that's supposed to handle the
// variable-phrasing class. New self-suppress shapes belong in the
// judge prompt's example list, not here.
var selfSuppressSubstrings = []string{
	"i don't have anything to add",
	"this isn't for me",
	"this message isn't addressed to me",
}

// selfSuppressPrefixes are phrases that are only self-suppress when
// they OPEN the reply. As substring matches they would catch
// legitimate substantive replies (e.g. "after running this audit
// I'll stay quiet on the security implications until we discuss" is
// real content, not a non-reply). Anchoring to the first chars
// avoids that false positive.
var selfSuppressPrefixes = []string{
	"i'll stay quiet",
	"i'll remain quiet",
	"nothing to add here",
}

// Judge runs hard rules; on no match, delegates to Inner.
func (f HardRulesFilter) Judge(ctx context.Context, in FilterInput) FilterDecision {
	trimmed := strings.TrimSpace(in.FinalText)
	if trimmed == "" {
		return FilterDecision{ShouldPost: false, Reason: FilterReasonEmpty}
	}
	lowered := strings.ToLower(trimmed)
	for _, phrase := range selfSuppressSubstrings {
		if strings.Contains(lowered, phrase) {
			return FilterDecision{ShouldPost: false, Reason: FilterReasonSelfSuppress}
		}
	}
	for _, phrase := range selfSuppressPrefixes {
		if strings.HasPrefix(lowered, phrase) {
			return FilterDecision{ShouldPost: false, Reason: FilterReasonSelfSuppress}
		}
	}
	if f.Inner != nil {
		return f.Inner.Judge(ctx, in)
	}
	return AlwaysPostFilter{}.Judge(ctx, in)
}

// CheapModelFilter calls a small/fast model via bridle to judge
// whether a reply is substantive. Wrap inside HardRulesFilter so
// obvious cases never reach the model.
//
// The model is asked a single yes/no question; the filter parses the
// response. Bounded by ctx deadline (filterJudgeTimeout); if the
// model errors or times out, the filter defaults to ShouldPost=true
// — failing open is the right shape, since a broken filter
// suppressing real content is worse than a noisy filter occasionally
// letting a thin reply through.
type CheapModelFilter struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string

	// AspectHome is the per-aspect home dir passed as bridle's
	// TurnRequest.Cwd. The judge runs claude-code subprocess turns and
	// inherits the same identity-collision risk the main funnel turn
	// does — without this, the judge subprocess discovers .mcp.json
	// from whatever cwd nexus.exe was launched with. Operator decision
	// (#239): filter judge inherits the same Cwd as the main turn so
	// both subprocesses anchor at the same aspect home.
	AspectHome string

	// Logger is optional; when set, every Judge() call emits an INFO
	// log with the input preview + judge raw output + decision. Pair
	// the input text against the model's verdict for post-hoc analysis
	// ("why did the filter let X through"). nil = silent.
	Logger *slog.Logger

	// ObservabilityHook, when set, surfaces the judge turn under the
	// "filter-judge" label so dashboards can show what the filter
	// actually saw. Nil leaves the judge invisible to observability.
	// Mirrors funnel.Config.ObservabilityHook — pass the same
	// Hub.GrouperFor(aspectID) for the aspect being judged.
	ObservabilityHook ObservabilityHook

	// ProviderEnv overlays env vars onto each judge TurnRequest —
	// e.g. ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL pointing at a
	// cheap-tier endpoint (DeepSeek's Anthropic-compatible API)
	// so the bare claude-code subprocess routes the classifier
	// call to a separate auth domain from the main turn. Empty
	// map / nil = inherit ambient process env (subscription auth).
	//
	// Pre-NEX-103 (per-kind credential dispatch through
	// ProviderEnvResolver): the filter is statically configured at
	// construction time. Resolver-driven per-call dispatch lands
	// when NEX-103's schema migration lands.
	ProviderEnv map[string]string

	// EnforceJSONSchema opts the judge into OpenAI's strict structured-
	// outputs mode (response_format=json_schema with Strict=true and
	// judgeVerdictSchema). Only safe on providers that support the
	// strict variant — verified working on real api.openai.com;
	// verified NOT supported on DeepSeek's /v1 (returns 400 "type
	// unavailable", live-tested NEX-297 L2 2026-05-26).
	//
	// Default (false) sends response_format=json_object — looser but
	// portable. Model must return valid JSON; shape conformance falls
	// to parseJudgeJSON tolerance + the explicit prompt instructions.
	// This is the safe default that works on both OpenAI and DeepSeek
	// /v1; flip true per-aspect or network-wide when operator knows
	// the configured provider supports strict mode.
	EnforceJSONSchema bool

	// DegradedCooldown bounds fail-open posts during periods when the
	// judge subprocess can't return a verdict (harness error: crash,
	// auth failure, network blip, timeout). Inside the cooldown window
	// after the last fail-open post, subsequent judge errors return
	// ShouldPost=false — preventing the cascade pattern observed
	// 2026-05-25 where a broken judge + unconditional fail-open + the
	// multi-aspect echo topology produced exponential reply storms.
	//
	// Zero = default 30s. Negative = no rate limit (legacy unconditional
	// fail-open; safe only for single-aspect deployments). Verdict-path
	// (judge returned a yes/no) is never rate-limited — only the
	// harness-error path engages this. NEX-292.
	DegradedCooldown time.Duration

	// degradedMu guards degraded / lastDegradedPost. Per-aspect state
	// because a single CheapModelFilter instance can in principle be
	// shared across aspects (today it's per-aspect, but the map keyed
	// by AspectID keeps the contract correct either way).
	degradedMu sync.Mutex
	// degraded[aspect] = true means we've already posted the entry
	// system notice for this degradation window. Cleared on first
	// successful verdict after the window.
	degraded map[string]bool
	// lastDegradedPost[aspect] = time of last fail-open post during
	// degradation. Used for cooldown rate-limit math.
	lastDegradedPost map[string]time.Time
}

// defaultDegradedCooldown is applied when DegradedCooldown is zero.
const defaultDegradedCooldown = 30 * time.Second

// filterJudgeTimeout is a safety bound for a fully hung judge —
// "model is broken / network gone" backstop, not a "make it snappy"
// budget. The outer runFilter waits without timing out (the judge
// IS the authority on whether to post), so this only fires when
// something pathological is going on. 30s is way past any normal
// cold-start; if we hit it, log the WARN and fall back to posting.
const filterJudgeTimeout = 30 * time.Second

// filterJudgePrompt is the system prompt for the cheap-model filter.
// Reply format is a single JSON object:
//   {"class": "complete" | "scratch" | "goal_not_met" | "blocked", "reason": "one short phrase"}
//
// The four-class format (NEX-210) extends the prior binary {post, reason}
// shape. When DoD context is present in the user message, the judge
// compares the turn's artifacts against DoD criteria to distinguish
// complete from goal_not_met. Without DoD, the judge falls back to
// binary behavior: complete when substantive, scratch when not.
//
// Backward compat: the parser still accepts the old {"post": bool} format
// from judge models that haven't picked up the new prompt yet.
//
// Symmetric treatment: operator messages are NOT bypassed. Operator is
// just another peer for filter purposes.
const filterJudgePrompt = `You are a turn classifier for an AI agent in a group chat. Classify each turn into one of four categories.

Respond with ONLY valid JSON (no markdown, no explanation):
{"class": "complete" | "scratch" | "goal_not_met" | "blocked", "reason": "one short phrase"}

CLASS DEFINITIONS:

"complete" — The candidate output is substantive chat content AND any stated Definition of Done is satisfied. The work is finished. Post to chat.

"scratch" — The candidate output is NOT meaningful chat content. Examples:
- The agent said "this isn't for me, I'll stay quiet" or "no response needed"
- Raw JSON / internal reasoning / classification format that leaked
- Empty, whitespace-only, or near-empty
- Tool-call trace or internal protocol output that escaped
- LOOP CONTINUATION: when a RECENT THREAD section is provided and shows the
  thread is a low-signal acknowledgement/agreement/celebration loop — the
  recent posts are congratulations, "great work", "amazing", emoji reactions,
  "confirmed, we're live" restated, or other content that adds no NEW
  information, question, decision, or action — and the candidate is just
  another such reply, classify it "scratch" EVEN IF the candidate reads as
  substantive on its own. Judge the thread's intent, not the single line:
  a varied wording of "nice job team!" is still loop churn. What BREAKS the
  loop (and earns "complete"): genuinely new information, a real question, a
  decision, a concrete next action, or a correction. When unsure whether a
  reply adds something new versus echoes the loop, prefer "scratch" — the
  echo re-triggers every other participant and a broadcast storm is far worse
  than one suppressed pleasantry.
Do NOT post to chat.

"goal_not_met" — The candidate output IS substantive chat content, AND a Definition of Done was provided, AND the DoD is NOT yet satisfied. The agent made progress but is not finished. Post to chat (the reply is real) but flag for continuation.

"blocked" — The agent explicitly says it is blocked, waiting on external input, or cannot make progress. OR the candidate shows zero forward progress against the DoD (same output as prior turn, looping, no new artifacts). Do NOT post to chat. Surface to operator.

TOOL USAGE: when the TOOLS USED section lists one or more tool calls AND the candidate output is thin (a terse confirmation like "done", "ok", "updated X"), prefer "complete" — substantive work happened via tool effects; the terse confirmation IS the appropriate chat reply. Reserve "scratch" for thin text with NO tool work.

PARTIAL RESULTS: when the candidate is marked PARTIAL, the underlying turn errored mid-stream (transport issue, output cap). Lean toward "complete" if the partial is coherent; the user has been waiting and a half-formed answer is better than silence after a stream failure. Only flag as "scratch" if the partial is genuinely unusable (truncated mid-token, fragments of internal reasoning).

BIAS: when uncertain between complete and goal_not_met, prefer goal_not_met (safer to continue than to stop early). When uncertain between scratch and blocked, prefer scratch. When DoD is absent, never return goal_not_met — use complete for substantive, scratch for non-substantive.

Respond with ONLY the JSON object.`

// maxJudgeTriggerLen / maxJudgeCandidateLen bound the strings the judge
// sees, mirroring agent-network/code/harness/index.js:501,504. Predictable
// judge cost: ~1.2k inbound + ~4k candidate. Long candidate replies
// aren't more meaningful — they're more likely the rambling shape the
// classifier should catch on the FIRST 4k chars anyway.
const (
	maxJudgeTriggerLen   = 1200
	maxJudgeCandidateLen = 4000
)

// loopTailMaxLines / loopTailMaxLineLen bound the RECENT THREAD section.
// The judge only needs enough of the tail to recognise a churn pattern —
// the last few posts are sufficient ("are these all content-free
// acknowledgements?"). Cap line length so one pathological post can't
// dominate the prompt; the loop signal lives in the SHAPE of the recent
// exchange, not the full text of any single line.
const (
	loopTailMaxLines   = 8
	loopTailMaxLineLen = 280
)

// buildJudgeUserMessage assembles the per-call user message for the cheap
// judge. Includes the inbound trigger context (what the candidate is
// replying TO) so short substantive replies don't read as scratch in
// isolation. When no trigger is known (autonomous turn) says so
// explicitly so the model doesn't hallucinate one.
//
// The system prompt (filterJudgePrompt) tells the model the format —
// this is just the message body it judges.
func buildJudgeUserMessage(in FilterInput) string {
	triggerFrom := strings.TrimSpace(in.TriggerFrom)
	triggerText := strings.TrimSpace(in.TriggerText)
	if len(triggerText) > maxJudgeTriggerLen {
		triggerText = triggerText[:maxJudgeTriggerLen] + "…"
	}
	candidate := in.FinalText
	if len(candidate) > maxJudgeCandidateLen {
		candidate = candidate[:maxJudgeCandidateLen] + "…"
	}
	var triggerBlock string
	if triggerFrom == "" && triggerText == "" {
		triggerBlock = "(autonomous turn — no inbound trigger)"
	} else {
		triggerBlock = "From: @" + triggerFrom + "\nContent: " + triggerText
	}
	msg := "INCOMING MESSAGE:\n" + triggerBlock
	// NEX-210: when a Definition of Done is provided, include it so the
	// judge can compare turn artifacts against DoD criteria. Bounded to
	// keep prompt cost predictable.
	if dod := strings.TrimSpace(in.DoD); dod != "" {
		const maxDoDLen = 2000
		if len(dod) > maxDoDLen {
			dod = dod[:maxDoDLen] + "…"
		}
		msg += "\n\nDEFINITION OF DONE:\n" + dod
	}
	// NEX-249: prior turn's final text, when available. Surfaces the
	// repetition signal the judge prompt's "blocked" criterion already
	// names ("same output as prior turn, looping, no new artifacts").
	// Bounded — long candidates are already capped at maxJudgeCandidateLen
	// and the same shape applies here.
	if prior := strings.TrimSpace(in.PriorTurnFinalText); prior != "" {
		const maxPriorTurnLen = 2000
		if len(prior) > maxPriorTurnLen {
			prior = prior[:maxPriorTurnLen] + "…"
		}
		msg += "\n\nPRIOR TURN OUTPUT (the candidate below must show forward progress vs this; near-identical output is the blocked signal):\n" + prior
	}
	// Recent thread tail — lets the judge match churn INTENT across the
	// thread rather than judging the candidate in isolation. Bounded to
	// the last loopTailMaxLines, each line capped, so a long or
	// pathological thread can't blow the prompt. See FilterInput.ThreadTail
	// + the LOOP SUPPRESSION clause in filterJudgePrompt.
	if tail := boundedThreadTail(in.ThreadTail); len(tail) > 0 {
		msg += "\n\nRECENT THREAD (oldest first — judge whether this thread is a low-signal acknowledgement/agreement loop the candidate is merely continuing):\n" + strings.Join(tail, "\n")
	}
	// Surface tool usage so the judge can weight "thin text + real
	// work via tools" as complete rather than scratch. Bounded list
	// — 20 names is enough to convey the shape without bloating the
	// prompt; anything beyond gets summarized.
	if len(in.ToolNames) > 0 {
		const maxNames = 20
		names := in.ToolNames
		extra := 0
		if len(names) > maxNames {
			extra = len(names) - maxNames
			names = names[:maxNames]
		}
		toolList := strings.Join(names, ", ")
		if extra > 0 {
			toolList += fmt.Sprintf(" (+%d more)", extra)
		}
		msg += "\n\nTOOLS USED:\n" + toolList
	}
	// Partial marker — tell the judge the underlying turn errored
	// mid-stream so it can lean toward post for coherent partials.
	if in.Partial {
		msg += "\n\nPARTIAL: the turn errored mid-stream; this is the recovered partial output."
	}
	// Plain section labels, no leading dashes. claudecode's `-p <prompt>`
	// passes the prompt as a command-line argument; a leading "---" gets
	// parsed by the CLI argv parser as an unknown flag and the subprocess
	// exits 1 (`error: unknown option '--- INCOMING MESSAGE ---'`). The
	// CheapModelFilter then fails open and the filter is effectively
	// disabled. Surfaced 2026-05-12 by operator catching that the judge
	// hadn't actually run during the post-fix conversation — every
	// non-operator-triggered turn was failing open.
	msg += "\n\nCANDIDATE OUTPUT (from @" + in.AspectID + "):\n" + candidate
	return msg
}

// boundedThreadTail trims the thread tail to the last loopTailMaxLines
// entries and caps each line at loopTailMaxLineLen. Blank/whitespace-only
// lines are dropped. Returns nil when nothing survives so the caller can
// omit the section entirely. Keeps the LAST lines (most recent) because
// the churn signal is in how the thread is trending right now.
func boundedThreadTail(tail []string) []string {
	out := make([]string, 0, len(tail))
	for _, line := range tail {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > loopTailMaxLineLen {
			line = line[:loopTailMaxLineLen] + "…"
		}
		out = append(out, line)
	}
	if len(out) > loopTailMaxLines {
		out = out[len(out)-loopTailMaxLines:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Judge runs the cheap-model judgment. The deadline is enforced by a
// child context so a slow model call doesn't stall the deliberation
// past the filter's budget.
//
// No operator bypass. An earlier version (af690e9 → 80ff175) skipped
// the judge entirely when the triggering message was from "operator",
// reasoning that operator @-mentions are load-bearing and ghost-silence
// is worse than a thin reply. Two problems with that shape: (1) it gave
// operator-addressed turns a permanent free pass through the filter, so
// we never tested the calibration on the traffic the operator can see;
// (2) it MASKED a separate bug — the judge prompt's "---" separators
// were being parsed by claudecode's argv as an unknown flag, so every
// non-operator turn was silently failing open. Both surfaced 2026-05-12
// when the operator noticed "Not addressed to me — that's for keel-cli"
// posted via operator_bypass (should have been suppressed). The right
// shape: judge runs on every reply; if it gets calibration wrong, that's
// signal we ACT on, not signal we hide behind a bypass.
func (f *CheapModelFilter) Judge(parent context.Context, in FilterInput) FilterDecision {
	if strings.TrimSpace(in.FinalText) == "" {
		return FilterDecision{ShouldPost: false, Reason: FilterReasonEmpty}
	}
	if f.Harness == nil || f.Provider == "" || f.Model == "" {
		// Misconfigured — fail open rather than blocking the post.
		return FilterDecision{ShouldPost: true}
	}

	ctx, cancel := context.WithTimeout(parent, filterJudgeTimeout)
	defer cancel()

	// No session at all. The filter judge is a one-shot text classifier:
	// system prompt + one user message + one-token reply, no continuity
	// across calls. Previously we passed Session.ID = "filter-<turn_id>"
	// which the claude-code CLI rejected because --session-id must be a
	// valid UUID — every cheap-judge call exited 1, harness returned an
	// error, CheapModelFilter failed open, and the filter was effectively
	// disabled the entire time it was configured.
	//
	// Leaving SessionHandle zero-valued (no ID, no New flag) skips the
	// --session-id / --resume args entirely; claude-code creates its own
	// auto-generated session per call. Per-call sessions are slight waste
	// (we never resume any of them) but zero risk of UUID-validation
	// surprises and no UUID-gen dep needed. Surfaced 2026-05-12 by the
	// judge-decision logging — see task #186 for the discovery trail.
	// NEX-300: tighten the judge call with the standard provider knobs
	// bridle exposes (NEX-299 Pass 2). Temperature=0 makes the
	// classifier deterministic; MaxOutputTokens bounds runaway cost;
	// response_format guarantees the model returns valid JSON.
	//
	// ResponseFormat default is json_object (portable across OpenAI
	// AND DeepSeek /v1 — NEX-297 L2 finding 2026-05-26: DeepSeek
	// returns 400 for json_schema strict). Operators with verified-
	// capable providers (real api.openai.com) can opt into strict via
	// EnforceJSONSchema=true for the additional schema-shape guarantee.
	//
	// Providers that ignore response_format entirely (claude REST,
	// claude-code subprocess) silently ignore both variants — the
	// existing parseJudgeJSON fallback path still tolerates the
	// verdict shapes either way.
	temperature := judgeTemperature
	req := bridle.TurnRequest{
		AspectID:           in.AspectID,
		AppendSystemPrompt: filterJudgePrompt,
		UserMessage:        buildJudgeUserMessage(in),
		Provider:           f.Provider,
		Model:              f.Model,
		MaxSteps:           1, // pure text; no tools
		Cwd:                f.AspectHome,
		ProviderEnv:        f.ProviderEnv,
		Temperature:        &temperature,
		MaxOutputTokens:    judgeMaxOutputTokens,
		ResponseFormat:     judgeResponseFormat(f.EnforceJSONSchema),
	}

	// Phase E: bracket the judge turn under "filter-judge" label.
	// Uses the FilterInput.TurnID so observers can correlate the judge
	// to the main turn it's evaluating. Not deferred — close immediately
	// after RunTurn so the Grouper's terminal frame settles before any
	// downstream caller logic.
	if f.ObservabilityHook != nil {
		f.ObservabilityHook.BeginTurn(in.TurnID+"-judge", "filter-judge", f.Model, string(f.Provider), in.TriggerMsgID)
	}
	sink := turnSink(f.ObservabilityHook)
	result, err := f.Harness.RunTurn(ctx, req, NullRunner{}, sink)
	if f.ObservabilityHook != nil {
		f.ObservabilityHook.EndTurn()
	}
	if err != nil {
		return f.judgeErrorDecision(in.AspectID, in.TurnID, err)
	}

	// Parse JSON verdict from model. The prompt asks for a single JSON
	// object {"post": bool, "reason": string} with no markdown / no
	// explanation. Tolerate fenced-code wrappers and surrounding
	// whitespace; anything else falls through to fail-open with an
	// explicit parse_failure reason.
	decision, parseErr := parseJudgeJSON(result.FinalText)
	if parseErr != nil {
		// Fail open on parse failure — suppressing a real reply
		// because the cheap model emitted bad JSON is worse than
		// posting a thin one. Reason surfaces parse_failure so it's
		// visible in audit logs. NEX-292 deliberately does NOT
		// rate-limit parse_failure: a model that emits bad JSON
		// once probably emits good JSON next turn, and the cascade
		// scenario was specifically "judge can't respond at all".
		decision = FilterDecision{ShouldPost: true, Reason: "parse_failure"}
	} else {
		// Real verdict — clear degradation if we were in one and
		// emit a recovery notice in-band so the operator/users see
		// the loop close (NEX-292).
		if notice := f.clearDegradationIfRecovered(in.AspectID); notice != "" {
			decision.SystemNotice = notice
		}
	}

	// Always log the judge round-trip so post-hoc analysis can pair
	// the input text + trigger context against the model's verdict and
	// stated reason. Without this, "why did the filter suppress X"
	// requires reproducing the model call by hand. Truncate the input
	// to keep log lines readable; the FinalText raw answer is bounded
	// by MaxSteps:1 and short by construction so it logs in full.
	if f.Logger != nil {
		preview := in.FinalText
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		triggerPreview := in.TriggerText
		if len(triggerPreview) > 120 {
			triggerPreview = triggerPreview[:120] + "…"
		}
		f.Logger.Info("filter judge decision",
			"aspect", in.AspectID,
			"turn_id", in.TurnID,
			"should_post", decision.ShouldPost,
			"class", decision.Class,
			"reason", decision.Reason,
			"judge_raw", result.FinalText,
			"trigger_from", in.TriggerFrom,
			"trigger_preview", triggerPreview,
			"input_preview", preview)
	}
	return decision
}

// judgeErrorDecision implements NEX-292's rate-limited fail-open on
// harness errors (subprocess crash, auth failure, network blip,
// timeout). The first failure per cooldown window per aspect posts
// the reply (preserves pre-NEX-292 fail-open behaviour) and — on the
// first failure of a fresh degradation window — emits a SystemNotice
// for in-band visibility. Subsequent failures within the cooldown
// return ShouldPost=false (judge-degraded-rate-limited) so the
// cascade-amplification pattern observed 2026-05-25 can't recur:
// rate-limited replies have a hard ceiling regardless of inbound echo
// volume. The verdict path (judge returned a yes/no) clears the
// degradation marker and emits a recovery SystemNotice — see
// clearDegradationIfRecovered.
//
// DegradedCooldown == 0 → defaults to defaultDegradedCooldown (30s).
// DegradedCooldown < 0 → opt-out, unconditional fail-open (legacy
// behaviour; safe only for single-aspect deployments).
func (f *CheapModelFilter) judgeErrorDecision(aspect, turnID string, err error) FilterDecision {
	cooldown := f.DegradedCooldown
	if cooldown == 0 {
		cooldown = defaultDegradedCooldown
	}
	if cooldown < 0 {
		if f.Logger != nil {
			f.Logger.Warn("filter judge: harness error — failing open (rate-limit disabled)",
				"aspect", aspect, "turn_id", turnID, "err", err)
		}
		return FilterDecision{ShouldPost: true, Reason: "judge-degraded-fail-open"}
	}
	f.degradedMu.Lock()
	defer f.degradedMu.Unlock()
	if f.degraded == nil {
		f.degraded = map[string]bool{}
	}
	if f.lastDegradedPost == nil {
		f.lastDegradedPost = map[string]time.Time{}
	}
	now := time.Now()
	if last, ok := f.lastDegradedPost[aspect]; ok && now.Sub(last) < cooldown {
		if f.Logger != nil {
			f.Logger.Warn("filter judge: harness error — rate-limited (degraded)",
				"aspect", aspect, "turn_id", turnID,
				"cooldown_remaining", (cooldown - now.Sub(last)).String(),
				"err", err)
		}
		return FilterDecision{ShouldPost: false, Reason: "judge-degraded-rate-limited"}
	}
	// Allow this fail-open; mark cooldown floor and emit entry notice
	// on first failure of this degradation window.
	var notice string
	if !f.degraded[aspect] {
		notice = aspect + ": judge unavailable — replies rate-limited (1 per " + cooldown.String() + ")"
		f.degraded[aspect] = true
	}
	f.lastDegradedPost[aspect] = now
	if f.Logger != nil {
		f.Logger.Warn("filter judge: harness error — failing open (rate-limited)",
			"aspect", aspect, "turn_id", turnID,
			"cooldown", cooldown.String(),
			"first_in_window", notice != "",
			"err", err)
	}
	return FilterDecision{
		ShouldPost:   true,
		Reason:       "judge-degraded-fail-open",
		SystemNotice: notice,
	}
}

// clearDegradationIfRecovered clears the per-aspect degradation
// markers on first successful verdict after a fail-open window.
// Returns a recovery notice string when the aspect was previously
// degraded (so the funnel can post it in-band), or "" if no
// degradation was in flight.
func (f *CheapModelFilter) clearDegradationIfRecovered(aspect string) string {
	f.degradedMu.Lock()
	defer f.degradedMu.Unlock()
	if !f.degraded[aspect] {
		return ""
	}
	delete(f.degraded, aspect)
	delete(f.lastDegradedPost, aspect)
	return aspect + ": judge recovered"
}

// parseJudgeJSON extracts the verdict from the cheap-model's JSON
// response. Accepts two formats:
//
//   New (NEX-210): {"class": "complete"|"scratch"|"goal_not_met"|"blocked", "reason": "…"}
//   Legacy:        {"post": true|false, "reason": "…"}
//
// The new format is preferred. When both fields are present, class wins.
// When only post is present, the parser derives class from it (complete/scratch).
// Cheap models occasionally wrap in code fences or add surrounding
// whitespace; those are tolerated. Anything else fails so the caller can
// fail-open with parse_failure.
func parseJudgeJSON(raw string) (FilterDecision, error) {
	trimmed := strings.TrimSpace(raw)
	// Strip leading/trailing ``` or ```json fences if present.
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	// Locate the JSON object boundaries — providers sometimes prepend or
	// append prose despite the "ONLY JSON" instruction.
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < 0 || end < start {
		return FilterDecision{}, fmt.Errorf("no JSON object in response: %q", trimmed)
	}
	objText := trimmed[start : end+1]

	var verdict struct {
		Class  string `json:"class"`
		Post   *bool  `json:"post"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(objText), &verdict); err != nil {
		return FilterDecision{}, fmt.Errorf("json unmarshal: %w (text=%q)", err, objText)
	}

	reason := strings.TrimSpace(verdict.Reason)
	if len(reason) > 200 {
		reason = reason[:200] + "…"
	}

	// New four-class format (NEX-210): class field drives the decision.
	if verdict.Class != "" {
		class := verdict.Class
		var shouldPost bool
		switch class {
		case FilterClassComplete:
			shouldPost = true
		case FilterClassScratch:
			shouldPost = false
			if reason == "" {
				reason = FilterReasonScratch
			}
		case FilterClassGoalNotMet:
			shouldPost = true
			if reason == "" {
				reason = "goal_not_met"
			}
		case FilterClassBlocked:
			shouldPost = false
			if reason == "" {
				reason = "blocked"
			}
		default:
			// Unknown class — fail open. The model invented a class not
			// in the prompt; treat as parse failure so caller posts.
			return FilterDecision{}, fmt.Errorf("unknown class %q: %s", class, objText)
		}
		return FilterDecision{ShouldPost: shouldPost, Reason: reason, Class: class}, nil
	}

	// Legacy binary format: derive from post field.
	if verdict.Post == nil {
		return FilterDecision{}, fmt.Errorf("missing class and post fields: %q", objText)
	}
	if !*verdict.Post && reason == "" {
		reason = FilterReasonScratch
	}
	class := FilterClassComplete
	if !*verdict.Post {
		class = FilterClassScratch
	}
	return FilterDecision{ShouldPost: *verdict.Post, Reason: reason, Class: class}, nil
}
