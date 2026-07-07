package main

import "testing"

func TestClampInt(t *testing.T) {
	cases := []struct {
		name      string
		v, lo, hi int
		want      int
	}{
		{"below-range", -5, 0, 10, 0},
		{"in-range", 5, 0, 10, 5},
		{"above-range", 15, 0, 10, 10},
		{"lo-equals-hi", 7, 3, 3, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampInt(tc.v, tc.lo, tc.hi)
			if got != tc.want {
				t.Errorf("clampInt(%d, %d, %d) = %d, want %d", tc.v, tc.lo, tc.hi, got, tc.want)
			}
		})
	}
}
