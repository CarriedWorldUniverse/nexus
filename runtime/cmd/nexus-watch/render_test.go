package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func fixedTS(t *testing.T) time.Time {
	t.Helper()
	// Fixed UTC instant so the HH:MM column is stable across hosts.
	// Tests pass time.UTC to renderChatFrame for deterministic output
	// without mutating the time.Local global.
	return time.Date(2026, 5, 12, 9, 14, 0, 0, time.UTC)
}

func TestRenderChatFrameInbound(t *testing.T) {
	var buf bytes.Buffer
	cf := observability.ChatFrame{
		MsgID:     192,
		From:      "operator",
		Content:   "@plumb can you fix it?",
		Direction: observability.DirectionInbound,
		CreatedAt: fixedTS(t),
	}
	renderChatFrame(&buf, "plumb", cf, false, time.UTC)
	got := buf.String()
	want := "< #192 [@operator 09:14] @plumb can you fix it?\n"
	if got != want {
		t.Errorf("inbound render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderChatFrameOutbound(t *testing.T) {
	var buf bytes.Buffer
	cf := observability.ChatFrame{
		MsgID:     193,
		From:      "plumb",
		Content:   "done — pushed as abc123",
		Direction: observability.DirectionOutbound,
		CreatedAt: fixedTS(t),
	}
	renderChatFrame(&buf, "plumb", cf, false, time.UTC)
	got := buf.String()
	want := "→ #193 [@plumb 09:14] done — pushed as abc123\n"
	if got != want {
		t.Errorf("outbound render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderChatFrameReplyTo(t *testing.T) {
	var buf bytes.Buffer
	cf := observability.ChatFrame{
		MsgID:     193,
		From:      "plumb",
		Content:   "looking",
		Direction: observability.DirectionOutbound,
		CreatedAt: fixedTS(t),
		ReplyTo:   192,
	}
	renderChatFrame(&buf, "plumb", cf, false, time.UTC)
	got := buf.String()
	if !strings.Contains(got, "→ #193 [@plumb 09:14] looking\n") {
		t.Errorf("missing main line: %q", got)
	}
	if !strings.Contains(got, "↳ reply to #192") {
		t.Errorf("missing reply-to indicator: %q", got)
	}
}

func TestRenderChatFrameNoReplyToWhenZero(t *testing.T) {
	var buf bytes.Buffer
	cf := observability.ChatFrame{
		MsgID:     193,
		From:      "plumb",
		Content:   "hi",
		Direction: observability.DirectionOutbound,
		CreatedAt: fixedTS(t),
		ReplyTo:   0,
	}
	renderChatFrame(&buf, "plumb", cf, false, time.UTC)
	if strings.Contains(buf.String(), "reply to") {
		t.Errorf("unexpected reply-to indicator when ReplyTo==0: %q", buf.String())
	}
}

func TestRenderPresenceFrame(t *testing.T) {
	var buf bytes.Buffer
	pf := observability.PresenceFrame{Connected: true, Reason: "registered"}
	renderPresenceFrame(&buf, "plumb", pf, false)
	got := buf.String()
	want := "─ presence: @plumb connected (registered)\n"
	if got != want {
		t.Errorf("presence render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderPresenceFrameDisconnected(t *testing.T) {
	var buf bytes.Buffer
	pf := observability.PresenceFrame{Connected: false}
	renderPresenceFrame(&buf, "plumb", pf, false)
	got := buf.String()
	want := "─ presence: @plumb disconnected\n"
	if got != want {
		t.Errorf("presence render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestNoColorStripsANSI(t *testing.T) {
	cf := observability.ChatFrame{
		MsgID:     1,
		From:      "anvil",
		Content:   "hi",
		Direction: observability.DirectionInbound,
		CreatedAt: fixedTS(t),
	}
	var withColor, without bytes.Buffer
	renderChatFrame(&withColor, "plumb", cf, true, time.UTC)
	renderChatFrame(&without, "plumb", cf, false, time.UTC)
	if !strings.Contains(withColor.String(), "\x1b[") {
		t.Errorf("expected ANSI when color=true; got %q", withColor.String())
	}
	if strings.Contains(without.String(), "\x1b[") {
		t.Errorf("unexpected ANSI when color=false; got %q", without.String())
	}
	if stripANSI(withColor.String()) != without.String() {
		t.Errorf("stripped colored output should match plain\n stripped: %q\n   plain: %q",
			stripANSI(withColor.String()), without.String())
	}
}

func TestColorizeFromOperatorIsCyanBold(t *testing.T) {
	got := colorizeFrom("operator", true)
	if !strings.Contains(got, ansiCyan) || !strings.Contains(got, ansiBold) {
		t.Errorf("operator should be cyan+bold; got %q", got)
	}
}

func TestColorizeFromStableHash(t *testing.T) {
	a := colorizeFrom("plumb", true)
	b := colorizeFrom("plumb", true)
	if a != b {
		t.Errorf("colourisation must be stable for same name: %q vs %q", a, b)
	}
	c := colorizeFrom("anvil", true)
	if a == c {
		// extremely unlikely collision; if it happens for these names,
		// the test is just signalling our palette is too small.
		t.Errorf("expected different colours for different aspects; got both %q", a)
	}
}
