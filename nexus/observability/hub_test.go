package observability

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
)

func TestHub_GrouperFor_ConcurrentReturnsSameInstance(t *testing.T) {
	h := NewHub(50, func(string, Frame) {})

	const goroutines = 32
	results := make([]*Grouper, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = h.GrouperFor("plumb")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different *Grouper instance", i)
		}
	}
}

func TestHub_EmitFansOutToBufferAndCallback(t *testing.T) {
	var callbackCount int32
	var lastAspect string
	var mu sync.Mutex
	h := NewHub(50, func(aspect string, f Frame) {
		atomic.AddInt32(&callbackCount, 1)
		mu.Lock()
		lastAspect = aspect
		mu.Unlock()
	})

	g := h.GrouperFor("plumb")
	g.OnChat(chat.Message{ID: 1, From: "plumb", Content: "hi"}, DirectionOutbound)

	if got := atomic.LoadInt32(&callbackCount); got != 1 {
		t.Fatalf("callback fire count: got %d want 1", got)
	}
	mu.Lock()
	if lastAspect != "plumb" {
		t.Errorf("aspect: got %q want plumb", lastAspect)
	}
	mu.Unlock()

	tail := h.Tail("plumb", 0)
	if len(tail) != 1 {
		t.Fatalf("buffer tail size: got %d want 1", len(tail))
	}
	if tail[0].Kind != FrameChat || tail[0].Aspect != "plumb" {
		t.Errorf("tail frame shape: %+v", tail[0])
	}
}

func TestHub_TailFiltersBySinceSeq(t *testing.T) {
	h := NewHub(50, func(string, Frame) {})
	g := h.GrouperFor("plumb")
	for i := int64(1); i <= 5; i++ {
		g.OnChat(chat.Message{ID: i, From: "plumb", Content: "msg"}, DirectionOutbound)
	}
	all := h.Tail("plumb", 0)
	if len(all) != 5 {
		t.Fatalf("tail size: got %d want 5", len(all))
	}
	since3 := h.Tail("plumb", 3)
	if len(since3) != 2 {
		t.Fatalf("tail since=3 size: got %d want 2", len(since3))
	}
	if since3[0].Sequence != 4 || since3[1].Sequence != 5 {
		t.Errorf("tail since=3 sequences: got %d,%d want 4,5", since3[0].Sequence, since3[1].Sequence)
	}
}

func TestHub_PerAspectIsolation(t *testing.T) {
	h := NewHub(50, func(string, Frame) {})
	gPlumb := h.GrouperFor("plumb")
	gKeel := h.GrouperFor("keel")
	if gPlumb == gKeel {
		t.Fatal("different aspects must yield distinct groupers")
	}
	gPlumb.OnChat(chat.Message{ID: 1, From: "plumb", Content: "p"}, DirectionOutbound)
	gKeel.OnChat(chat.Message{ID: 2, From: "keel", Content: "k"}, DirectionOutbound)

	if t1 := h.Tail("plumb", 0); len(t1) != 1 || t1[0].Aspect != "plumb" {
		t.Errorf("plumb tail wrong: %+v", t1)
	}
	if t2 := h.Tail("keel", 0); len(t2) != 1 || t2[0].Aspect != "keel" {
		t.Errorf("keel tail wrong: %+v", t2)
	}
}
