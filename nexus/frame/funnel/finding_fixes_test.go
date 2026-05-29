package funnel

import (
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// NEX-369: bare Claude tiers expand to full Anthropic model ids ONLY on the
// native claude-api path; claude-code keeps its CLI shorthand, non-Claude
// providers and full ids pass through unchanged.
func TestExpandBareClaudeTier(t *testing.T) {
	cases := []struct {
		model string
		pid   bridle.ProviderID
		want  string
	}{
		{"haiku", "claude-api", "claude-haiku-4-5-20251001"},
		{"haiku", "claude", "claude-haiku-4-5-20251001"},
		{"sonnet", "claude-api", "claude-sonnet-4-6"},
		{"opus", "claude-api", "claude-opus-4-8"},
		{"haiku", "claude-code", "haiku"},  // CLI shorthand preserved
		{"haiku", "claudecode", "haiku"},   // alias
		{"haiku", "openai", "haiku"},        // non-Claude: no expansion
		{"claude-haiku-4-5-20251001", "claude-api", "claude-haiku-4-5-20251001"}, // full id passthrough
		{"deepseek-v4-flash", "openai", "deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := ExpandBareClaudeTier(c.model, c.pid); got != c.want {
			t.Errorf("ExpandBareClaudeTier(%q, %q) = %q, want %q", c.model, c.pid, got, c.want)
		}
	}
}

// NEX-370: a turn that posted a chat message via send_chat / announce /
// share counts as "already posted" (suppress FinalText auto-post);
// react_to / chat_read / knowledge / no-tools do not.
func TestPostedChatViaTool(t *testing.T) {
	inv := func(names ...string) []bridle.ToolInvocation {
		out := make([]bridle.ToolInvocation, 0, len(names))
		for _, n := range names {
			out = append(out, bridle.ToolInvocation{Name: n})
		}
		return out
	}
	cases := []struct {
		name  string
		invs  []bridle.ToolInvocation
		want  bool
	}{
		{"send_chat", inv(ToolNameSendChat), true},
		{"announce_file", inv(ToolNameAnnounceFile), true},
		{"share_file", inv(ToolNameShareFile), true},
		{"send_chat among others", inv(ToolNameChatRead, ToolNameSendChat), true},
		{"react only", inv(ToolNameReactTo), false},
		{"chat_read only", inv(ToolNameChatRead), false},
		{"no tools", nil, false},
	}
	for _, c := range cases {
		if got := postedChatViaTool(c.invs); got != c.want {
			t.Errorf("%s: postedChatViaTool = %v, want %v", c.name, got, c.want)
		}
	}
}
