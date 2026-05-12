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
}

// FilterDecision is the result of Judge.
type FilterDecision struct {
	// ShouldPost true: post the FinalText to chat. False: suppress.
	ShouldPost bool

	// Reason is a short, machine-readable label for telemetry. For
	// allowed posts, conventionally empty. For suppressions, one of
	// the FilterReason* constants below or a free-form reason from
	// custom filters.
	Reason string
}

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

	// Logger is optional; when set, every Judge() call emits an INFO
	// log with the input preview + judge raw output + decision. Pair
	// the input text against the model's verdict for post-hoc analysis
	// ("why did the filter let X through"). nil = silent.
	Logger *slog.Logger

	// ObservabilityHook, when set, surfaces the judge turn under the
	// "filter-judge" label so dashboards can show what the filter
	// actually saw. Nil leaves the judge invisible to observability.
	// Mirrors funnel.Config.ObservabilityHook — wiring should pass the
	// same Hub.GrouperFor(aspectID) for the aspect being judged.
	ObservabilityHook ObservabilityHook
}

// filterJudgeTimeout is a safety bound for a fully hung judge —
// "model is broken / network gone" backstop, not a "make it snappy"
// budget. The outer runFilter waits without timing out (the judge
// IS the authority on whether to post), so this only fires when
// something pathological is going on. 30s is way past any normal
// cold-start; if we hit it, log the WARN and fall back to posting.
const filterJudgeTimeout = 30 * time.Second

// filterJudgePrompt is the system prompt for the cheap-model filter.
// Optimized for "yes/no with no preamble" — the funnel parses the
// response with a substring check, so anything but a clean yes/no
// degrades into the fail-open path.
const filterJudgePrompt = `You are a chat-meaningfulness judge. You will be shown a single message produced by an aspect (an AI agent) at the end of a turn. Your job: decide whether this message contains meaningful content for the group chat, or whether it is scratch/internal-thinking/empty/non-reply/meta-routing content that should be suppressed.

A message is MEANINGFUL only if it contributes something the recipients would want to read: information, a question, a decision, a status update, a substantive opinion, a tool result they need to see, or an emotional/social acknowledgement that's part of an ongoing conversation they're actively in.

A message is NOT meaningful (and should be suppressed) if it falls into any of these patterns — note that these are CATEGORIES, not exhaustive phrasings. Match the intent, not the words:

  1. Self-suppress / non-reply. The aspect saying it has nothing to add, isn't going to respond, will stay quiet, is observing only. Direct ("I don't have anything to add") and indirect ("Nothing further from me on this") both count.

  2. Meta-routing commentary. The aspect noticing that a message wasn't addressed to it and narrating that observation. "This isn't for me." / "Addressed to @operator, not me. No action required." / "Still addressed to @X, not me. Silence." / "Routing artifact — ignoring." All of these talk ABOUT being silent instead of being silent. ALL suppress.

  3. Internal thinking / scratch. Bracketed thoughts, "(thinking: ...)", "should I respond?", chain-of-reasoning leak.

  4. Empty acknowledgements. "Acknowledged.", "Noted.", "Holding.", "Standing by.", "Copy that." — content-free even when grammatical.

  5. Echo / mirror. A reply that just restates what the previous message said without adding anything ("So you're saying X" where X is verbatim the prior msg).

Reply with EXACTLY one token: either "yes" (meaningful, post it) or "no" (scratch/meta/empty, suppress it). No preamble, no punctuation, no explanation. Just one word.

Examples:
- "I'll check the database and report back" → yes
- "Looking at the code now" → yes
- "Migration completed — 4.2M rows updated, 0 errors" → yes
- "I don't have anything to add to this thread" → no
- "This message is addressed to @operator, not me. No action required." → no
- "Still addressed to @anvil, not me. Silence." → no
- "plumb's message is for the operator — I'll stay out of it." → no
- "(internal: should I respond?)" → no
- "Holding." → no
- "Acknowledged." → no
- "Noted, will let you know if it changes" → yes (commits to a future action)
- empty / whitespace only → no
- "{thinking: this is for someone else}" → no`

// Judge runs the cheap-model judgment. The deadline is enforced by a
// child context so a slow model call doesn't stall the deliberation
// past the filter's budget.
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
		UserMessage:        in.FinalText,
		Provider:           f.Provider,
		Model:              f.Model,
		MaxSteps:           1, // pure text; no tools
	}

	// Phase E: bracket the judge turn under "filter-judge" label.
	// Uses the FilterInput.TurnID so observers can correlate the judge
	// to the main turn it's evaluating. Not deferred — close immediately
	// after RunTurn so the Grouper's terminal frame settles before any
	// downstream caller logic.
	if f.ObservabilityHook != nil {
		f.ObservabilityHook.BeginTurn(in.TurnID+"-judge", "filter-judge", f.Model, string(f.Provider), 0)
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

	answer := strings.ToLower(strings.TrimSpace(result.FinalText))
	// Tolerate "no.", "no!", "no\n" by checking prefix on a
	// pre-trimmed string. The prompt asks for a clean yes/no but
	// providers don't always cooperate.
	var decision FilterDecision
	switch {
	case strings.HasPrefix(answer, "no"):
		decision = FilterDecision{ShouldPost: false, Reason: FilterReasonScratch}
	case strings.HasPrefix(answer, "yes"):
		decision = FilterDecision{ShouldPost: true}
	default:
		// Unparseable — fail open.
		decision = FilterDecision{ShouldPost: true}
	}

	// Always log the judge round-trip so post-hoc analysis can pair
	// the input text against the model's verdict. Without this, "why
	// did the filter let X through" requires reproducing the model
	// call by hand. Truncate the input to keep log lines readable;
	// the FinalText raw answer is bounded by MaxSteps:1 and short by
	// construction so it logs in full.
	if f.Logger != nil {
		preview := in.FinalText
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		f.Logger.Info("filter judge decision",
			"aspect", in.AspectID,
			"turn_id", in.TurnID,
			"should_post", decision.ShouldPost,
			"reason", decision.Reason,
			"judge_raw", result.FinalText,
			"input_preview", preview)
	}
	return decision
}
