package wsasp

import (
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// recordingTarget captures ReceiveWithMsgID calls so the bridge's
// translation can be asserted without standing up a real funnel.
type recordingTarget struct {
	mu    sync.Mutex
	items []bridle.InboxItem
	ids   []int64
}

func (r *recordingTarget) ReceiveWithMsgID(item bridle.InboxItem, msgID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
	r.ids = append(r.ids, msgID)
}

func TestBridge_OnDeliverTranslatesAndForwards(t *testing.T) {
	target := &recordingTarget{}
	bridge := NewBridge(target)

	bridge.OnDeliver(DeliveredMessage{
		ID:         42,
		From:       "operator",
		Content:    "hello forge",
		ReplyTo:    7,
		ReceivedAt: "2026-05-04T12:00:00Z",
		Reason:     "mention",
	})

	if len(target.items) != 1 {
		t.Fatalf("expected 1 forwarded item, got %d", len(target.items))
	}
	if target.items[0].From != "operator" {
		t.Errorf("From mismatch: %q", target.items[0].From)
	}
	if target.items[0].Content != "hello forge" {
		t.Errorf("Content mismatch: %q", target.items[0].Content)
	}
	if target.ids[0] != 42 {
		t.Errorf("msg_id mismatch: %d", target.ids[0])
	}
}

func TestBridge_ReplayFlagDoesNotCorruptForwarding(t *testing.T) {
	target := &recordingTarget{}
	bridge := NewBridge(target)

	// Replay messages must reach the funnel just like live ones — the
	// flag is informational, not a routing gate.
	bridge.OnDeliver(DeliveredMessage{
		ID:      9242,
		From:    "operator",
		Content: "stale request",
		Replay:  true,
	})

	if len(target.items) != 1 {
		t.Fatalf("replay item dropped — bridge should forward regardless of replay flag")
	}
	if target.ids[0] != 9242 {
		t.Errorf("replay id mismatch")
	}
}

func TestBridge_MultipleDeliveriesPreserveOrder(t *testing.T) {
	target := &recordingTarget{}
	bridge := NewBridge(target)

	for i := int64(1); i <= 5; i++ {
		bridge.OnDeliver(DeliveredMessage{
			ID:      i,
			From:    "x",
			Content: "msg",
		})
	}

	if len(target.ids) != 5 {
		t.Fatalf("got %d items, want 5", len(target.ids))
	}
	for i, got := range target.ids {
		if got != int64(i+1) {
			t.Errorf("order corruption at index %d: got %d", i, got)
		}
	}
}
