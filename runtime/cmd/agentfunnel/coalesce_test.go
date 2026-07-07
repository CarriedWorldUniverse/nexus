package main

import "testing"

func TestCoalesceStr(t *testing.T) {
	tests := []struct {
		name string
		vals []string
		want string
	}{
		{
			name: "first non-empty wins",
			vals: []string{"", "  ", "first", "second"},
			want: "first",
		},
		{
			name: "whitespace-only is treated as empty",
			vals: []string{"   ", "\t", "\n", "value"},
			want: "value",
		},
		{
			name: "all-empty returns empty",
			vals: []string{"", "  ", "\t"},
			want: "",
		},
		{
			name: "no args returns empty",
			vals: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coalesceStr(tt.vals...)
			if got != tt.want {
				t.Errorf("coalesceStr(%#v) = %q, want %q", tt.vals, got, tt.want)
			}
		})
	}
}
