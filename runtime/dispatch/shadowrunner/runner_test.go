package shadowrunner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunner_HeartbeatDrains(t *testing.T) {
	var drains int32
	r := New(Config{Heartbeat: 5 * time.Millisecond}, func(context.Context) error {
		atomic.AddInt32(&drains, 1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	time.Sleep(40 * time.Millisecond)
	cancel()
	if atomic.LoadInt32(&drains) == 0 {
		t.Fatal("heartbeat should have triggered at least one drain")
	}
}

func TestRunner_NoConcurrentDrains(t *testing.T) {
	var inFlight, maxSeen int32
	r := New(Config{Heartbeat: time.Millisecond}, func(context.Context) error {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond) // slow drain; ticks pile up
		atomic.AddInt32(&inFlight, -1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	if atomic.LoadInt32(&maxSeen) > 1 {
		t.Fatalf("drains overlapped: maxSeen=%d, want 1", maxSeen)
	}
}
