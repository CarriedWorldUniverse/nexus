package broker

import (
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func TestMergeTimelineOrdersByTimeChatBeforeActivityOnTie(t *testing.T) {
	msgs := []chat.Message{
		{ID: 1, From: "shadow", Content: "!dispatch", CreatedAt: time.UnixMilli(100)},
		{ID: 2, From: "anvil", Content: "done", CreatedAt: time.UnixMilli(300)},
	}
	acts := []observability.Frame{
		{Kind: observability.FrameTurn, Sequence: 1, TS: time.UnixMilli(100)},
		{Kind: observability.FrameTurn, Sequence: 2, TS: time.UnixMilli(200)},
	}
	tl := mergeTimeline(msgs, acts)
	if len(tl) != 4 {
		t.Fatalf("len = %d", len(tl))
	}
	if tl[0].Kind != "chat" || tl[1].Kind != "activity" {
		t.Fatalf("tie-break wrong: %+v %+v", tl[0], tl[1])
	}
	if tl[2].At != 200 || tl[3].At != 300 {
		t.Fatalf("order wrong: %+v", tl)
	}
	_ = frames.TimelineItemPayload{}
}

func TestFilterRunTimelineMessagesFreezesCompletedRun(t *testing.T) {
	msgs := []chat.Message{
		{ID: 1, Content: "!dispatch", CreatedAt: time.UnixMilli(100)},
		{ID: 2, Content: "done", CreatedAt: time.UnixMilli(200)},
		{ID: 3, Content: "later unrelated chatter", CreatedAt: time.UnixMilli(300)},
	}

	got := filterRunTimelineMessages(msgs, time.UnixMilli(100), time.UnixMilli(200))
	if len(got) != 2 {
		t.Fatalf("len = %d, got = %+v", len(got), got)
	}
	for _, m := range got {
		if m.ID == 3 {
			t.Fatalf("completed run included later chat: %+v", got)
		}
	}
}
