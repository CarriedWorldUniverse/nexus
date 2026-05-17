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
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

// OutputFilter judges a turn's natural final reply. Returns shouldPost
// true if the reply is substantive enough to surface in chat, false to
// suppress. Reason is a short string for telemetry; ignored when
// shouldPost is true.
//
// Implementations MUST be safe to call from any goroutine. Slow
// implementations (e.g. cheap-bridle filter making a network call)
// must respect the ctx deadline; the funnel sets a bounded timeout
// per Judge.
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
}

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
Do NOT post to chat.

"goal_not_met" — The candidate output IS substantive chat content, AND a Definition of Done was provided, AND the DoD is NOT yet satisfied. The agent made progress but is not finished. Post to chat (the reply is real) but flag for continuation.

"blocked" — The agent explicitly says it is blocked, waiting on external input, or cannot make progress. OR the candidate shows zero forward progress against the DoD (same output as prior turn, looping, no new artifacts). Do NOT post to chat. Surface to operator.

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
func (f CheapModelFilter) Judge(parent context.Context, in FilterInput) FilterDecision {
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
	req := bridle.TurnRequest{
		AspectID:           in.AspectID,
		AppendSystemPrompt: filterJudgePrompt,
		UserMessage:        buildJudgeUserMessage(in),
		Provider:           f.Provider,
		Model:              f.Model,
		MaxSteps:           1, // pure text; no tools
		Cwd:                f.AspectHome,
		ProviderEnv:        f.ProviderEnv,
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
		if f.Logger != nil {
			f.Logger.Warn("filter judge: harness error — failing open",
				"aspect", in.AspectID, "turn_id", in.TurnID, "err", err)
		}
		// Fail open: posting a thin reply is better than suppressing
		// a real one because the filter timed out or errored.
		return FilterDecision{ShouldPost: true}
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
		// visible in audit logs.
		decision = FilterDecision{ShouldPost: true, Reason: "parse_failure"}
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
