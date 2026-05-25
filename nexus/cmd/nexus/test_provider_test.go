package main

import (
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// NEX-297 L1: buildTestProvider dispatches on --provider name and
// returns the correct provider + canonical ID. Empty / unknown names
// must fail loudly so the operator sees the typo, not a silent
// fallback to the wrong provider.
func TestBuildTestProvider_DispatchByName(t *testing.T) {
	cases := []struct {
		name   string
		wantID bridle.ProviderID
		wantOK bool
	}{
		{"claude-api", bridle.ProviderClaude, true},
		{"claude", bridle.ProviderClaude, true},
		{"anthropic", bridle.ProviderClaude, true},
		{"CLAUDE-API", bridle.ProviderClaude, true}, // case-insensitive
		{"  claude  ", bridle.ProviderClaude, true}, // trims whitespace
		{"openai", bridle.ProviderOpenAI, true},
		{"OPENAI", bridle.ProviderOpenAI, true},
		{"", "", false},
		{"claude-code", "", false}, // CLI-subprocess flavor isn't in scope here
		{"deepseek", "", false},    // future native provider, not yet supported
		{"banana", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, id, err := buildTestProvider(c.name, "test-key", "")
			if c.wantOK {
				if err != nil {
					t.Fatalf("expected ok for %q; got err %v", c.name, err)
				}
				if p == nil {
					t.Errorf("provider is nil for %q", c.name)
				}
				if id != c.wantID {
					t.Errorf("id = %q, want %q for %q", id, c.wantID, c.name)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error for unsupported %q; got id=%q", c.name, id)
			}
		})
	}
}

// NEX-297 L1: buildTestProvider must surface the base URL to the
// underlying provider. We can't directly assert on the SDK option
// chain from here, but the bridle provider tests pin
// NewWithBaseURL's behaviour (#35). What this test pins is that the
// dispatcher passes baseURL through to the chosen constructor —
// missing this wire-up would silently route DeepSeek calls to
// api.anthropic.com.
func TestBuildTestProvider_BaseURLAcceptedByBothFlavors(t *testing.T) {
	// Just confirm construction doesn't error out when a base URL is
	// supplied. Routing semantics are the provider's responsibility,
	// covered by the bridle-side tests in NEX-295.
	for _, name := range []string{"claude-api", "openai"} {
		p, _, err := buildTestProvider(name, "k", "https://example.com")
		if err != nil {
			t.Errorf("%s with baseURL should not error; got %v", name, err)
		}
		if p == nil {
			t.Errorf("%s with baseURL returned nil provider", name)
		}
	}
}

// NEX-297 L1: oneline collapses whitespace + truncates long strings
// so the structured report fits one terminal line. The model's full
// output is what matters for "did this work"; if the operator wants
// full text they re-run with --stream.
func TestOneline(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "(empty)"},
		{"   \n\t ", "(empty)"},
		{"hello", "hello"},
		{"line1\nline2", "line1 line2"},
		{"a\r\nb", "a b"},
		{"a   b   c", "a b c"},
		{"  trimmed  ", "trimmed"},
	}
	for _, c := range cases {
		if got := oneline(c.in); got != c.want {
			t.Errorf("oneline(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOneline_TruncatesLongStrings(t *testing.T) {
	in := strings.Repeat("a", 250)
	got := oneline(in)
	if len(got) > 200 {
		t.Errorf("oneline output exceeds 200 chars: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output should end in ellipsis; got %q", got[len(got)-10:])
	}
}
