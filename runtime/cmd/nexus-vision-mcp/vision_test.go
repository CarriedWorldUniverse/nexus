package main

import (
	"math"
	"testing"
)

func TestParseVisionContent(t *testing.T) {
	ok := `{"choices":[{"message":{"content":" a green terrain with a river "}}]}`
	got, err := parseVisionContent([]byte(ok))
	if err != nil || got != "a green terrain with a river" {
		t.Fatalf("got %q err %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"choices":[]}`, `{"choices":[{"message":{"content":"  "}}]}`, `not json`} {
		if _, err := parseVisionContent([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestFrameTimestamps(t *testing.T) {
	// 3 frames over a 12s clip → strictly inside, evenly spaced (3,6,9)
	got := frameTimestamps(12, 3)
	want := []float64{3, 6, 9}
	if len(got) != 3 {
		t.Fatalf("want 3 timestamps, got %v", got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("ts[%d]=%v want %v", i, got[i], want[i])
		}
	}
	// all strictly inside (0, duration)
	for _, ts := range frameTimestamps(10, 6) {
		if ts <= 0 || ts >= 10 {
			t.Fatalf("timestamp %v not strictly inside clip", ts)
		}
	}
	// n<1 coerced to 1
	if len(frameTimestamps(10, 0)) != 1 {
		t.Fatalf("n<1 should yield 1 timestamp")
	}
}

func TestClampFrames(t *testing.T) {
	cases := map[int]int{-1: defaultVideoFrames, 0: defaultVideoFrames, 1: 1, 6: 6, 8: 8, 20: maxVideoFrames}
	for in, want := range cases {
		if got := clampFrames(in); got != want {
			t.Errorf("clampFrames(%d)=%d want %d", in, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Errorf("got %q want x", got)
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Errorf("got %q want empty", got)
	}
}
