// Package route codifies the Frame's chat-routing rules per §6.5 P8 and
// the #80 lock (docs/2026-05-01-frame-stop-decisions.md).
//
// The rules — Frame receives a message iff one of:
//
//  1. Un-addressed traffic: content contains no @<aspect> mention.
//     Applies regardless of sender. This is the routing-awareness role —
//     the Frame may need to surface, route, or act on un-addressed
//     traffic even when not the originator.
//
//  2. Frame is a participant in addressed traffic:
//     a. content @-mentions the Frame, OR
//     b. message is FROM the Frame (no-op delivery; recorded for symmetry), OR
//     c. ReplyTo refers to a message the Frame previously authored, OR
//     d. Topic matches a topic the Frame has previously posted in, OR
//     e. ThreadRoot matches a message the Frame has previously authored or
//     replied within (best-effort transitive — see ThreadIndex).
//
// What this package is NOT:
//   - A chat bus. The Nexus broker doesn't yet host a chat surface; this
//     package is a pure predicate the future broker (or P6's deliberation
//     loop receiving comms by some other path) can call.
//   - A persistence layer. ThreadIndex is in-memory; reset on Nexus
//     restart. The Frame re-learns its participation set as it posts.
//
// The seam is deliberate: the routing rules are the contract; how the
// chat bus delivers messages to consult the predicate is the broker's
// problem when the broker grows a chat surface.
package route

import (
	"regexp"
	"strings"
	"sync"
)

// Message is the routing-relevant subset of a chat message. Future chat
// surfaces should map their wire shape into this struct rather than
// propagating provider-specific types into the predicate.
type Message struct {
	ID         int64  // unique message id (broker-assigned)
	From       string // sender's aspect id (operator, system, or aspect name)
	Content    string // message text — scanned for @<aspect> mentions
	ReplyTo    int64  // 0 if not a reply; else id of the message being replied to
	Topic      string // empty for top-level threads; else operator-or-aspect-supplied topic name
	ThreadRoot int64  // 0 means this message IS the root; otherwise id of the
	// originating message. Top-level new threads always have ThreadRoot=0.
	// Subsequent messages in a thread carry the originating root id.
	// v1 approximation: rule 2e checks AuthoredMessage(ThreadRoot), so a
	// thread the Frame joined deep (without authoring or being delivered
	// the root) won't match. Acceptable for v1; revisit when the chat bus
	// surfaces a richer reply-graph.
}

// mentionRE is the legacy regex-based mention matcher. Kept for the
// deprecated Mentions/IsAddressed entry points (no longer used by
// routing). Do NOT add new callers; use MentionsForRoster /
// IsAddressedToAny instead, which take an explicit aspect list.
//
// Why deprecate: a regex doesn't know which aspects exist. It happily
// returns "@anyword-with-hyphens" whether or not it's a real aspect, and
// any character not in the class silently truncates the match — that's
// the substrate-grade bug that hid for hours when test-keel wasn't
// receiving messages. Roster-aware matching searches for known aspect
// names with word-boundary checks, so identity drives the parse, not
// the parser's guesses about what an identifier looks like.
var mentionRE = regexp.MustCompile(`@([A-Za-z0-9_-]+)`)

