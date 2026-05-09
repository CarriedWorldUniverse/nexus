package route

import (
	"sync"
	"testing"
)

const frameName = "anchor" // operator-chosen Frame name in tests; non-default to catch hardcoding

func TestMentions(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"none", "no mentions here", nil},
		{"single", "hi @anchor", []string{"anchor"}},
		{"multiple", "@anchor @wren both", []string{"anchor", "wren"}},
		{"dedup", "@anchor and again @anchor", []string{"anchor"}},
		{"alphanumeric", "@anvil_v2 mid-sentence", []string{"anvil_v2"}},
		{"adjacent punctuation", "@anchor, please", []string{"anchor"}},
		{"email-fragment", "user@example.com", []string{"example"}}, // matches but harmless
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Mentions(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("idx %d: got %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIsAddressed(t *testing.T) {
	if IsAddressed("plain content") {
		t.Error("expected un-addressed for plain content")
	}
	if !IsAddressed("hi @anchor") {
		t.Error("expected addressed for @anchor mention")
	}
}

// Rule 1 — un-addressed always routes to the Frame regardless of sender.
func TestShouldRoute_UnAddressedFromAnyone(t *testing.T) {
	cases := []struct {
		from    string
		content string
	}{
		{"operator", "broadcast to everyone"},
		{"wren", "thinking out loud"},
		{"anchor", "self-narrating"},
		{"system", "alarm fired at 02:00"},
	}
	for _, tc := range cases {
		msg := Message{From: tc.from, Content: tc.content}
		if !ShouldRouteToFrame(msg, frameName, NewThreadIndex()) {
			t.Errorf("from=%q content=%q: expected route to frame", tc.from, tc.content)
		}
	}
}

// Rule 2a — Frame is the addressee.
func TestShouldRoute_AddressedToFrame(t *testing.T) {
	msg := Message{From: "operator", Content: "@anchor please ack"}
	if !ShouldRouteToFrame(msg, frameName, NewThreadIndex()) {
		t.Error("expected route to frame when @-mentioned")
	}
}

// Rule 2b — Frame is the sender. Symmetric, no-op delivery.
func TestShouldRoute_FromFrame(t *testing.T) {
	msg := Message{From: frameName, Content: "@wren thanks"}
	if !ShouldRouteToFrame(msg, frameName, NewThreadIndex()) {
		t.Error("expected route to frame when frame is sender")
	}
}

// Rule 2c — addressed but not at the Frame and Frame isn't a participant.
// Should NOT route. This is the discriminating case — addressed traffic
// to other aspects shouldn't reach the Frame unless it's already in the thread.
func TestShouldRoute_AddressedToOtherAspect_NotInThread(t *testing.T) {
	msg := Message{From: "operator", Content: "@wren can you check"}
	if ShouldRouteToFrame(msg, frameName, NewThreadIndex()) {
		t.Error("expected NO route — addressed to wren, frame not in thread")
	}
}

// Rule 2c reply-to-frame: addressed elsewhere but reply chains to the Frame.
func TestShouldRoute_ReplyToFrameAuthoredMessage(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordPost(42, "")
	msg := Message{From: "wren", Content: "@maren take this", ReplyTo: 42}
	if !ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("expected route — replying to a frame-authored message")
	}
}

// Regression: rule 2c (replying to frame-authored) must NOT fire if the
// frame only participated in the thread without authoring the target msg.
func TestShouldRoute_ReplyToParticipatedNotAuthored(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordParticipation(50, "")
	msg := Message{From: "wren", Content: "@maren take this", ReplyTo: 50}
	if ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("rule 2c should NOT fire — frame only participated, didn't author 50")
	}
}

// Rule 2e — frame can match via participation in the thread root, even
// when it didn't author the root.
func TestShouldRoute_ThreadRootParticipated(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordParticipation(50, "")
	msg := Message{From: "wren", Content: "@maren follow-up", ThreadRoot: 50}
	if !ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("rule 2e should fire — frame is in thread 50 (via participation)")
	}
}

// Rule 2d — topic the Frame has participated in.
func TestShouldRoute_TopicMatch(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordPost(0, "harness-naming") // topic-only post
	msg := Message{From: "wren", Content: "@maren one more thought", Topic: "harness-naming"}
	if !ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("expected route — topic the frame participated in")
	}
}

// Rule 2e — thread-root match.
func TestShouldRoute_ThreadRootMatch(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordPost(100, "") // root authored by frame
	msg := Message{From: "wren", Content: "@maren follow-up", ThreadRoot: 100}
	if !ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("expected route — thread root authored by frame")
	}
}

// Pure aspect-to-aspect chatter when the frame has no participation history.
func TestShouldRoute_AspectToAspect_NoFrameMembership(t *testing.T) {
	idx := NewThreadIndex()
	msg := Message{From: "wren", Content: "@maren agreed", ReplyTo: 5, Topic: "voice-cleanup"}
	if ShouldRouteToFrame(msg, frameName, idx) {
		t.Error("expected NO route — aspect-to-aspect, frame not participant")
	}
}

