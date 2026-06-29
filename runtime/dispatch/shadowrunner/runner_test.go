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
	defer cancel()
	go r.Run(ctx)

	// Poll until the first heartbeat drain lands, rather than checking once
	// after a fixed window. A fixed 40ms sleep + 5ms ticker is racy on coarse
	// timers (Windows CI defaults to ~15ms granularity), where the window can
	// elapse with zero ticks delivered even though the runner is correct.
	// Polling returns as soon as a drain happens (fast in the common case) and
	// only the deadline guards against a genuinely stuck heartbeat.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&drains) == 0 {
		select {
		case <-deadline:
			t.Fatal("heartbeat should have triggered at least one drain within 2s")
		case <-time.After(5 * time.Millisecond):
		}
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