// Mentions is DEPRECATED. Use MentionsForRoster with the live aspect
// list. Retained because tests in other packages still call it; new
// code MUST go through the roster-aware path.
//
// Returns @-tokens the regex finds in content. Validity against the
// real aspect set is the caller's problem — exactly the property
// that caused the test-keel routing bug, so prefer MentionsForRoster.
func Mentions(content string) []string {
	matches := mentionRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := m[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// IsAddressed is DEPRECATED. Use IsAddressedToAny. The regex-based
// "is anything addressed" check is conservative — any @-token returns
// true even when the token isn't a real aspect, which can flip the
// "un-addressed → route to Frame" rule incorrectly.
func IsAddressed(content string) bool {
	return mentionRE.MatchString(content)
}

// MentionsForRoster returns the subset of `roster` that the content
// addresses via `@<name>`. Identity-driven match: we look for each
// known aspect name in the content with word-boundary checks rather
// than parsing arbitrary `@`-tokens and hoping they match.
//
// Match rules:
//   - Case-INsensitive — `@Test-Keel` matches an aspect named test-keel.
//   - Word-boundary — `@test` does NOT match aspect "test-keel"; `@test-keel`
//     does. The match must be followed by end-of-string OR a non-word
//     character (anything not [A-Za-z0-9_-]).
//   - Order-preserving, deduplicated — each aspect appears at most once
//     in the result, in the order they first appear in the roster slice.
//
// Special tokens "@all" and "@operator" are NOT included in the roster
// for routing — those are SPA mention-autocomplete affordances, not
// aspect identities. Handle them in the caller's policy layer if needed.
func MentionsForRoster(content string, roster []string) []string {
	if len(roster) == 0 || content == "" {
		return nil
	}
	lc := strings.ToLower(content)
	seen := make(map[string]struct{}, len(roster))
	out := make([]string, 0, len(roster))
	for _, name := range roster {
		if name == "" {
			continue
		}
		if _, dup := seen[strings.ToLower(name)]; dup {
			continue
		}
		if mentionContains(lc, "@"+strings.ToLower(name)) {
			seen[strings.ToLower(name)] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// IsAddressedToAny reports whether the content explicitly addresses
// any aspect in the roster (or @all). Used by the Frame's routing rule
// 1: when content is NOT addressed to any known aspect, route to the
// Frame as the network's catch-all.
//
// "@all" counts as addressed — broadcast traffic is intentional, not
// un-addressed. Aspects in the roster count by name match. Anything
// else (random `@words`, no @-tokens at all) returns false → routes
// to Frame.
func IsAddressedToAny(content string, roster []string) bool {
	if content == "" {
		return false
	}
	lc := strings.ToLower(content)
	if mentionContains(lc, "@all") {
		return true
	}
	for _, name := range roster {
		if name == "" {
			continue
		}
		if mentionContains(lc, "@"+strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// mentionContains reports whether lcContent contains lcToken at a
// word boundary — the token must either end the string OR be followed
// by a non-name character (anything not [A-Za-z0-9_-]). Without the
// boundary check, "@test" would falsely match "@test-keel" and
// vice-versa. Both inputs are pre-lowercased by the caller.
func mentionContains(lcContent, lcToken string) bool {
	for i := 0; i <= len(lcContent)-len(lcToken); i++ {
		if lcContent[i:i+len(lcToken)] != lcToken {
			continue
		}
		// Token found. Check the trailing boundary.
		end := i + len(lcToken)
		if end == len(lcContent) {
			return true
		}
		next := lcContent[end]
		if !isNameByte(next) {
			return true
		}
		// Token is a prefix of a longer @-id (e.g. "@test" inside
		// "@test-keel"); keep scanning for a proper match later in the
		// string.
	}
	return false
}

// isNameByte reports whether b is in the aspect-name byte class
// [A-Za-z0-9_-]. ASCII-only — aspect names are ASCII per the
// frame.validateName regex.
func isNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	}
	return false
}

// ShouldRouteToFrame applies the §6.5 P8 / #80 routing rules using the
// roster-aware addressing check.
//
// frameName is the operator-chosen Frame identity (from EmbeddedFrame.Name).
// roster is the set of OTHER addressable aspect names — used to decide
// whether the message is "addressed to someone" so rule 1 can fire.
// idx may be nil — a nil index treats the Frame as having no participation
// history, so only rules 1 and 2a–2b can match.
//
// The roster MUST include the Frame's own name plus every other live
// aspect. Without the Frame in the roster, rule 1 ("addressed to nobody")
// would fire for messages addressed only to the Frame.
//
// Pure: does NOT mutate idx. Callers integrating with a chat bus are
// responsible for calling idx.RecordPost(msgID, topic) after the Frame
// posts a message, so subsequent routing decisions see the new
// participation. Likewise idx.RecordParticipation is the caller's call,
// not this function's.
func ShouldRouteToFrame(msg Message, frameName string, roster []string, idx *ThreadIndex) bool {
	// Rule 1: un-addressed traffic always routes to the Frame. "Addressed"
	// means addressing a known aspect or @all — random @-tokens that
	// don't match an aspect aren't real addressing.
	if !IsAddressedToAny(msg.Content, roster) {
		return true
	}

	// Rule 2a: Frame is the addressee. Roster-aware match handles
	// hyphens, case, punctuation correctly.
	if mentionsName(msg.Content, frameName) {
		return true
	}

	// Rule 2b: Frame is the addressor. Delivery is a no-op (the Frame
	// already knows about its own posts) but we record it for downstream
	// consumers that compute "did the Frame see this?".
	if msg.From == frameName {
		return true
	}

	// Rule 2c-2e: participation lookup. Skip if no index was supplied.
	if idx == nil {
		return false
	}

	// Rule 2c: replying to a message the Frame authored.
	if msg.ReplyTo != 0 && idx.AuthoredMessage(msg.ReplyTo) {
		return true
	}

	// Rule 2d: topic match.
	if msg.Topic != "" && idx.ParticipatedInTopic(msg.Topic) {
		return true
	}

	// Rule 2e: thread root match. Routes if the Frame authored or
	// participated in the thread root. v1 approximation per Message.ThreadRoot
	// docs — Frame replies deep in someone else's thread don't match here
	// because we don't track the full reply chain.
	if msg.ThreadRoot != 0 && idx.InThread(msg.ThreadRoot) {
		return true
	}

	return false
}

// mentionsName reports whether content addresses the named aspect via
// `@<name>` with word-boundary checks. Case-insensitive. Convenience
// over MentionsForRoster for the single-name check.
func mentionsName(content, name string) bool {
	if content == "" || name == "" {
		return false
	}
	return mentionContains(strings.ToLower(content), "@"+strings.ToLower(name))
}

// ThreadIndex tracks the Frame's participation footprint: messages it
// has authored, threads it has joined without authoring, and topics it
// has posted in. Updated whenever the Frame posts or the funnel decides
// the Frame engaged with a thread. In-memory only; rebuilt as the Frame
// participates after Nexus restart.
//
// Authored vs participated is kept distinct because rule 2c ("replying
// to a message the Frame authored") has different semantics from
// rule 2e ("Frame is in this thread"). Conflating them would cause
// false positives in 2c when the Frame merely observed a thread root
// without authoring it.
//
// Threadsafe: the chat surface may notify the index from any goroutine.
type ThreadIndex struct {
	mu           sync.RWMutex
	authored     map[int64]struct{} // message ids the Frame authored
	participated map[int64]struct{} // thread root ids the Frame joined (incl. authored)
	topics       map[string]struct{}
}

// NewThreadIndex returns an empty index.
func NewThreadIndex() *ThreadIndex {
	return &ThreadIndex{
		authored:     make(map[int64]struct{}),
		participated: make(map[int64]struct{}),
		topics:       make(map[string]struct{}),
	}
}

// RecordPost is called whenever the Frame posts a message. Records the
// message id under authored (so rule 2c matches replies to it), and the
// topic under topics (so rule 2d matches future messages in this topic).
// If the post is itself a reply (or part of an existing thread), the
// caller should also call RecordParticipation with the thread root so
// rule 2e matches.
func (i *ThreadIndex) RecordPost(msgID int64, topic string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if msgID != 0 {
		i.authored[msgID] = struct{}{}
		// A new top-level message is its own thread root; mark the
		// Frame as a participant in that thread for rule 2e.
		i.participated[msgID] = struct{}{}
	}
	if topic != "" {
		i.topics[topic] = struct{}{}
	}
}

// RecordParticipation is called when the Frame engages with a thread
// without authoring its root — e.g., the Frame replied deep in a thread
// it didn't start, or observed an un-addressed broadcast and decided
// the thread is now in scope. Updates rule 2e's match set without
// implying authorship (which would falsely satisfy rule 2c).
//
// v1 funnel may call this only when the Frame actually posts a reply
// in someone else's thread; observe-only participation is a policy
// call left to the funnel.
func (i *ThreadIndex) RecordParticipation(threadRootID int64, topic string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if threadRootID != 0 {
		i.participated[threadRootID] = struct{}{}
	}
	if topic != "" {
		i.topics[topic] = struct{}{}
	}
}

// AuthoredMessage reports whether the Frame previously authored msgID.
// True only for messages the Frame originated; false for thread roots
// the Frame merely joined.
func (i *ThreadIndex) AuthoredMessage(msgID int64) bool {
	if msgID == 0 {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.authored[msgID]
	return ok
}

// InThread reports whether the Frame is a member of the thread rooted
// at rootID — either authored the root or recorded participation.
// Used by rule 2e.
func (i *ThreadIndex) InThread(rootID int64) bool {
	if rootID == 0 {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.participated[rootID]
	return ok
}

// ParticipatedInTopic reports whether the Frame has previously posted
// in topic.
func (i *ThreadIndex) ParticipatedInTopic(topic string) bool {
	if topic == "" {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.topics[topic]
	return ok
}

// Stats returns small counts for observability. Cheap; safe to call
// from a status endpoint or log line.
func (i *ThreadIndex) Stats() (authored, threads, topics int) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.authored), len(i.participated), len(i.topics)
}