// Nil ThreadIndex behaves as empty index.
func TestShouldRoute_NilIndex(t *testing.T) {
	// Un-addressed still routes (rule 1 doesn't need an index).
	if !ShouldRouteToFrame(Message{From: "operator", Content: "hi"}, frameName, nil) {
		t.Error("nil-idx + un-addressed: expected route")
	}
	// Addressed-to-frame still routes (rule 2a doesn't need an index).
	if !ShouldRouteToFrame(Message{From: "operator", Content: "@anchor hi"}, frameName, nil) {
		t.Error("nil-idx + @-frame: expected route")
	}
	// Addressed-to-other with reply-to: nil-idx means we can't check
	// participation, so should NOT route.
	if ShouldRouteToFrame(Message{From: "operator", Content: "@wren hi", ReplyTo: 1}, frameName, nil) {
		t.Error("nil-idx + addressed-to-other + reply-to: expected NO route (idx unknown)")
	}
}

func TestThreadIndex_RecordAndQuery(t *testing.T) {
	idx := NewThreadIndex()

	if idx.AuthoredMessage(1) {
		t.Error("empty idx: should not have authored msg 1")
	}
	if idx.InThread(1) {
		t.Error("empty idx: should not have InThread 1")
	}
	if idx.ParticipatedInTopic("foo") {
		t.Error("empty idx: should not have participated in foo")
	}

	idx.RecordPost(1, "foo")
	if !idx.AuthoredMessage(1) {
		t.Error("expected authored msg 1 after RecordPost")
	}
	if !idx.InThread(1) {
		t.Error("RecordPost should also mark Frame as InThread for the post id")
	}
	if !idx.ParticipatedInTopic("foo") {
		t.Error("expected participated in foo after RecordPost")
	}
	if idx.AuthoredMessage(2) {
		t.Error("did not record msg 2")
	}

	authored, threads, topics := idx.Stats()
	if authored != 1 || threads != 1 || topics != 1 {
		t.Errorf("Stats = (%d, %d, %d), want (1, 1, 1)", authored, threads, topics)
	}
}

// Authored vs participated must stay distinct so rule 2c doesn't fire
// false positives on threads the Frame merely joined.
func TestThreadIndex_ParticipatedNotAuthored(t *testing.T) {
	idx := NewThreadIndex()
	idx.RecordParticipation(99, "")

	if idx.AuthoredMessage(99) {
		t.Error("RecordParticipation must NOT imply authorship — rule 2c would false-positive")
	}
	if !idx.InThread(99) {
		t.Error("RecordParticipation should set InThread")
	}
}

func TestThreadIndex_ZeroIDsIgnored(t *testing.T) {
	// RecordPost(0, "") should be a no-op — guarding against accidental
	// "track every post regardless of validity" pollution.
	idx := NewThreadIndex()
	idx.RecordPost(0, "")
	idx.RecordParticipation(0, "")
	authored, threads, topics := idx.Stats()
	if authored != 0 || threads != 0 || topics != 0 {
		t.Errorf("Stats after zero records = (%d, %d, %d), want (0, 0, 0)",
			authored, threads, topics)
	}
}

func TestThreadIndex_ConcurrentSafe(t *testing.T) {
	// Many goroutines posting concurrently should not race or corrupt
	// the maps. Run with -race to verify.
	idx := NewThreadIndex()
	var wg sync.WaitGroup
	for i := int64(1); i <= 100; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			idx.RecordPost(id, "concurrent-topic")
			_ = idx.AuthoredMessage(id)
			_ = idx.ParticipatedInTopic("concurrent-topic")
		}(i)
	}
	wg.Wait()

	authored, _, _ := idx.Stats()
	if authored != 100 {
		t.Errorf("authored = %d, want 100", authored)
	}
}

func TestShouldRoute_FrameMentionedAlongsideOthers(t *testing.T) {
	// Multi-mention: @frame + @other. Frame is mentioned, so route.
	msg := Message{From: "operator", Content: "@anchor @wren can you both look"}
	if !ShouldRouteToFrame(msg, frameName, NewThreadIndex()) {
		t.Error("expected route — frame is one of multiple mentions")
	}
}

func TestShouldRoute_ContentEdgeCases(t *testing.T) {
	idx := NewThreadIndex()
	cases := []struct {
		name    string
		content string
		want    bool // true = route to frame
	}{
		{"empty content", "", true},                                // un-addressed, routes
		{"whitespace-only", "   \n  ", true},                       // un-addressed, routes
		{"@-symbol-no-name", "@", true},                            // not a valid mention; un-addressed
		{"frame-name-not-mention", frameName, true},                // bare word, no @
		{"frame-name-as-substring", "anchorman @wren said", false}, // not @anchor, addressed @wren
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldRouteToFrame(Message{From: "wren", Content: tc.content}, frameName, idx)
			if got != tc.want {
				t.Errorf("got %v want %v for %q", got, tc.want, tc.content)
			}
		})
	}
}
