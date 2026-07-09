package main

import "testing"

func TestDnsPathPart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "lowercase_noop", in: "nexus", want: "nexus"},
		{name: "digits_noop", in: "abc123", want: "abc123"},
		{name: "uppercase_lowercased", in: "Anvil", want: "anvil"},
		{name: "mixed_case_lowercased", in: "Anvil.Agent", want: "anvil-agent"},
		{name: "mixed_case_upper_run_id", in: "RUN_ABC_123", want: "run-abc-123"},
		{name: "dashes_preserved", in: "builder-NEX-1", want: "builder-nex-1"},
		{name: "slashes_collapse", in: "path/to/repo", want: "path-to-repo"},
		{name: "special_chars_collapse_to_single_dash", in: "foo!!bar", want: "foo-bar"},
		{name: "multiple_special_chars_collapse", in: "foo---bar", want: "foo-bar"},
		{name: "leading_special_trimmed", in: "---anvil", want: "anvil"},
		{name: "trailing_special_trimmed", in: "anvil---", want: "anvil"},
		{name: "leading_and_trailing_dashes_trimmed", in: "---anvil---", want: "anvil"},
		{name: "only_special_chars_yields_empty", in: "!!!", want: ""},
		{name: "dots_and_underscores", in: "test.v2.0_beta", want: "test-v2-0-beta"},
		{name: "mixed_special", in: "!!nexus-cw!!", want: "nexus-cw"},
		{name: "unicode_dropped", in: "café", want: "caf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dnsPathPart(tt.in); got != tt.want {
				t.Errorf("dnsPathPart(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}