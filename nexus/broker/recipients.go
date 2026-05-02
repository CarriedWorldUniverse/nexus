// Lock 2 recipient routing — server-side computation of who gets a
// chat.deliver frame for an inbound chat.send.
//
// Rules per the architecture (operator #9085, #9147):
//
//   - Default: parent message's author + any explicit @-mentions in
//     the body. Reply-into-broadcast does NOT auto-fan-out.
//   - @all: explicit broadcast to every registered aspect. Frame
//     itself is included.
//   - Frame is the host: it always sees its own deliberation surface,
//     so a frame-addressed reply still reaches Frame even though
//     Frame doesn't have a WS connection of its own.
//
// The broker calls Compute() on every chat.send before fanning out;
// Frame in-process consumption stays through the existing
// ChatRouter.RouteChat path.

package broker

import (
	"regexp"
	"strings"
)

// mentionRE matches @<word> tokens. Word chars + dash. The leading
// @ is part of the match; group 1 is the bare name. Greedy enough
// to catch "@anvil" but not @-in-middle-of-token like "user@host".
var mentionRE = regexp.MustCompile(`(?:^|\s|[.,;!?])@([a-zA-Z0-9_-]+)`)

// MentionAll is the canonical broadcast token. Case-insensitive
// match; renders as "all" in any case.
const MentionAll = "all"

// ParentLookup resolves a reply_to msg_id to the original sender's
// aspect id. The broker supplies an implementation backed by the
// chat store; a stub returning ("", nil) is acceptable when the
// parent is unknown (the recipient set will fall back to mentions
// only, matching "no parent author" semantics).
type ParentLookup func(msgID int64) (string, error)

// AspectLookup returns the registered aspect names. Used for @all
// expansion. Frame is included if it's part of the registered set —
// callers that have a separate frame name should add it explicitly.
type AspectLookup func() []string

// RecipientPolicy captures the lookup callbacks. Held by the broker
// once at construction; passed to Compute on each chat.send.
type RecipientPolicy struct {
	Parent  ParentLookup
	Aspects AspectLookup

	// FrameName is the embedded Frame's aspect name, if any. Frame
	// is always included when @all fires; it's also the implicit
	// recipient of a @-less reply if the parent author is empty
	// (no parent to address — Frame is the default operator partner).
	FrameName string
}

// Compute returns the set of aspect names that should receive a
// chat.deliver frame for the given inbound chat. The set is
// deduplicated and stable-ordered (alphabetical) so callers can
// log/debug deterministically.
//
// The sender is excluded — sending to yourself is a no-op even if
// the @-mention syntactically includes you.
func (p RecipientPolicy) Compute(sender, content string, replyTo int64) []string {
	mentions := ExtractMentions(content)

	// @all overrides everything — broadcast to every registered
	// aspect (plus Frame if not in the registered list).
	if hasAll(mentions) {
		return p.expandAll(sender)
	}

	set := map[string]struct{}{}

	// Parent author is included unless it's the sender (replying to
	// yourself doesn't generate a notification to yourself).
	if replyTo > 0 && p.Parent != nil {
		if author, _ := p.Parent(replyTo); author != "" && author != sender {
			set[author] = struct{}{}
		}
	}

	// Explicit @-mentions add named aspects.
	for _, m := range mentions {
		if m != "" && m != sender {
			set[m] = struct{}{}
		}
	}

	// If no recipients computed AND there's no parent (top-level
	// post with no @-mentions and no reply target), default to
	// Frame — it's the operator's partner aspect and the natural
	// fallback when nobody else is named.
	if len(set) == 0 && replyTo == 0 && p.FrameName != "" && p.FrameName != sender {
		set[p.FrameName] = struct{}{}
	}

	return sortedKeys(set)
}

// ExtractMentions pulls @-mentions out of message content. Exposed
// (capitalised) so tests and telemetry can use the same parser the
// router uses; drift between two regexes would create silent routing
// bugs.
func ExtractMentions(content string) []string {
	if !strings.Contains(content, "@") {
		return nil
	}
	matches := mentionRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 && m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// hasAll returns true if any mention is the canonical broadcast
// token. Case-insensitive — "@All" / "@ALL" both broadcast.
func hasAll(mentions []string) bool {
	for _, m := range mentions {
		if strings.EqualFold(m, MentionAll) {
			return true
		}
	}
	return false
}

// expandAll returns every registered aspect plus Frame, minus the
// sender. Sorted for stable iteration.
func (p RecipientPolicy) expandAll(sender string) []string {
	set := map[string]struct{}{}
	if p.Aspects != nil {
		for _, name := range p.Aspects() {
			if name != sender {
				set[name] = struct{}{}
			}
		}
	}
	if p.FrameName != "" && p.FrameName != sender {
		set[p.FrameName] = struct{}{}
	}
	return sortedKeys(set)
}

// sortedKeys returns the map's keys in alphabetical order. Called
// by Compute and expandAll; tiny but deduplicates the sort dance
// at three call sites (well, two; the third is in tests).
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Selection sort is fine — recipient sets are typically <10
	// elements; importing sort for tiny payloads adds nothing.
	for i := 0; i < len(out); i++ {
		min := i
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[min] {
				min = j
			}
		}
		out[i], out[min] = out[min], out[i]
	}
	return out
}
