package main

import (
	"reflect"
	"testing"
)

func TestParseKVList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single", "plumb=wake-on-mention", map[string]string{"plumb": "wake-on-mention"}},
		{
			"multi with spaces",
			" plumb=wake-on-mention, keel = always-on ,anvil=wake-on-mention",
			map[string]string{
				"plumb": "wake-on-mention",
				"keel":  "always-on",
				"anvil": "wake-on-mention",
			},
		},
		{"skips malformed entries", "plumb=ok,bogus,=novalue", map[string]string{"plumb": "ok"}},
		{"all malformed → nil", "bogus,alsobogus", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseKVList(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseKVList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
