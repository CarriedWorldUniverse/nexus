package handqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
)

func TestSubmitRunsExecutor(t *testing.T) {
	var calls atomic.Int32
	q, err := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			calls.Add(1)
			return frames.DispatchResultPayload{
				Aspect:     req.Aspect,
				DispatchID: req.DispatchID,
				Output:     map[string]any{"echo": req.Aspect},
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Shutdown(context.Background())

	ctx := context.Background()
	resp, err := q.Submit(ctx, frames.DispatchPayload{
		Aspect:     "wren",
		DispatchID: "d-1",
		Payload:    map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Aspect != "wren" {
		t.Errorf("Aspect = %q", resp.Aspect)
	}
	if calls.Load() != 1 {
		t.Errorf("executor calls = %d, want 1", calls.Load())
	}
}

func TestConcurrencyCap(t *testing.T) {
	var inFlight atomic.Int32
	var maxSeen atomic.Int32
	start := make(chan struct{})
	q, err := New(Config{
		MaxConcurrent: 3,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			n := inFlight.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m {
					break
				}
				if maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			<-start
			time.Sleep(50 * time.Millisecond)
			inFlight.Add(-1)
			return frames.DispatchResultPayload{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Shutdown(context.Background())

	// Submit 10 jobs in parallel.
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := q.Submit(context.Background(), frames.DispatchPayload{
				Aspect: "x",
			})
			results <- err
		}()
	}

	// Let jobs progress.
	time.Sleep(50 * time.Millisecond)
	close(start)

	for i := 0; i < 10; i++ {
		if err := <-results; err != nil {
			t.Errorf("Submit err: %v", err)
		}
	}

	if maxSeen.Load() > 3 {
		t.Errorf("in-flight max = %d, want ≤ 3 (concurrency cap)", maxSeen.Load())
	}
}

func TestSubmitSurfacesExecutorError(t *testing.T) {
	q, _ := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(context.Context, frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			return frames.DispatchResultPayload{}, errors.New("simulated failure")
		}),
	})
	defer q.Shutdown(context.Background())

	_, err := q.Submit(context.Background(), frames.DispatchPayload{Aspect: "x"})
	if err == nil || err.Error() != "simulated failure" {
		t.Errorf("err = %v, want simulated failure", err)
	}
}

// -------------------------------------------------------------------
// Spec §10 v0.1 invariant tests
// -------------------------------------------------------------------

// TestFairnessReleaseScanPrefersIdleAspect — spec §3 fairness rule
// (release-side): when a worker exits, scan picks an item from an
// aspect with no active workers in preference to FIFO head whose
// aspect is busy.
//
// Setup (N=2, H=3):
//   - A1, A2 spawn (active=2, A busy)
//   - A3 submits → enqueues (A still busy, no spillover)
//   - B1 submits → B is idle → spillover spawn (active=3)
//   - B2 submits → B busy now → enqueues. Queue: [A3, B2]
//   - Release B1. Now B is idle, A still has 2 active.
//   - Scan must prefer B2 (idle aspect) over A3 (FIFO head, A busy).
func TestFairnessReleaseScanPrefersIdleAspect(t *testing.T) {
	type runEvent struct{ aspect, id string }
	runs := make(chan runEvent, 8)
	releaseB1 := make(chan struct{})
	holdAFlights := make(chan struct{})

	q, err := New(Config{
		MaxConcurrent: 2,
		HardCeiling:   3,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			runs <- runEvent{req.Aspect, req.DispatchID}
			switch req.DispatchID {
			case "A1", "A2":
				<-holdAFlights
			case "B1":
				<-releaseB1
			}
			return frames.DispatchResultPayload{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	releasedB1 := false
	defer func() {
		if !releasedB1 {
			close(releaseB1)
		}
		close(holdAFlights)
		_ = q.Shutdown(context.Background())
	}()

	// Phase 1: A1, A2 enter executor (active=2, A busy).
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A1"})
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A2"})
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case ev := <-runs:
			got[ev.id] = true
		case <-time.After(time.Second):
			t.Fatal("A1+A2 didn't both enter executor")
		}
	}

	// Phase 2: A3 enqueues, B1 spillover-spawns, B2 enqueues.
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A3"})
	time.Sleep(20 * time.Millisecond)
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "B", DispatchID: "B1"})
	// Wait for B1 to enter executor.
	select {
	case ev := <-runs:
		if ev.id != "B1" {
			t.Fatalf("expected B1 to spillover-spawn; saw %q", ev.id)
		}
	case <-time.After(time.Second):
		t.Fatal("B1 didn't spillover-spawn")
	}
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "B", DispatchID: "B2"})
	time.Sleep(50 * time.Millisecond)

	// Phase 3: release B1. Fairness scan must pick B2 (B now idle)
	// over A3 (FIFO head; A still has 2 active).
	close(releaseB1)
	releasedB1 = true
	select {
	case ev := <-runs:
		if ev.id != "B2" {
			t.Errorf("after B1 release, ran %q; fairness should pick B2 (B is idle, A still busy)", ev.id)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing ran after B1 released")
	}
}

