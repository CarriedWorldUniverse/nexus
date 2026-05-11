package main

import (
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantCmd  string
		wantRest string
	}{
		{"/quit", "/quit", ""},
		{"/switch plumb", "/switch", "plumb"},
		{"/history 20", "/history", "20"},
		{"  /say  hello world  ", "/say", " hello world"},
		{"/help", "/help", ""},
	}
	for _, c := range cases {
		gotCmd, gotRest := splitCommand(c.in)
		if gotCmd != c.wantCmd || gotRest != c.wantRest {
			t.Errorf("splitCommand(%q) = (%q, %q); want (%q, %q)",
				c.in, gotCmd, gotRest, c.wantCmd, c.wantRest)
		}
	}
}

func TestParsePosInt(t *testing.T) {
	if v, err := parsePosInt("42"); err != nil || v != 42 {
		t.Errorf("parsePosInt(\"42\") = (%d, %v); want (42, nil)", v, err)
	}
	if _, err := parsePosInt(""); err == nil {
		t.Errorf("parsePosInt(\"\") should error")
	}
	if _, err := parsePosInt("12x"); err == nil {
		t.Errorf("parsePosInt(\"12x\") should error")
	}
}

func TestWatchStateLastSeenOnlyInbound(t *testing.T) {
	s := &watchState{currentAspect: "plumb"}

	// Outbound msg doesn't bump last-seen.
	s.observeChat("plumb", observability.ChatFrame{
		MsgID: 100, Direction: observability.DirectionOutbound,
	})
	if got := s.lastSeen("plumb"); got != 0 {
		t.Errorf("outbound should not bump lastSeen; got %d", got)
	}

	// Inbound msg sets last-seen.
	s.observeChat("plumb", observability.ChatFrame{
		MsgID: 200, Direction: observability.DirectionInbound,
	})
	if got := s.lastSeen("plumb"); got != 200 {
		t.Errorf("inbound should set lastSeen; got %d", got)
	}

	// Older inbound does not lower it.
	s.observeChat("plumb", observability.ChatFrame{
		MsgID: 150, Direction: observability.DirectionInbound,
	})
	if got := s.lastSeen("plumb"); got != 200 {
		t.Errorf("older inbound should not lower lastSeen; got %d", got)
	}

	// /new clears.
	s.clearLastSeen("plumb")
	if got := s.lastSeen("plumb"); got != 0 {
		t.Errorf("clearLastSeen should reset to 0; got %d", got)
	}
}

func TestToWSURL(t *testing.T) {
	cases := map[string]string{
		"https://host:7888":          "wss://host:7888/connect",
		"http://host:7888":           "ws://host:7888/connect",
		"wss://host:7888/connect":    "wss://host:7888/connect",
		"wss://host:7888/connect/":   "wss://host:7888/connect",
		"wss://host:7888":            "wss://host:7888/connect",
		"https://host:7888/":         "wss://host:7888/connect",
	}
	for in, want := range cases {
		if got := toWSURL(in); got != want {
			t.Errorf("toWSURL(%q) = %q; want %q", in, got, want)
		}
	}
}
