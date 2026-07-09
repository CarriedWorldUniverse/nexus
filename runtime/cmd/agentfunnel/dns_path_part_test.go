package main

import "testing"

func TestDnsPathPart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "mixed_case_and_special_chars", in: "Anvil.Agent", want: "anvil-agent"},
		{name: "leading_trailing_dashes_trimmed", in: "---foo---", want: "foo"},
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