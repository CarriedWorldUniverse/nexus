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
//        replied within (best-effort transitive — see ThreadIndex).
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

// mentionRE matches @<word> tokens. Aspect ids are alphanumeric +
// underscore (frame.validateName regex). Email-style fragments are not
// expected in chat messages; if they appear, they'll match the regex
// but won't equal any registered aspect name and will fall through.
var mentionRE = regexp.MustCompile(`@([A-Za-z0-9_]+)`)

// Mentions returns the set of aspect ids @-mentioned in the content.
// Useful for both routing decisions and observability.
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

// IsAddressed reports whether the content contains at least one
// @<aspect> mention. The inverse — un-addressed traffic — flows to the
// Frame regardless of sender per the routing-awareness rule.
func IsAddressed(content string) bool {
	return mentionRE.MatchString(content)
}

// ShouldRouteToFrame applies the §6.5 P8 / #80 routing rules.
//
// frameName is the operator-chosen Frame identity (from EmbeddedFrame.Name).
// idx may be nil — a nil index treats the Frame as having no participation
// history, so only rules 1 and 2a–2b can match.
//
// Pure: does NOT mutate idx. Callers integrating with a chat bus are
// responsible for calling idx.RecordPost(msgID, topic) after the Frame
// posts a message, so subsequent routing decisions see the new
// participation. Likewise idx.RecordParticipation is the caller's call,
// not this function's.
func ShouldRouteToFrame(msg Message, frameName string, idx *ThreadIndex) bool {
	// Rule 1: un-addressed traffic always routes to the Frame.
	if !IsAddressed(msg.Content) {
		return true
	}

	// Rule 2a: Frame is the addressee.
	for _, m := range Mentions(msg.Content) {
		if m == frameName {
			return true
		}
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
