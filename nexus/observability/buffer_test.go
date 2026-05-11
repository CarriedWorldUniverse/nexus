package observability

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkFrame(aspect string, seq int64) Frame {
	return Frame{Kind: FrameChat, Aspect: aspect, Sequence: seq, TS: time.Unix(seq, 0)}
}

func TestBufferAppendAndTailFromZero(t *testing.T) {
	b := NewBuffer(5)
	for i := int64(1); i <= 3; i++ {
		b.Append(mkFrame("a", i))
	}
	got := b.Tail("a", 0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for i, f := range got {
		if f.Sequence != int64(i+1) {
			t.Errorf("got[%d].seq=%d want %d", i, f.Sequence, i+1)
		}
	}
}

func TestBufferTailSinceMid(t *testing.T) {
	b := NewBuffer(10)
	for i := int64(1); i <= 5; i++ {
		b.Append(mkFrame("a", i))
	}
	got := b.Tail("a", 3)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Sequence != 4 || got[1].Sequence != 5 {
		t.Errorf("got seqs %d,%d want 4,5", got[0].Sequence, got[1].Sequence)
	}
}

func TestBufferEvictionWhenCapExceeded(t *testing.T) {
	b := NewBuffer(3)
	for i := int64(1); i <= 6; i++ {
		b.Append(mkFrame("a", i))
	}
	got := b.Tail("a", 0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Sequence != 4 || got[1].Sequence != 5 || got[2].Sequence != 6 {
		t.Errorf("got seqs %d,%d,%d want 4,5,6", got[0].Sequence, got[1].Sequence, got[2].Sequence)
	}
}

func TestBufferMultiAspectIsolation(t *testing.T) {
	b := NewBuffer(5)
	b.Append(mkFrame("a", 1))
	b.Append(mkFrame("b", 1))
	b.Append(mkFrame("a", 2))
	if got := b.Tail("a", 0); len(got) != 2 {
		t.Errorf("a tail=%d want 2", len(got))
	}
	if got := b.Tail("b", 0); len(got) != 1 {
		t.Errorf("b tail=%d want 1", len(got))
	}
	if got := b.Tail("missing", 0); got != nil {
		t.Errorf("missing tail=%v want nil", got)
	}
}

func TestBufferDrop(t *testing.T) {
	b := NewBuffer(5)
	b.Append(mkFrame("a", 1))
	b.Drop("a")
	if got := b.Tail("a", 0); got != nil {
		t.Errorf("post-Drop tail=%v want nil", got)
	}
	// Re-append after Drop should work fresh.
	b.Append(mkFrame("a", 10))
	if got := b.Tail("a", 0); len(got) != 1 || got[0].Sequence != 10 {
		t.Errorf("post-Drop re-append got=%v", got)
	}
}

func TestBufferConcurrentAppendTail(t *testing.T) {
	b := NewBuffer(64)
	var wg sync.WaitGroup
	var appended int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				seq := atomic.AddInt64(&appended, 1)
				b.Append(mkFrame("x", seq))
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = b.Tail("x", 0)
			}
		}()
	}
	wg.Wait()
	// Final tail: cap=64, so at most 64 frames retained.
	got := b.Tail("x", 0)
	if len(got) > 64 {
		t.Errorf("len=%d > cap=64", len(got))
	}
}

func TestBufferZeroCapPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on cap<=0")
		}
	}()
	NewBuffer(0)
}
