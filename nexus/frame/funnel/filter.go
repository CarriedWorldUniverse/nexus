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
	FilterReasonEmpty       = "empty_output"
	FilterReasonSelfSuppress = "self_suppress" // model said "I don't have anything to add"
	FilterReasonScratch      = "scratch"        // looks like internal thinking, not a reply
	FilterReasonRamble       = "ramble"         // long, no actual content
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
}

// filterJudgeTimeout caps the cheap-model call. A filter that takes
// 2s before posting defeats the point of mid-turn comms; the filter
// has to be fast or fail open.
const filterJudgeTimeout = 1500 * time.Millisecond

// filterJudgePrompt is the system prompt for the cheap-model filter.
// Optimized for "yes/no with no preamble" — the funnel parses the
// response with a substring check, so anything but a clean yes/no
// degrades into the fail-open path.
const filterJudgePrompt = `You are a chat-meaningfulness judge. You will be shown a single message produced by an aspect (an AI agent) at the end of a turn. Your job: decide whether this message contains meaningful content for the group chat, or whether it is scratch/internal-thinking/empty/non-reply content that should be suppressed.

Reply with EXACTLY one token: either "yes" (meaningful, post it) or "no" (scratch, suppress it). No preamble, no punctuation, no explanation. Just one word.

Examples:
- "I'll check the database and report back" → yes
- "Looking at the code now" → yes
- "I don't have anything to add to this thread" → no
- "(internal: should I respond?)" → no
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

	req := bridle.TurnRequest{
		AspectID:     in.AspectID,
		SystemPrompt: filterJudgePrompt,
		Session:      bridle.SessionHandle{ID: "filter-" + in.TurnID},
		UserMessage:  in.FinalText,
		Provider:     f.Provider,
		Model:        f.Model,
		MaxSteps:     1, // pure text; no tools
	}

	result, err := f.Harness.RunTurn(ctx, req, NullRunner{}, collectSink{})
	if err != nil {
		// Fail open: posting a thin reply is better than suppressing
		// a real one because the filter timed out or errored.
		return FilterDecision{ShouldPost: true}
	}

	answer := strings.ToLower(strings.TrimSpace(result.FinalText))
	// Tolerate "no.", "no!", "no\n" by checking prefix on a
	// pre-trimmed string. The prompt asks for a clean yes/no but
	// providers don't always cooperate.
	switch {
	case strings.HasPrefix(answer, "no"):
		return FilterDecision{ShouldPost: false, Reason: FilterReasonScratch}
	case strings.HasPrefix(answer, "yes"):
		return FilterDecision{ShouldPost: true}
	default:
		// Unparseable — fail open.
		return FilterDecision{ShouldPost: true}
	}
}