// TestFairnessFIFOWhenSameAspect — when the queue's items are all
// from the same (busy) aspect, FIFO order is preserved.
//
// Setup: N=2 H=2. A1, A2 spawn (active=2, A busy). A3, A4 enqueue
// (queue: [A3, A4]). Release A1 → scan: queue head A3, no idle
// aspect to prefer → FIFO head A3 spawns.
func TestFairnessFIFOWhenSameAspect(t *testing.T) {
	type runEvent struct{ id string }
	runs := make(chan runEvent, 8)
	releaseA1 := make(chan struct{})
	releaseA2 := make(chan struct{})

	q, _ := New(Config{
		MaxConcurrent: 2,
		HardCeiling:   2,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			runs <- runEvent{req.DispatchID}
			switch req.DispatchID {
			case "A1":
				<-releaseA1
			case "A2":
				<-releaseA2
			}
			return frames.DispatchResultPayload{}, nil
		}),
	})
	defer func() { _ = q.Shutdown(context.Background()) }()

	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A1"})
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A2"})
	for i := 0; i < 2; i++ {
		<-runs
	}
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A3"})
	time.Sleep(20 * time.Millisecond)
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: "A4"})
	time.Sleep(20 * time.Millisecond)

	close(releaseA1)
	select {
	case ev := <-runs:
		if ev.id != "A3" {
			t.Errorf("first dispatched after A1 release = %q, want A3 (FIFO head)", ev.id)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing ran after A1 release")
	}
	close(releaseA2)
	// Drain remaining run events to keep the goroutines from blocking.
	for i := 0; i < 1; i++ {
		select {
		case <-runs:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// TestSpilloverIdleAspect — spec §10.2: with all soft-cap workers busy
// on aspect A, an arrival from aspect B (no active worker) immediately
// spawns a 4th worker (under H).
func TestSpilloverIdleAspect(t *testing.T) {
	gate := make(chan struct{})
	var inFlight atomic.Int32
	var maxSeen atomic.Int32

	q, err := New(Config{
		MaxConcurrent: 3,
		HardCeiling:   5,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			n := inFlight.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			<-gate
			inFlight.Add(-1)
			return frames.DispatchResultPayload{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(gate); _ = q.Shutdown(context.Background()) }()

	// Fill 3 slots with aspect-A work.
	for i := 0; i < 3; i++ {
		go func(i int) {
			_, _ = q.Submit(context.Background(), frames.DispatchPayload{Aspect: "A", DispatchID: fmt.Sprintf("a-%d", i)})
		}(i)
	}
	// Wait until all 3 are in flight.
	for time.Now(); inFlight.Load() < 3; {
		time.Sleep(5 * time.Millisecond)
	}

	// Idle aspect B arrives — must spawn (spillover) immediately
	// rather than enqueue, because B has no active worker.
	go func() {
		_, _ = q.Submit(context.Background(), frames.DispatchPayload{Aspect: "B", DispatchID: "b-0"})
	}()

	// Spinwait briefly for spillover.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if inFlight.Load() >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := inFlight.Load(); got < 4 {
		t.Errorf("spillover did not fire: in-flight = %d, want 4", got)
	}
	if got := maxSeen.Load(); got < 4 {
		t.Errorf("maxSeen = %d, want >= 4 (spillover)", got)
	}
}

// TestHardCeilingRejection — spec §10.3: with N=3 H=5, the 6th
// simultaneous spawn rejects with hard_ceiling.
func TestHardCeilingRejection(t *testing.T) {
	gate := make(chan struct{})
	var spawned atomic.Int32
	q, err := New(Config{
		MaxConcurrent: 3,
		HardCeiling:   5,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			spawned.Add(1)
			<-gate
			return frames.DispatchResultPayload{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(gate); _ = q.Shutdown(context.Background()) }()

	// 5 unique aspects each dispatching once → all should spawn
	// (3 immediate + 2 spillover) up to H=5.
	for i := 0; i < 5; i++ {
		go func(i int) {
			_, _ = q.Submit(context.Background(), frames.DispatchPayload{
				Aspect: fmt.Sprintf("a%d", i), DispatchID: fmt.Sprintf("d-%d", i),
			})
		}(i)
	}
	// Wait for all 5 to be in flight.
	for time.Now(); spawned.Load() < 5; {
		time.Sleep(5 * time.Millisecond)
	}

	// 6th from a fresh aspect — at H, must reject.
	_, err = q.Submit(context.Background(), frames.DispatchPayload{
		Aspect: "a-overflow", DispatchID: "d-6",
	})
	if err == nil {
		t.Fatal("Submit at H expected to reject; got nil err")
	}
	var hc *HardCeilingError
	if !errors.As(err, &hc) {
		t.Fatalf("err = %v, want *HardCeilingError", err)
	}
	if !errors.Is(err, ErrHardCeiling) {
		t.Errorf("errors.Is(err, ErrHardCeiling) false; sentinel matching broken")
	}
	if hc.SoftCap != 3 || hc.Limit != 5 {
		t.Errorf("HardCeilingError = %+v; want SoftCap=3 Limit=5", hc)
	}
}

// TestDispatcherTimeoutDefault — spec §10.8 part 1: a dispatch with no
// deadline_secs runs until the default and is then killed.
func TestDispatcherTimeoutDefault(t *testing.T) {
	q, err := New(Config{
		MaxConcurrent:   1,
		HardCeiling:     2,
		DefaultDeadline: 50 * time.Millisecond,
		MaxDeadline:     200 * time.Millisecond,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			<-ctx.Done()
			return frames.DispatchResultPayload{}, ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Shutdown(context.Background())

	start := time.Now()
	res, err := q.Submit(context.Background(), frames.DispatchPayload{
		Aspect: "x", DispatchID: "d-1", Payload: map[string]any{},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want contains 'timeout'", err)
	}
	if !strings.Contains(res.Error, "timeout") {
		t.Errorf("res.Error = %q, want contains 'timeout'", res.Error)
	}
	if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want ~50ms", elapsed)
	}
}

// TestDispatcherTimeoutOverride — spec §10.8 part 2: deadline_secs in
// payload is honored.
func TestDispatcherTimeoutOverride(t *testing.T) {
	q, _ := New(Config{
		MaxConcurrent:   1,
		HardCeiling:     2,
		DefaultDeadline: 5 * time.Second,
		MaxDeadline:     5 * time.Second,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			<-ctx.Done()
			return frames.DispatchResultPayload{}, ctx.Err()
		}),
	})
	defer q.Shutdown(context.Background())

	start := time.Now()
	// 0.05 seconds = 50ms, well below default of 5s.
	_, err := q.Submit(context.Background(), frames.DispatchPayload{
		Aspect: "x", DispatchID: "d-1",
		Payload: map[string]any{"deadline_secs": 0.05},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout, got nil err")
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, expected ~50ms (override should bound below default)", elapsed)
	}
}

// TestDispatcherTimeoutCap — spec §10.8 part 3: deadline_secs above the
// configured max is silently capped, not errored.
func TestDispatcherTimeoutCap(t *testing.T) {
	q, _ := New(Config{
		MaxConcurrent:   1,
		HardCeiling:     2,
		DefaultDeadline: 30 * time.Second,
		MaxDeadline:     50 * time.Millisecond,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			<-ctx.Done()
			return frames.DispatchResultPayload{}, ctx.Err()
		}),
	})
	defer q.Shutdown(context.Background())

	start := time.Now()
	_, err := q.Submit(context.Background(), frames.DispatchPayload{
		Aspect: "x", DispatchID: "d-1",
		Payload: map[string]any{"deadline_secs": 99999.0},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout, got nil err")
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, want ~50ms (caller asked 99999s, MaxDeadline=50ms should cap silently)", elapsed)
	}
}

// TestStatsExposesPoolState — Stats() snapshot reflects the dispatcher
// state for Frame / dashboard visibility.
func TestStatsExposesPoolState(t *testing.T) {
	gate := make(chan struct{})
	q, _ := New(Config{
		MaxConcurrent: 2,
		HardCeiling:   3,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			<-gate
			return frames.DispatchResultPayload{}, nil
		}),
	})
	defer func() { close(gate); _ = q.Shutdown(context.Background()) }()

	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "wren", DispatchID: "1"})
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "anvil", DispatchID: "2"})
	for time.Now(); ; {
		s := q.Stats()
		if s.ActiveTotal == 2 {
			if s.ActiveByAspect["wren"] != 1 || s.ActiveByAspect["anvil"] != 1 {
				t.Errorf("active by aspect = %v", s.ActiveByAspect)
			}
			if s.SoftCap != 2 || s.HardCeiling != 3 {
				t.Errorf("config snapshot = %+v", s)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubmitRespectsContext(t *testing.T) {
	// Queue capacity 1 + 1 worker; block the worker so Submit's
	// channel send backs up and ctx can time it out.
	block := make(chan struct{})
	q, _ := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			<-block
			return frames.DispatchResultPayload{}, nil
		}),
	})
	defer func() { close(block); q.Shutdown(context.Background()) }()

	// Fill the first worker slot.
	go q.Submit(context.Background(), frames.DispatchPayload{Aspect: "x", DispatchID: "first"})
	time.Sleep(20 * time.Millisecond)

	// Second submit with a tight ctx should time out waiting for result.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := q.Submit(ctx, frames.DispatchPayload{Aspect: "x", DispatchID: "second"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}
