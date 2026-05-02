package broker

import (
	"reflect"
	"testing"
)

func TestExtractMentions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "no mentions here", nil},
		{"start", "@anvil please review", []string{"anvil"}},
		{"middle", "hey @forge, can you check?", []string{"forge"}},
		{"multiple", "@anvil and @wren, plus @harrow", []string{"anvil", "wren", "harrow"}},
		{"email-like-not-mention", "send to user@host.com", nil},
		{"email-followed-by-mention", "user@host @forge ping", []string{"forge"}},
		{"trailing-punctuation", "thanks @anvil!", []string{"anvil"}},
		{"hyphenated", "@nexus-frame check this", []string{"nexus-frame"}},
		{"all-broadcast", "@all heads up", []string{"all"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractMentions(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ExtractMentions(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRecipientPolicy_DefaultsToFrameOnTopLevelPost(t *testing.T) {
	// Top-level (no reply_to), no @-mentions: defaults to Frame.
	p := RecipientPolicy{
		FrameName: "frame",
		Aspects:   func() []string { return []string{"anvil", "frame", "wren"} },
	}
	got := p.Compute("operator", "hello", 0)
	want := []string{"frame"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_ParentAuthorPlusMentions(t *testing.T) {
	p := RecipientPolicy{
		Parent:    func(msgID int64) (string, error) { return "anvil", nil },
		Aspects:   func() []string { return []string{"anvil", "wren", "frame"} },
		FrameName: "frame",
	}
	// Reply to a message anvil wrote, mentioning wren too.
	got := p.Compute("operator", "@wren check this thread", 42)
	want := []string{"anvil", "wren"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_ExcludesSender(t *testing.T) {
	p := RecipientPolicy{
		Parent:    func(int64) (string, error) { return "anvil", nil },
		Aspects:   func() []string { return []string{"anvil", "wren"} },
		FrameName: "frame",
	}
	// Anvil replying to themselves with a self-mention should
	// produce no recipients (sender always excluded).
	got := p.Compute("anvil", "@anvil note to self", 42)
	if len(got) != 0 {
		t.Errorf("self-reply with self-mention should yield no recipients: got %v", got)
	}
}

func TestRecipientPolicy_AllBroadcast(t *testing.T) {
	p := RecipientPolicy{
		Aspects: func() []string {
			return []string{"anvil", "forge", "wren"}
		},
		FrameName: "frame",
	}
	got := p.Compute("operator", "@all stand up", 0)
	want := []string{"anvil", "forge", "frame", "wren"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_AllExcludesSender(t *testing.T) {
	p := RecipientPolicy{
		Aspects:   func() []string { return []string{"anvil", "forge"} },
		FrameName: "frame",
	}
	got := p.Compute("anvil", "@all stand up", 0)
	want := []string{"forge", "frame"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_AllOverridesParentAndMentions(t *testing.T) {
	// @all is the broadcast escape hatch. Even if there's a parent
	// and explicit @-mentions, @all means everyone.
	p := RecipientPolicy{
		Parent:    func(int64) (string, error) { return "anvil", nil },
		Aspects:   func() []string { return []string{"anvil", "wren"} },
		FrameName: "frame",
	}
	got := p.Compute("operator", "@anvil @wren — actually @all should see this", 42)
	want := []string{"anvil", "frame", "wren"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_AllCaseInsensitive(t *testing.T) {
	p := RecipientPolicy{
		Aspects: func() []string { return []string{"anvil"} },
	}
	for _, variant := range []string{"@all", "@All", "@ALL"} {
		got := p.Compute("operator", variant+" wake up", 0)
		if len(got) != 1 || got[0] != "anvil" {
			t.Errorf("%s: got %v", variant, got)
		}
	}
}

func TestRecipientPolicy_NoFanoutOnReplyChain(t *testing.T) {
	// Operator replied to anvil; anvil replied back. Operator's
	// reply does NOT auto-broadcast to thread participants — only
	// to anvil (parent author). Wren who participated earlier
	// doesn't see this push (they'd have to chat.read, per Lock 2).
	p := RecipientPolicy{
		Parent:    func(int64) (string, error) { return "anvil", nil },
		Aspects:   func() []string { return []string{"anvil", "wren", "frame"} },
		FrameName: "frame",
	}
	got := p.Compute("operator", "ack thanks", 42) // no @-mentions
	want := []string{"anvil"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_NilParentLookupSafe(t *testing.T) {
	// Some deployments may not have a parent lookup wired (e.g.
	// tests). Reply-into-thread degrades to mentions-only.
	p := RecipientPolicy{
		Aspects:   func() []string { return []string{"anvil"} },
		FrameName: "frame",
	}
	got := p.Compute("operator", "@anvil please", 42)
	want := []string{"anvil"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_DedupesParentInMentions(t *testing.T) {
	// Operator replies to anvil and also @-mentions anvil. Should
	// result in a single delivery to anvil, not duplicates.
	p := RecipientPolicy{
		Parent: func(int64) (string, error) { return "anvil", nil },
	}
	got := p.Compute("operator", "@anvil — and to confirm", 42)
	want := []string{"anvil"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRecipientPolicy_FrameIsValidExplicitMention(t *testing.T) {
	p := RecipientPolicy{
		FrameName: "frame",
	}
	got := p.Compute("operator", "@frame what's the status?", 0)
	want := []string{"frame"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
